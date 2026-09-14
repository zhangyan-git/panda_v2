package upload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
)

// fakeStore 把 I/O 挡在外面，让这一整组测试都不需要凭据、不联网。
type fakeStore struct {
	keys         []string
	contents     []string
	sizes        []int64
	contentTypes []string
	payloads     [][]byte
	err          error
}

func (s *fakeStore) Put(_ context.Context, key string, r io.Reader, size int64, contentType string) error {
	if s.err != nil {
		return s.err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.keys = append(s.keys, key)
	s.contents = append(s.contents, string(data))
	s.sizes = append(s.sizes, size)
	s.contentTypes = append(s.contentTypes, contentType)
	s.payloads = append(s.payloads, data)
	return nil
}

func testConfig() Config {
	return Config{
		AccessKey:   "test-access-key",
		SecretKey:   "test-secret-key",
		Endpoint:    "oss-cn-hangzhou.aliyuncs.com",
		Bucket:      "test-bucket",
		MaxFileSize: 1024,
		UseMD5:      true,
	}
}

func newTestUploader(t *testing.T, cfg Config, store Store) *Uploader {
	t.Helper()
	uploader, err := newUploader(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	return uploader
}

// pngBytes 造一份以真实 PNG magic 开头的字节流，长度正好 size。
// 嗅探只看开头 512 字节，所以签名之后补零就够了。
func pngBytes(size int) []byte {
	signature := []byte("\x89PNG\r\n\x1a\n")
	if size < len(signature) {
		return signature[:size]
	}
	return append(signature, make([]byte, size-len(signature))...)
}

func TestUploadIsContentAddressed(t *testing.T) {
	store := &fakeStore{}
	uploader := newTestUploader(t, testConfig(), store)
	data := pngBytes(64)

	first, err := uploader.Upload(context.Background(), "logo.png", data)
	if err != nil {
		t.Fatal(err)
	}
	// 同样的字节、不同的文件名：必须落在同一个键上，否则重试就不再幂等。
	second, err := uploader.Upload(context.Background(), "another-name.png", data)
	if err != nil {
		t.Fatal(err)
	}
	if first.Key != second.Key {
		t.Fatalf("same bytes produced different keys: %q vs %q", first.Key, second.Key)
	}
	if store.keys[0] != store.keys[1] {
		t.Fatalf("store saw different keys: %q vs %q", store.keys[0], store.keys[1])
	}

	expected := "panda-v2/images/" + first.MD5[:2] + "/" + first.MD5[2:4] + "/" + first.MD5 + ".png"
	if first.Key != expected {
		t.Fatalf("key = %q, want %q", first.Key, expected)
	}
	if first.URL != "https://test-bucket.oss-cn-hangzhou.aliyuncs.com/"+first.Key {
		t.Fatalf("url = %q", first.URL)
	}
	if !strings.HasPrefix(first.URL, "https://") {
		t.Fatalf("url must be absolute: %q", first.URL)
	}
	if first.ContentType != "image/png" {
		t.Fatalf("content type = %q", first.ContentType)
	}
	if first.Name != "logo.png" {
		t.Fatalf("name = %q", first.Name)
	}
	if first.Size != int64(len(data)) {
		t.Fatalf("size = %d", first.Size)
	}
	if !bytes.Equal(store.payloads[0], data) {
		t.Fatal("stored payload differs from the uploaded bytes")
	}
}

// 旧实现在 UPLOAD_USE_MD5=false 时把客户端文件名原样拼进对象键，
// filename="../../../x.png" 就能写到 UPLOAD_PATH 之外。键必须只由内容决定。
func TestUploadKeyNeverContainsClientPath(t *testing.T) {
	store := &fakeStore{}
	uploader := newTestUploader(t, testConfig(), store)

	for _, name := range []string{"../../etc/passwd.png", "a/b/../../x.png", "/absolute.png", "..\\..\\win.png"} {
		result, err := uploader.Upload(context.Background(), name, pngBytes(32))
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if strings.Contains(result.Key, "..") {
			t.Fatalf("%q produced a traversing key %q", name, result.Key)
		}
		if !strings.HasPrefix(result.Key, "panda-v2/images/") {
			t.Fatalf("%q escaped the upload prefix: %q", name, result.Key)
		}
		if strings.ContainsAny(result.Name, `/\`) {
			t.Fatalf("%q echoed a path back as the display name %q", name, result.Name)
		}
	}
}

func TestUploadEnforcesSizeLimit(t *testing.T) {
	cfg := testConfig()
	cfg.MaxFileSize = 64
	store := &fakeStore{}
	uploader := newTestUploader(t, cfg, store)

	if _, err := uploader.Upload(context.Background(), "small.png", pngBytes(63)); err != nil {
		t.Fatalf("max-1 must be accepted: %v", err)
	}
	if _, err := uploader.Upload(context.Background(), "exact.png", pngBytes(64)); err != nil {
		t.Fatalf("exactly max must be accepted: %v", err)
	}
	_, err := uploader.Upload(context.Background(), "big.png", pngBytes(65))
	var sizeErr *SizeError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("max+1 must be rejected with a SizeError, got %v", err)
	}
	if sizeErr.Limit != 64 || sizeErr.Actual != 65 {
		t.Fatalf("size error = %+v", sizeErr)
	}
	if len(store.keys) != 2 {
		t.Fatalf("rejected upload reached the store: %v", store.keys)
	}
	if _, err := uploader.Upload(context.Background(), "empty.png", nil); !errors.Is(err, ErrEmptyFile) {
		t.Fatalf("empty file must be rejected, got %v", err)
	}
}

// 旧实现的类型检查是 strings.Contains(ct, "image/")，而 image/svg+xml 恰好满足它。
// svg 能执行脚本，且对象是公开可读的——这条断言不能靠扩展名之外的任何东西兜住。
func TestUploadRejectsDisallowedExtensions(t *testing.T) {
	store := &fakeStore{}
	uploader := newTestUploader(t, testConfig(), store)

	tests := []struct {
		name string
		file string
	}{
		{name: "svg", file: "logo.svg"},
		{name: "html", file: "page.html"},
		{name: "executable", file: "payload.exe"},
		{name: "no extension", file: "logo"},
		{name: "upper case svg", file: "logo.SVG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := uploader.Upload(context.Background(), tt.file, pngBytes(32))
			var typeErr *TypeError
			if !errors.As(err, &typeErr) {
				t.Fatalf("expected a TypeError, got %v", err)
			}
		})
	}
	if len(store.keys) != 0 {
		t.Fatalf("rejected uploads reached the store: %v", store.keys)
	}
}

// 扩展名对得上不代表内容对得上：改个后缀就能骗过扩展名检查，
// 所以对象上声明的 Content-Type 必须来自嗅探结果。
func TestUploadSniffsContentInsteadOfTrustingTheName(t *testing.T) {
	store := &fakeStore{}
	uploader := newTestUploader(t, testConfig(), store)

	html := []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>")
	if _, err := uploader.Upload(context.Background(), "logo.png", html); err == nil {
		t.Fatal("html disguised as a png must be rejected")
	}

	// 反过来：真图片配错扩展名是允许的，只是对象键的扩展名跟随内容，
	// 这样键的扩展名和对象上的 Content-Type 永远一致。
	result, err := uploader.Upload(context.Background(), "actually-a-png.jpg", pngBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	if result.ContentType != "image/png" || !strings.HasSuffix(result.Key, ".png") {
		t.Fatalf("key extension must follow the sniffed type: %+v", result)
	}
	if store.contentTypes[0] != "image/png" {
		t.Fatalf("store received content type %q", store.contentTypes[0])
	}
}

func TestUploadPropagatesStoreFailure(t *testing.T) {
	store := &fakeStore{err: errors.New("bucket is not writable")}
	uploader := newTestUploader(t, testConfig(), store)

	if _, err := uploader.Upload(context.Background(), "logo.png", pngBytes(32)); err == nil {
		t.Fatal("a failing store must surface as an upload error")
	}
}

func TestPublicBase(t *testing.T) {
	tests := []struct {
		name       string
		cname      string
		endpoint   string
		bucket     string
		want       string
		wantErr    bool
		errMention string
	}{
		{
			name:     "endpoint with scheme",
			endpoint: "https://oss-cn-hangzhou.aliyuncs.com",
			bucket:   "panda",
			want:     "https://panda.oss-cn-hangzhou.aliyuncs.com",
		},
		{
			name:     "bare host",
			endpoint: "oss-cn-hangzhou.aliyuncs.com",
			bucket:   "panda",
			want:     "https://panda.oss-cn-hangzhou.aliyuncs.com",
		},
		{
			name:     "trailing slash",
			endpoint: "https://oss-cn-hangzhou.aliyuncs.com/",
			bucket:   "panda",
			want:     "https://panda.oss-cn-hangzhou.aliyuncs.com",
		},
		{
			name:     "http endpoint keeps its scheme",
			endpoint: "http://oss-cn-hangzhou.aliyuncs.com",
			bucket:   "panda",
			want:     "http://panda.oss-cn-hangzhou.aliyuncs.com",
		},
		{
			name:     "cname wins and gets no bucket prefix",
			cname:    "https://img.example.com",
			endpoint: "https://oss-cn-hangzhou.aliyuncs.com",
			bucket:   "panda",
			want:     "https://img.example.com",
		},
		{
			name:     "cname without scheme",
			cname:    "img.example.com",
			endpoint: "oss-cn-hangzhou.aliyuncs.com",
			bucket:   "panda",
			want:     "https://img.example.com",
		},
		{
			name:       "endpoint that already contains the bucket",
			endpoint:   "https://panda.oss-cn-hangzhou.aliyuncs.com",
			bucket:     "panda",
			wantErr:    true,
			errMention: "already contains the bucket",
		},
		{
			name:       "endpoint without a host",
			endpoint:   "https://",
			bucket:     "panda",
			wantErr:    true,
			errMention: "not a usable URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.CNAME = tt.cname
			if tt.endpoint != "" {
				cfg.Endpoint = tt.endpoint
			}
			cfg.Bucket = tt.bucket
			base, err := cfg.publicBase()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), tt.errMention) {
					t.Fatalf("error %q does not mention %q", err, tt.errMention)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if base != tt.want {
				t.Fatalf("base = %q, want %q", base, tt.want)
			}
			parsed, err := url.Parse(base)
			if err != nil || !parsed.IsAbs() {
				t.Fatalf("base %q is not an absolute URL", base)
			}
		})
	}
}

func TestValidateNamesEveryMissingVariable(t *testing.T) {
	all := map[string]func(*Config, string){
		"OSS_ACCESS_KEY": func(c *Config, v string) { c.AccessKey = v },
		"OSS_SECRET_KEY": func(c *Config, v string) { c.SecretKey = v },
		"OSS_ENDPOINT":   func(c *Config, v string) { c.Endpoint = v },
		"OSS_BUCKET":     func(c *Config, v string) { c.Bucket = v },
	}
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("a complete config must validate: %v", err)
	}
	for name, clear := range all {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			clear(&cfg, "")
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			// 报错要点名缺的是哪个变量：上传未配置时接口要把名字回给运维，
			// 笼统的一句「未配置」正是配置分叉当初被漏掉的原因。
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q does not name %s", err, name)
			}
		})
	}
	t.Run("all four at once are all named", func(t *testing.T) {
		err := Config{MaxFileSize: 1024}.Validate()
		for name := range all {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error %q does not name %s", err, name)
			}
		}
	})
}

// TestNewValidatesBeforeTouchingTheSDK 钉住 New 里的顺序：配置全空时必须报出
// 「缺哪几个变量」，而不是 SDK 那句「bucket name len is between [3-63]」。
//
// 这组测试里其它用例都走 newUploader（不碰 SDK 的那一半），所以 New 自己的顺序
// 对它们不可见——这个 bug 就是这么活下来的：Validate 的文案被测过，接线没被测过。
// 这条用例不需要凭据：修好之后它根本到不了 SDK。
func TestNewValidatesBeforeTouchingTheSDK(t *testing.T) {
	_, err := New(Config{MaxFileSize: 1024})
	if err == nil {
		t.Fatal("an empty OSS configuration must not produce an uploader")
	}
	for _, name := range []string{"OSS_ACCESS_KEY", "OSS_SECRET_KEY", "OSS_ENDPOINT", "OSS_BUCKET"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not name %s", err, name)
		}
	}
}

// TestNewNamesTheOneMissingVariable 覆盖「只缺一个」的常见情形：服务已经在跑、
// 运维只是删掉了一行。这时报错必须精确到那一个名字，而不是把四个都列一遍。
func TestNewNamesTheOneMissingVariable(t *testing.T) {
	cfg := testConfig()
	cfg.Bucket = ""
	_, err := New(cfg)
	if err == nil {
		t.Fatal("a config without OSS_BUCKET must not produce an uploader")
	}
	if !strings.Contains(err.Error(), "OSS_BUCKET") {
		t.Fatalf("error %q does not name OSS_BUCKET", err)
	}
	if strings.Contains(err.Error(), "OSS_ACCESS_KEY") {
		t.Fatalf("error %q names a variable that is set", err)
	}
}

func TestValidateRejectsUnusableSizeLimit(t *testing.T) {
	tests := []struct {
		name    string
		size    int64
		wantErr string
	}{
		{name: "zero", size: 0, wantErr: "positive"},
		{name: "negative", size: -1, wantErr: "positive"},
		{name: "at the ceiling", size: MaxFileSizeCeiling},
		{name: "over the ceiling", size: MaxFileSizeCeiling + 1, wantErr: "must not exceed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MaxFileSize = tt.size
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSanitizePrefix(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "", want: DefaultPrefix},
		{raw: "   ", want: DefaultPrefix},
		{raw: "panda-v2", want: "panda-v2"},
		{raw: "/panda-v2/", want: "panda-v2"},
		{raw: "a/b", want: "a/b"},
		{raw: "../escape", want: "escape"},
		{raw: "..", want: DefaultPrefix},
		{raw: "uploads/../../etc", want: "etc"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := sanitizePrefix(tt.raw); got != tt.want {
				t.Fatalf("sanitizePrefix(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestUploadRejectsMissingStore(t *testing.T) {
	if _, err := newUploader(testConfig(), nil); err == nil {
		t.Fatal("a nil store must be rejected at construction, not at the first upload")
	}
}

// UPLOAD_USE_MD5=false 是旧配置里可能带过来的值。它不被支持，但也不该让服务起不来。
func TestUploaderIgnoresUseMD5False(t *testing.T) {
	cfg := testConfig()
	cfg.UseMD5 = false
	store := &fakeStore{}
	uploader := newTestUploader(t, cfg, store)

	result, err := uploader.Upload(context.Background(), "logo.png", pngBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Key, result.MD5) {
		t.Fatalf("key %q must stay content-addressed", result.Key)
	}
}
