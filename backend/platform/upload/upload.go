// Package upload 把图片写入对象存储，并给出可直接渲染的绝对 URL。
//
// 除 oss.go 之外，这个包不 import 任何对象存储 SDK：配置校验、内容嗅探、
// 对象键推导与 URL 构造都是纯逻辑，可以完全离线测试。这条边界也决定了
// 依赖落在哪里——backend 是被各服务 replace 的模块，只有真正 import 这个包的
// 服务，go.sum 才会跟着变。
//
// 它不认识 HTTP：处理器负责读 multipart、限流与状态码，这里只回答
// 「这份字节能不能收」和「写到哪个键上」。
package upload

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
)

const (
	// DefaultPrefix 是 UPLOAD_PATH 为空时的对象键前缀，用来把本站的对象
	// 与旧站（panda_serve）的对象隔开。
	DefaultPrefix = "panda-v2"

	// MaxFileSizeCeiling 是 UPLOAD_MAX_FILE_SIZE 的上限。
	// 这不是个审美数字：网关给上传的预算是 120 秒，覆盖客户端上行与 OSS 写入，
	// 10MB 在 1Mbps 上行下就要 80 秒。允许配到 1GB 只会稳定产出 504，
	// 所以在这里直接拒掉，而不是等运维自己发现。
	MaxFileSizeCeiling = 64 << 20

	// imagesSegment 是图片对象键里的固定段，与旧站布局一致。
	imagesSegment = "images"

	// sniffLength 是 http.DetectContentType 判定所需的字节数。
	sniffLength = 512
)

// Config 是一次上传所需的全部配置。它不读环境变量，由 platform/config 读出后
// 组装，因此可以脱离进程环境测试，也让「配置里有哪些键」只有一处定义。
type Config struct {
	AccessKey string // OSS_ACCESS_KEY
	SecretKey string // OSS_SECRET_KEY
	Endpoint  string // OSS_ENDPOINT，裸 endpoint，不带 bucket
	Bucket    string // OSS_BUCKET
	// CNAME 是绑在 bucket 上的自定义域名（OSS_CNAME），留空则从 Endpoint 推。
	CNAME string
	// Prefix 是对象键前缀（UPLOAD_PATH），留空用 DefaultPrefix。
	Prefix string
	// MaxFileSize 是单个文件的字节上限（UPLOAD_MAX_FILE_SIZE）。
	MaxFileSize int64
	// UseMD5 对应旧后端的 UPLOAD_USE_MD5。设为 false 只会产生一条警告：
	// 非 md5 的对象键要把客户端文件名拼进 key，是路径穿越的来源，不再支持。
	UseMD5 bool
}

// Result 是一次成功上传的结果。url 是绝对地址，前端可以直接塞进 <img>；
// key 不含 host，将来换 CDN 或删除对象都要靠它。
type Result struct {
	URL         string `json:"url"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	MD5         string `json:"md5"`
}

// Store 是对象存储的最小接口。把 I/O 隔在这里，Uploader 的其余部分就能用
// 一个假实现离线测。size 是 r 的精确字节数，便于需要预先声明长度的后端。
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
}

// ErrEmptyFile 表示收到的文件是空的。
var ErrEmptyFile = errors.New("上传的文件为空")

// SizeError 表示文件超过上限。处理器据此返回 413，而不是笼统的 400。
type SizeError struct {
	Limit  int64
	Actual int64
}

func (e *SizeError) Error() string {
	return fmt.Sprintf("图片不能超过 %s（当前 %s）", HumanBytes(e.Limit), HumanBytes(e.Actual))
}

// TypeError 表示内容或文件名不是受支持的图片格式。
// 与 Config.Validate 的错误不同：那些是给运维看的英文启动期报错，
// 这条会原样出现在用户界面上。
type TypeError struct {
	Name   string
	Reason string
}

func (e *TypeError) Error() string {
	if strings.TrimSpace(e.Name) == "" {
		return e.Reason
	}
	return fmt.Sprintf("%s：%s", displayName(e.Name), e.Reason)
}

// displayName 取文件名的最后一段。path.Base 只认 /，所以反斜杠也要先当分隔符
// 处理：这个名字会原样回显到界面上，不该带任何路径成分。
func displayName(name string) string {
	trimmed := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	base := path.Base(trimmed)
	if base == "." || base == "/" {
		return ""
	}
	return base
}

// imageType 是嗅探结果到存储写法的映射。
//
// 扩展名由内容决定而不是由客户端文件名决定，因此对象键的扩展名与对象上
// 声明的 Content-Type 永远一致，同一份字节也永远得到同一个键——这正是
// 「同一张图重复上传不占额外存储、重试幂等」的前提。
type imageType struct {
	contentType string
	extension   string
}

var imageTypes = map[string]imageType{
	"image/jpeg": {contentType: "image/jpeg", extension: ".jpg"},
	"image/png":  {contentType: "image/png", extension: ".png"},
	"image/gif":  {contentType: "image/gif", extension: ".gif"},
	"image/webp": {contentType: "image/webp", extension: ".webp"},
	"image/bmp":  {contentType: "image/bmp", extension: ".bmp"},
}

// allowedExtensionList 是客户端文件名的白名单，顺序固定，用于报错文案。
// 用切片而不是 map，是为了让文案里的格式顺序稳定、可断言。
var allowedExtensionList = []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp"}

// Uploader 是上传的入口。
type Uploader struct {
	store      Store
	prefix     string
	maxSize    int64
	publicBase string
}

// New 用配置组装一个可用的 Uploader：建连接、推导公开基址。
//
// 配置不自洽时返回错误。调用方**不应**因此让服务起不来：缺 OSS 凭据是自洽的
// 状态，正确做法是记一条日志、把上传接口置为未配置（503），其余接口照常。
func New(cfg Config) (*Uploader, error) {
	// 先校验配置，再碰 SDK——顺序不能反。SDK 自己也会校验（bucket 名长度、endpoint
	// 格式…），而它的报错是「bucket name len is between [3-63],now is 0」这种运维
	// 读不出所以然的话；配置全空恰恰是最常见的状态（本机没配、线上漏配），那时
	// Validate 那句「missing configuration OSS_ACCESS_KEY, OSS_SECRET_KEY, …」才是
	// 唯一有用的信息。旧顺序让 SDK 抢先失败，这句永远到不了运维眼前。
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	store, err := newOSSStore(cfg)
	if err != nil {
		return nil, err
	}
	return newUploader(cfg, store)
}

// newUploader 是 New 里不碰 SDK 的那一半，测试用一个假 Store 直接调它。
func newUploader(cfg Config, store Store) (*Uploader, error) {
	if store == nil {
		return nil, errors.New("upload: store is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.UseMD5 {
		slog.Warn("UPLOAD_USE_MD5=false is not supported and is ignored: a key built from the client filename lets it escape UPLOAD_PATH, so content-addressed md5 keys are always used",
			"setting", "UPLOAD_USE_MD5")
	}
	base, err := cfg.publicBase()
	if err != nil {
		return nil, err
	}
	return &Uploader{
		store:      store,
		prefix:     sanitizePrefix(cfg.Prefix),
		maxSize:    cfg.MaxFileSize,
		publicBase: base,
	}, nil
}

// MaxFileSize 是单个文件的字节上限，处理器用它给请求体设界。
func (u *Uploader) MaxFileSize() int64 { return u.maxSize }

// Upload 校验 data 并写入对象存储，返回绝对 URL。
//
// name 是客户端文件名：只用来做一次便宜的格式预检和回显，绝不进入对象键——
// 旧实现把文件名原样拼进 key，`filename="../../../x.png"` 就能写到
// UPLOAD_PATH 之外（filepath.Join 不清理对象键）。
func (u *Uploader) Upload(ctx context.Context, name string, data []byte) (Result, error) {
	if len(data) == 0 {
		return Result{}, ErrEmptyFile
	}
	if int64(len(data)) > u.maxSize {
		return Result{}, &SizeError{Limit: u.maxSize, Actual: int64(len(data))}
	}
	if !isAllowedExtension(name) {
		return Result{}, &TypeError{
			Name:   name,
			Reason: "仅支持 " + strings.Join(allowedExtensionList, "、") + " 格式的图片",
		}
	}
	kind, ok := imageTypes[sniffContentType(data)]
	if !ok {
		return Result{}, &TypeError{Name: name, Reason: "文件内容不是受支持的图片格式"}
	}

	sum := md5.Sum(data)
	digest := hex.EncodeToString(sum[:])
	key := u.key(digest, kind.extension)
	if err := u.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), kind.contentType); err != nil {
		return Result{}, err
	}
	return Result{
		URL:         u.publicBase + "/" + key,
		Key:         key,
		Name:        displayName(name),
		Size:        int64(len(data)),
		ContentType: kind.contentType,
		MD5:         digest,
	}, nil
}

// key 生成内容寻址的对象键，布局与旧站一致：
//
//	<prefix>/images/<md5[:2]>/<md5[2:4]>/<md5><ext>
//
// 分两级前缀是为了避免单个目录下对象过多。同一份字节永远落在同一个 key 上，
// 所以重试是幂等的：一次超时被判定为失败、而对象其实已经写成，重试只是把
// 同样的内容再写一遍。
func (u *Uploader) key(digest, ext string) string {
	return path.Join(u.prefix, imagesSegment, digest[:2], digest[2:4], digest+ext)
}

// isAllowedExtension 做一次便宜的格式预检，好在真正的嗅探之前就给出
// 一条用户看得懂的提示。它不能替代嗅探：改个后缀名就能骗过它。
func isAllowedExtension(name string) bool {
	return slices.Contains(allowedExtensionList, strings.ToLower(path.Ext(name)))
}

// sniffContentType 用标准库的嗅探器判定内容类型，返回形如 image/png 的字符串。
//
// 绝不信客户端声明的 Content-Type。旧实现判的是 strings.Contains(ct, "image/")，
// 而 image/svg+xml 恰好满足它——一个能执行脚本的格式会因此被当成图片写进
// 公开可读的桶里。
func sniffContentType(data []byte) string {
	if len(data) > sniffLength {
		data = data[:sniffLength]
	}
	detected, _, _ := strings.Cut(http.DetectContentType(data), ";")
	return detected
}

// sanitizePrefix 保证前缀不能用 .. 逃出桶根。path.Join 本身会清理 ..，
// 但只有把前缀先归一成绝对形式、再切掉首尾斜杠，清理才覆盖到前缀自身。
func sanitizePrefix(raw string) string {
	cleaned := strings.Trim(path.Clean("/"+strings.TrimSpace(raw)), "/")
	if cleaned == "" {
		return DefaultPrefix
	}
	return cleaned
}

// Validate 报告这份配置缺了什么，并逐个点名缺失的环境变量：
// 上传未配置时接口要返回 503 并把缺的名字写给运维，
// 而不是笼统的「未配置」——那正是配置分叉（REGISTRY_ENDPOINT/ETCD_ENDPOINTS）
// 当初被漏掉的原因。
func (c Config) Validate() error {
	var missing []string
	for _, item := range []struct{ name, value string }{
		{"OSS_ACCESS_KEY", c.AccessKey},
		{"OSS_SECRET_KEY", c.SecretKey},
		{"OSS_ENDPOINT", c.Endpoint},
		{"OSS_BUCKET", c.Bucket},
	} {
		if strings.TrimSpace(item.value) == "" {
			missing = append(missing, item.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("upload: missing configuration %s", strings.Join(missing, ", "))
	}
	if c.MaxFileSize <= 0 {
		return errors.New("upload: UPLOAD_MAX_FILE_SIZE must be a positive number of bytes")
	}
	if c.MaxFileSize > MaxFileSizeCeiling {
		return fmt.Errorf("upload: UPLOAD_MAX_FILE_SIZE must not exceed %d bytes: a larger body cannot finish inside the gateway upload timeout", MaxFileSizeCeiling)
	}
	_, err := c.publicBase()
	return err
}

// publicBase 返回拼接对象 URL 的基址，不含结尾斜杠。
//
// 优先 OSS_CNAME：bucket 绑了自定义域名时，对象必须走那个域名——从 endpoint
// 推出来的地址会绕过 CDN，而且在 bucket 收紧公共读的时候最先失效。
// 刻意不设第二个名字（旧站的 CDN_URL）来表达同一件事：两个名字指向一个设置，
// 正是 REGISTRY_ENDPOINT/ETCD_ENDPOINTS 那次分叉的成因。
func (c Config) publicBase() (string, error) {
	if cname := strings.TrimSpace(c.CNAME); cname != "" {
		base, err := normalizeBase(cname, "")
		if err != nil {
			return "", fmt.Errorf("upload: OSS_CNAME %q is not a usable URL: %w", cname, err)
		}
		return base, nil
	}
	base, err := normalizeBase(c.Endpoint, strings.TrimSpace(c.Bucket))
	if err != nil {
		return "", fmt.Errorf("upload: OSS_ENDPOINT %q is not a usable URL: %w", c.Endpoint, err)
	}
	return base, nil
}

// normalizeBase 把 endpoint 或域名归一成基址。旧实现用
// strings.TrimPrefix(endpoint, "https://") 做字符串手术，遇到 http://、
// 结尾斜杠或裸域名都会拼出坏 URL；这里交给 net/url 解析。
//
// bucket 非空时按 OSS 的公网规则拼成 <bucket>.<host>。endpoint 里已经带了
// bucket 是常见的手抄错误，与其拼出一个永远访问不到的地址，不如直接报错。
func normalizeBase(raw, bucket string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("empty value")
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Host == "" {
		return "", errors.New("no host")
	}
	host := parsed.Host
	if bucket != "" {
		if host == bucket || strings.HasPrefix(host, bucket+".") {
			return "", fmt.Errorf("%q already contains the bucket %q; use the bare endpoint", raw, bucket)
		}
		host = bucket + "." + host
	}
	normalized := url.URL{
		Scheme: parsed.Scheme,
		Host:   host,
		Path:   strings.TrimSuffix(parsed.Path, "/"),
	}
	return normalized.String(), nil
}

// HumanBytes 把字节数写成 10MB 这样的整数形式，用于面向用户的错误文案。
// 导出是因为处理器还要用它给「请求体整体超限」那条消息拼出同样的措辞。
func HumanBytes(size int64) string {
	switch {
	case size >= 1<<20 && size%(1<<20) == 0:
		return fmt.Sprintf("%dMB", size>>20)
	case size >= 1<<10 && size%(1<<10) == 0:
		return fmt.Sprintf("%dKB", size>>10)
	default:
		return fmt.Sprintf("%dB", size)
	}
}
