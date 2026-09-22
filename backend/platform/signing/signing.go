// Package signing 把「怎么拼待签串」和「用什么摘要」拆成两个正交的维度，
// 出站（我们签名发给渠道）与入站（合作方签名发给我们）共用同一份实现。
//
// 拆成两维不是设计洁癖，是从老系统 panda_serve 里读出来的事实：同一套骨架
// —— 过滤/排序 → "k=v" 用 "&" 连 → 追加密钥 → 摘要 —— 在那里被写了四遍，
// 每一遍只改其中一两个参数：
//
//	位置              追加               摘要                密钥用法
//	对外 API 验签     &secret={secret}   HMAC-SHA256 小写   既拼进串又当 HMAC 密钥
//	丰选万联          &key={secret}      MD5 小写            只拼进串
//	首创饭卡/北方     &Key={secret}      MD5 大写            只拼进串
//
// 见 panda_serve/internal/app/openapi/services/merchant_api_service.go:99、
// internal/partners/fengxuan/client.go:408、internal/partners/shouchuang/client.go:52。
// 骨架相同、参数不同 —— 所以它们是「一个实现 + N 份配置」，不是 N 份实现。
//
// 两个维度分别落在 Canonical（拼串）与 Algorithm（摘要）上，互不知道对方：
// MD5 类只看 payload（密钥已拼进串里），HMAC 类还要拿 secret 当 HMAC 密钥。
// 把 secret 同时传给两者是有意的 —— 这正是对外 API 那套「既拼进去又当密钥」的形状。
//
// # 三个约定
//
//   - 验签一律 hmac.Equal，**不用 ==**。老系统三处验签都是普通字符串比较
//     （merchant_api_service.go:92、partners/fengxuan/client.go:396、
//     directmealcard/client.go:325），时序侧信道能逐字节试出签名。
//   - 摘要只出 hex 小写/大写与 base64 三种编码，不自己发明格式。
//   - 拼串无条件在密钥前加 "&"，即使前面为空 —— 与老系统逐字节一致（见 Canonical.Build）。
package signing

import (
	"crypto"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

var (
	// ErrAlgorithmUnknown：配置里写了一个没注册的算法名。
	//
	// 它在**启用渠道时**就该炸（见渠道配置校验），而不是等第一笔支付打进来 ——
	// 那时它是一次 5xx，用户看到的是「点了没反应」。
	ErrAlgorithmUnknown = errors.New("signing algorithm is not registered")

	// ErrSignatureMismatch：验签不通过。
	ErrSignatureMismatch = errors.New("signature does not match")

	// ErrKeyNotConfigured：算法要密钥但没给。
	//
	// 与 ErrSignatureMismatch 分开是必须的：前者是**运维漏配**（密钥槽是空的），
	// 后者可能是**有人在伪造**。混成一个错误会让「密钥没配」看起来像「有人攻击」，
	// 反过来也一样 —— 拿不到密钥时**绝不能放行**（见 payment 域的
	// ErrSecretNotConfigured）。
	ErrKeyNotConfigured = errors.New("signing key is not configured")
)

// Canonical 是待签串的构造规则。
//
// 零值是「不过滤、不排序、不追加密钥」，即把 map 里的键按任意顺序原样拼出来 ——
// 这几乎永远不是你想要的（Go 的 map 迭代顺序是随机的，同一份参数会签出不同的串），
// 所以 Sort 事实上应当总是为 true。保留它可关是为将来某家渠道的怪规范留的口子。
type Canonical struct {
	// SkipEmpty 为 true 时丢掉值为空串的参数。
	//
	// 老系统四处的差异里包含这一条：丰选万联过滤、对外 API 不过滤。
	SkipEmpty bool
	// Sort 为 true 时按键的字典序升序排列。四处都排。
	Sort bool
	// Omit 是要从待签串里排除的键名，通常是**签名参数自己**（"sign" / "Sign"）。
	//
	// 不排除它就会自指：先把 sign="" 拼进串算出一个签名，再把签名填回 sign 字段，
	// 而两端各自拼串时看到的 sign 值不同，永远对不上。老系统两处都靠
	// `k != "sign"` / `k != "Sign"` 处理这件事（fengxuan/client.go:412、
	// shouchuang/client.go:56），且**大小写敏感** —— 两个渠道的字段名本来就差一个
	// 大小写，拿它当同一个键处理会把签名算错。
	Omit []string
	// KeyName 是追加密钥时用的键名，例如 "key" / "Key" / "secret"。
	//
	// 为空表示**不追加密钥**（此时密钥只作为 HMAC 的 key 参与摘要，或者根本不用）。
	KeyName string
}

// Build 拼出待签串。
//
// 无条件在密钥前写 "&" —— 老系统四处都是这么写的（fengxuan/client.go:434 的
// `builder.WriteString("&Key=")` 不看前面有没有内容）。参数为空时它会产出
// "&Key=xxx" 而不是 "Key=xxx"，看着别扭，但**逐字节对齐比看着顺眼重要**：
// 我们与老系统对同一条渠道签出的串必须一模一样，否则迁移时对不上账。
func (c Canonical) Build(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if c.SkipEmpty && v == "" {
			continue
		}
		if c.omits(k) {
			continue
		}
		keys = append(keys, k)
	}
	if c.Sort {
		sort.Strings(keys)
	}

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("&")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params[k])
	}
	if c.KeyName != "" {
		b.WriteString("&")
		b.WriteString(c.KeyName)
		b.WriteString("=")
		b.WriteString(secret)
	}
	return b.String()
}

// omits 判断一个键是否在排除名单里。**大小写敏感**，理由见 Omit 字段的注释。
func (c Canonical) omits(key string) bool {
	return slices.Contains(c.Omit, key)
}

// Algorithm 是摘要算法。按名字注册，配置文件里写名字。
//
// 名字进配置，所以**改名是破坏性变更**：已经建好的渠道会突然找不到算法。
// 加新算法是安全的（旧的渠道不受影响）。
type Algorithm string

const (
	// MD5Lower：MD5 摘要，hex 小写。丰选万联、优联用。
	MD5Lower Algorithm = "md5_lower"
	// MD5Upper：MD5 摘要，hex 大写。首创饭卡、北方工业饭卡用。
	MD5Upper Algorithm = "md5_upper"
	// HMACSHA256Hex：HMAC-SHA256，hex 小写。对外 API 验签、家园消费金用。
	HMACSHA256Hex Algorithm = "hmac_sha256_hex"
	// HMACSHA256Base64：HMAC-SHA256，base64。银联商务用。
	HMACSHA256Base64 Algorithm = "hmac_sha256_base64"
	// RSA2SHA256：RSA-SHA256（PKCS#1 v1.5），输出 base64。微信 APIv3、支付宝用。
	//
	// 它与其他算法有两个不同，都在 Key 上：要的是 PEM 私钥而不是对称密钥；
	// 待签串往往由协议自己规定（微信是 "METHOD\nURL\ntimestamp\nnonce\nbody\n"，
	// 支付宝是排序后的 k=v），所以调用方多半走 SignPayload 而不是 Sign。
	RSA2SHA256 Algorithm = "rsa2_sha256"
	// None：不签名。只给联调与内网渠道用。
	//
	// **它能通过任何验签**（期望值是空串，来一个空签名就对上了），所以
	// 对外验签的路径上绝不能用它。渠道配置校验里要挡这一条。
	None Algorithm = "none"
)

// Supported 返回所有已注册的算法名，给后台的「试跑」与配置校验用。
//
// 返回切片而不是 map：调用方几乎总是要排序后展示或比较，直接给有序的更省事。
func Supported() []Algorithm {
	return []Algorithm{MD5Lower, MD5Upper, HMACSHA256Hex, HMACSHA256Base64, RSA2SHA256, None}
}

// ParseAlgorithm 把配置里的字符串翻成算法，不认识就报错。
//
// 大小写与空白不敏感：配置是人手填的，"MD5_LOWER " 该当 md5_lower 用。
// 这一条与「名字进配置」配套 —— 名字既然由人输入，解析就该宽容一点，
// 否则运营填错一个大小写就得到一个 500。
func ParseAlgorithm(name string) (Algorithm, error) {
	normalized := Algorithm(strings.ToLower(strings.TrimSpace(name)))
	for _, alg := range Supported() {
		if alg == normalized {
			return alg, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrAlgorithmUnknown, name)
}

// Key 是一次签名或验签用到的密钥材料。
//
// 对称与非对称塞进同一个结构，是为了让上层（渠道适配器）不必按算法分支准备密钥 ——
// 密钥槽解析出来就是它，适配器把它交给 Sign / Verify 即可。
type Key struct {
	// Secret 是对称密钥：MD5 类把它拼进串，HMAC 类把它当 HMAC 的密钥。
	Secret string
	// PrivateKeyPEM 是 PEM 编码的 RSA 私钥，RSA2SHA256 签名时用。
	// PKCS#1（BEGIN RSA PRIVATE KEY）与 PKCS#8（BEGIN PRIVATE KEY）都认 ——
	// 微信商户平台下发的 apiclient_key.pem 是前者，支付宝工具产出的是后者。
	PrivateKeyPEM []byte
	// PublicKeyPEM 是 PEM 编码的 RSA 公钥或证书，RSA2SHA256 验签时用。
	//
	// 能直接吃证书（BEGIN CERTIFICATE）是有意的：微信 APIv3 的回调验签用的是
	// **平台证书**而不是裸公钥，从证书里取公钥这一步在这里做掉，适配器就不必自己
	// 解析 x509。
	PublicKeyPEM []byte
}

// Spec 是一份完整的签名规则：怎么拼串 + 用什么摘要。
type Spec struct {
	Canonical Canonical
	Algorithm Algorithm
}

// Sign 按 spec 拼串并摘要，返回签名值。
func Sign(params map[string]string, spec Spec, key Key) (string, error) {
	return SignPayload(spec.Canonical.Build(params, key.Secret), spec.Algorithm, key)
}

// SignPayload 跳过拼串，直接对 payload 做摘要。
//
// 给待签串由协议自己规定的渠道用（微信 APIv3 的换行式、支付宝的排序 k=v 式）——
// 它们的"拼串"不是 Canonical 能表达的形状，硬塞进来会把 Canonical 撑成一个
// 什么都能装的模板引擎，那比现在这样多一个入口难懂得多。
func SignPayload(payload string, alg Algorithm, key Key) (string, error) {
	switch alg {
	case MD5Lower, MD5Upper:
		sum := md5.Sum([]byte(payload))
		if alg == MD5Upper {
			return strings.ToUpper(hex.EncodeToString(sum[:])), nil
		}
		return hex.EncodeToString(sum[:]), nil

	case HMACSHA256Hex, HMACSHA256Base64:
		if key.Secret == "" {
			return "", fmt.Errorf("%w: %s needs a symmetric secret", ErrKeyNotConfigured, alg)
		}
		mac := hmac.New(sha256.New, []byte(key.Secret))
		mac.Write([]byte(payload))
		sum := mac.Sum(nil)
		if alg == HMACSHA256Base64 {
			return base64.StdEncoding.EncodeToString(sum), nil
		}
		return hex.EncodeToString(sum), nil

	case RSA2SHA256:
		priv, err := parseRSAPrivateKey(key.PrivateKeyPEM)
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256([]byte(payload))
		sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
		if err != nil {
			return "", fmt.Errorf("rsa2_sha256 sign: %w", err)
		}
		return base64.StdEncoding.EncodeToString(sig), nil

	case None:
		return "", nil

	default:
		return "", fmt.Errorf("%w: %q", ErrAlgorithmUnknown, alg)
	}
}

// Verify 按 spec 拼串、算期望签名，再与 provided 比。
//
// 不通过返回 ErrSignatureMismatch；密钥没配、算法不认识这类**配置问题**原样返回，
// 让调用方能区分「验签失败」与「验不了签」。
func Verify(params map[string]string, spec Spec, key Key, provided string) error {
	return VerifyPayload(spec.Canonical.Build(params, key.Secret), spec.Algorithm, key, provided)
}

// VerifyPayload 与 SignPayload 对称，直接校验一份 payload 的签名。
func VerifyPayload(payload string, alg Algorithm, key Key, provided string) error {
	// RSA 的验签不是「算出期望值再比字符串」——它是把签名交给公钥运算，
	// 由 rsa.VerifyPKCS1v15 自己判断。所以它必须走独立分支，不能复用下面的比较。
	if alg == RSA2SHA256 {
		return verifyRSA2(payload, key, provided)
	}

	expected, err := SignPayload(payload, alg, key)
	if err != nil {
		return err
	}
	// hmac.Equal 而不是 ==：长度不同时它也是常量时间返回的。
	// 用 == 会让攻击者靠响应时间逐字节试出正确签名（时序侧信道）。
	if !hmac.Equal([]byte(expected), []byte(provided)) {
		return ErrSignatureMismatch
	}
	return nil
}

func verifyRSA2(payload string, key Key, provided string) error {
	if len(key.PublicKeyPEM) == 0 {
		return fmt.Errorf("%w: rsa2_sha256 needs a PEM public key or certificate", ErrKeyNotConfigured)
	}
	pub, err := parseRSAPublicKey(key.PublicKeyPEM)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(provided))
	if err != nil {
		// 签名不是合法 base64 —— 这是**签名不对**，不是配置问题。
		// 返回 ErrSignatureMismatch 让调用方按「有人伪造」处理。
		return ErrSignatureMismatch
	}
	digest := sha256.Sum256([]byte(payload))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return ErrSignatureMismatch
	}
	return nil
}

// parseRSAPrivateKey 认 PKCS#1 与 PKCS#8 两种 PEM 私钥。
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	if len(pemBytes) == 0 {
		return nil, fmt.Errorf("%w: rsa2_sha256 needs a PEM private key", ErrKeyNotConfigured)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		// 不把内容写进错误里 —— 私钥可能就混在这段文本中。
		return nil, errors.New("rsa private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("rsa private key: neither PKCS#1 nor PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("rsa private key: PKCS#8 block holds %T, not *rsa.PrivateKey", parsed)
	}
	return key, nil
}

// CheckPublicKey 只回答「这份 PEM 公钥或证书能不能用」，不签名也不验签。
//
// 与 parseRSAPublicKey 是同一份实现，导出的理由只有一个：**要在配置生效之前问一次这个问题**。
// 验签那条路上「证书解不开」与「签名不对」都表现为验签失败，而等到有人去分辨它们的时候，
// 通常已经是第一笔真实回调了——那时钱已经收了、支付单还停在原地。试跑与凭据解析因此需要一个
// 只问形状、不问签名的入口。
//
// 它不校验有效期：证书轮换是渠道那条链上的事（微信会换平台证书），过期不等于「配错了」，
// 把它算成配置错误会让一次正常的轮换表现成「这条渠道用不了」。
func CheckPublicKey(pemBytes []byte) error {
	_, err := parseRSAPublicKey(pemBytes)
	return err
}

// parseRSAPublicKey 认 PEM 公钥与 PEM 证书两种输入。
//
// 吃证书这一条是为微信 APIv3：它的回调验签用的是平台**证书**，
// 从证书里取公钥这一步在这里做掉，适配器就不必自己 x509.ParseCertificate。
func parseRSAPublicKey(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("rsa public key is not valid PEM")
	}

	if block.Type == "CERTIFICATE" {
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse platform certificate: %w", err)
		}
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("platform certificate holds %T, not *rsa.PublicKey", cert.PublicKey)
		}
		return pub, nil
	}

	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		key, ok := pub.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("PEM block holds %T, not *rsa.PublicKey", pub)
		}
		return key, nil
	}
	// 兼容 PKCS#1 公钥。
	return x509.ParsePKCS1PublicKey(block.Bytes)
}
