package config

import (
	"strings"
	"testing"
)

// setServiceEnv 把 Load 的必填项补齐，好让每个用例只关心自己那一个变量。
// 传空字符串也是显式设置，避免继承上一个用例或开发者本机的值。
func setServiceEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PANDA_ENV", "test")
	t.Setenv("REDIS_DB", "")
	t.Setenv("USER_GRPC_ADDR", "127.0.0.1:19081")
	t.Setenv("MERCHANT_GRPC_ADDR", "127.0.0.1:19082")
	t.Setenv("MERCHANT_INTERNAL_TOKEN", "test-only-internal-credential-32-bytes")
	t.Setenv("MERCHANT_OWNERSHIP_TIMEOUT_MS", "")
	// user-service / merchant-service 都有各自的库，resolveDatabase 没有回退，
	// 少了哪一份就起不来。这两个用例要测的是 HTTP 超时和上传设置，不补齐的话它们
	// 会因为「缺 USER_DATABASE_URL」而红，红得跟它们要测的东西毫无关系。
	setOwnedDatabaseURLs(t)
}

func TestLoadHTTPTimeout(t *testing.T) {
	// 不要发现开发者的 .env，也不要依赖真实的凭据。
	t.Chdir(t.TempDir())
	setServiceEnv(t)
	tests := []struct {
		name, service, value string
		want                 int
		wantErr              bool
	}{
		{name: "user default", service: "user-service", want: 5000},
		{name: "gateway default", service: "gateway-service", want: 5000},
		// merchant-service 的默认值要装得下一次 10MB 上传，所以它和其他服务不同。
		{name: "merchant default is the upload budget", service: "merchant-service", want: 90000},
		{name: "explicit value wins", service: "merchant-service", value: "120000", want: 120000},
		{name: "lower bound", service: "user-service", value: "1000", want: 1000},
		{name: "upper bound", service: "user-service", value: "300000", want: 300000},
		{name: "below lower bound", service: "user-service", value: "999", wantErr: true},
		{name: "above upper bound", service: "user-service", value: "300001", wantErr: true},
		{name: "zero", service: "user-service", value: "0", wantErr: true},
		{name: "not a number", service: "user-service", value: "five", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HTTP_TIMEOUT_MS", tt.value)
			cfg, err := Load(tt.service)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a configuration error")
				}
				if !strings.Contains(err.Error(), "HTTP_TIMEOUT_MS") {
					t.Fatalf("error %q does not name HTTP_TIMEOUT_MS", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.HTTPTimeoutMS != tt.want {
				t.Fatalf("HTTPTimeoutMS = %d, want %d", cfg.HTTPTimeoutMS, tt.want)
			}
		})
	}
}

func TestLoadUploadSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	setServiceEnv(t)

	newEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv("OSS_ACCESS_KEY", "")
		t.Setenv("OSS_SECRET_KEY", "")
		t.Setenv("OSS_ENDPOINT", "")
		t.Setenv("OSS_BUCKET", "")
		t.Setenv("OSS_CNAME", "")
		t.Setenv("UPLOAD_PATH", "")
		t.Setenv("UPLOAD_MAX_FILE_SIZE", "")
		t.Setenv("UPLOAD_USE_MD5", "")
	}

	t.Run("defaults", func(t *testing.T) {
		newEnv(t)
		cfg, err := Load("merchant-service")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UploadMaxFileSize != 10<<20 {
			t.Fatalf("UploadMaxFileSize = %d, want %d", cfg.UploadMaxFileSize, 10<<20)
		}
		if !cfg.UploadUseMD5 {
			t.Fatal("UPLOAD_USE_MD5 defaults to true: false is not a supported mode")
		}
		// 前缀留空是有意的：默认值只在 platform/upload 里定义一次，
		// 这里再兜一个就成了两个真相。
		if cfg.UploadPath != "" {
			t.Fatalf("UploadPath = %q, want the empty value passed through", cfg.UploadPath)
		}
	})

	// 缺 OSS 凭据是一个自洽的状态：接口要 503 并点名缺什么，而不是整个服务起不来。
	t.Run("missing oss credentials do not fail the load", func(t *testing.T) {
		newEnv(t)
		cfg, err := Load("merchant-service")
		if err != nil {
			t.Fatalf("Load must succeed without OSS settings: %v", err)
		}
		if cfg.OSSAccessKey != "" || cfg.OSSBucket != "" {
			t.Fatalf("unexpected OSS settings: %q %q", cfg.OSSAccessKey, cfg.OSSBucket)
		}
	})

	// 旧后端的键名原样搬过来就能用——「搬配置」指的就是这件事。
	t.Run("legacy key names pass through", func(t *testing.T) {
		newEnv(t)
		t.Setenv("OSS_ACCESS_KEY", "placeholder-access-key")
		t.Setenv("OSS_SECRET_KEY", "placeholder-secret-key")
		t.Setenv("OSS_ENDPOINT", "oss-cn-hangzhou.aliyuncs.com")
		t.Setenv("OSS_BUCKET", "test-bucket")
		t.Setenv("OSS_CNAME", "img.example.com")
		t.Setenv("UPLOAD_PATH", "panda-v2")
		t.Setenv("UPLOAD_MAX_FILE_SIZE", "5242880")
		t.Setenv("UPLOAD_USE_MD5", "true")
		cfg, err := Load("merchant-service")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OSSEndpoint != "oss-cn-hangzhou.aliyuncs.com" || cfg.OSSCNAME != "img.example.com" {
			t.Fatalf("endpoint/cname = %q %q", cfg.OSSEndpoint, cfg.OSSCNAME)
		}
		if cfg.UploadPath != "panda-v2" || cfg.UploadMaxFileSize != 5242880 {
			t.Fatalf("upload path/size = %q %d", cfg.UploadPath, cfg.UploadMaxFileSize)
		}
	})

	// 旧配置里可能是 false。它不再被支持（理由见 platform/upload），
	// 但读出来必须成功，剩下的交给上传包去警告。
	t.Run("UPLOAD_USE_MD5 false is read but ignored downstream", func(t *testing.T) {
		newEnv(t)
		t.Setenv("UPLOAD_USE_MD5", "false")
		cfg, err := Load("merchant-service")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UploadUseMD5 {
			t.Fatal("UPLOAD_USE_MD5=false must be read as false")
		}
	})

	t.Run("UPLOAD_USE_MD5 must be a boolean", func(t *testing.T) {
		newEnv(t)
		t.Setenv("UPLOAD_USE_MD5", "maybe")
		if _, err := Load("merchant-service"); err == nil {
			t.Fatal("expected a configuration error: a misspelled switch must not read as false")
		}
	})

	t.Run("UPLOAD_MAX_FILE_SIZE must be a positive integer", func(t *testing.T) {
		newEnv(t)
		t.Setenv("UPLOAD_MAX_FILE_SIZE", "ten-megabytes")
		if _, err := Load("merchant-service"); err == nil {
			t.Fatal("expected a configuration error")
		}
	})
}
