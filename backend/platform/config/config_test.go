package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain 先把 cwd 挪到一个空目录，再跑整个包。
//
// 起因是一个真实发生过的「本机绿、CI 红」，就在这个包上：Load 会从 cwd 向上找 .env
// （loadDotEnv），把读到的键写进**进程环境**——只写那些还没设过的。于是任何一个没先
// chdir 就调 Load 的用例，都会把开发者本机 panda_v2/.env 里的东西（包括每一份
// *_DATABASE_URL）永久留在这个测试进程里，它后面所有用例都跟着沾光，而它们自己
// chdir 得再干净也晚了，值已经在环境里。包内用例的结果因此取决于这台机器上有没有
// 那个文件、以及谁先跑：
//
//	go test -run TestLoadHTTPTimeout ./platform/config/   → FAIL（本机也是）
//	go test ./platform/config/                            → ok（前面有用例替它读了 .env）
//
// CI 上没有 .env，所以那里一直是后一种对照里的红那一半。把 cwd 钉在空目录里，
// 这两种跑法就一致了；用例缺什么变量就自己 t.Setenv 什么——这是它们本来就该有的样子。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "panda-config-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config tests: cannot create a scratch working directory:", err)
		os.Exit(1)
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, "config tests: cannot move out of the repository:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// 本文件里凡是「只想测某一项配置」的用例都拿 gateway-service 当服务名：它是唯一一个
// 既不拥有数据库、也不要求任何地址或令牌的服务名，因此 Load 只会因为那条用例真正关心的
// 东西失败。编一个 "test" 之类的名字不行——Load 对不认识的服务名直接报错。
//
// 这一点每次给某个域服务加必填项时都会被踩到（order-service 加过、lottery-service 加过），
// 所以宁可在这里写死。

func TestLoadRedisDB(t *testing.T) {
	t.Setenv("PANDA_ENV", "test")
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "empty", value: "", want: 0},
		{name: "valid", value: "7", want: 7},
		{name: "negative", value: "-1", wantErr: true},
		{name: "invalid", value: "redis", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REDIS_DB", tt.value)
			cfg, err := Load("gateway-service")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected configuration error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RedisDB != tt.want {
				t.Fatalf("RedisDB = %d, want %d", cfg.RedisDB, tt.want)
			}
		})
	}
}

func TestLoadReadsMerchantServiceURL(t *testing.T) {
	t.Setenv("PANDA_ENV", "test")
	t.Setenv("MERCHANT_INTERNAL_TOKEN", "test-token-which-is-at-least-32-bytes-long")
	t.Setenv("MERCHANT_SERVICE_URL", "http://merchant.test:8080")
	cfg, err := Load("gateway-service")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MerchantServiceURL != "http://merchant.test:8080" {
		t.Fatalf("MerchantServiceURL = %q", cfg.MerchantServiceURL)
	}
}

// REGISTRY_ENDPOINT and the deprecated ETCD_ENDPOINTS both name the etcd cluster.
// The two disagreeing was a real defect, not a cosmetic one: the deploy template
// shipped the old name while the code read only the new one, so a stack that
// configured etcd correctly still ran on the no-op registry — services never
// registered and discovery never resolved, with nothing to indicate why.
// Whichever name carries the value, the service has to see it.
func TestLoadResolvesRegistryEndpoint(t *testing.T) {
	t.Setenv("PANDA_ENV", "test")
	t.Setenv("MERCHANT_INTERNAL_TOKEN", "test-token-which-is-at-least-32-bytes-long")
	tests := []struct {
		name       string
		registry   string
		legacyEtcd string
		want       string
	}{
		{name: "neither set stays empty", registry: "", legacyEtcd: "", want: ""},
		{name: "registry endpoint", registry: "http://etcd:2379", legacyEtcd: "", want: "http://etcd:2379"},
		{name: "registry endpoint is trimmed", registry: "  http://etcd:2379  ", legacyEtcd: "", want: "http://etcd:2379"},
		{name: "legacy etcd endpoints", registry: "", legacyEtcd: "http://legacy:2379", want: "http://legacy:2379"},
		{name: "registry endpoint wins when both are set", registry: "http://etcd:2379", legacyEtcd: "http://legacy:2379", want: "http://etcd:2379"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REGISTRY_ENDPOINT", tt.registry)
			t.Setenv("ETCD_ENDPOINTS", tt.legacyEtcd)
			cfg, err := Load("gateway-service")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RegistryEndpoint != tt.want {
				t.Fatalf("RegistryEndpoint = %q, want %q", cfg.RegistryEndpoint, tt.want)
			}
		})
	}
}

func TestLoadReadsDotEnv(t *testing.T) {
	t.Setenv("PANDA_ENV", "test")
	t.Setenv("MERCHANT_INTERNAL_TOKEN", "test-token-which-is-at-least-32-bytes-long")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("PANDA_ENV=test\nREDIS_ADDR='127.0.0.1:6379'\nREDIS_DB=3\n# comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	previousRedisAddr, hadRedisAddr := os.LookupEnv("REDIS_ADDR")
	previousRedisDB, hadRedisDB := os.LookupEnv("REDIS_DB")
	_ = os.Unsetenv("REDIS_ADDR")
	_ = os.Unsetenv("REDIS_DB")
	t.Cleanup(func() {
		if hadRedisAddr {
			_ = os.Setenv("REDIS_ADDR", previousRedisAddr)
		} else {
			_ = os.Unsetenv("REDIS_ADDR")
		}
		if hadRedisDB {
			_ = os.Setenv("REDIS_DB", previousRedisDB)
		} else {
			_ = os.Unsetenv("REDIS_DB")
		}
	})

	// gateway-service 是唯一一个「不认识就报错」的规则放它过去、又不需要任何别的
	// 必填变量的服务（它不拥有数据库）。这几条用例查的是 .env 的读取本身，
	// 不该被别的必填项挡住。
	cfg, err := Load("gateway-service")
	if err != nil {
		t.Fatal(err)
	}
	// 断言的两个值都来自上面那个临时 .env。这里原本断言的是 cfg.DatabaseURL，而那个
	// 字段已经删了（Config 上不再有共享库这一说）——但这条用例查的从来不是那个字段，
	// 是「.env 里的值有没有被读进来」，换成 REDIS_ADDR 一样成立。
	if cfg.RedisAddress != "127.0.0.1:6379" || cfg.RedisDB != 3 {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestResolveDatabaseReadsTheServiceOwnedURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		service string
		envName string
	}{
		{"user service reads the identity database", "user-service", "USER_DATABASE_URL"},
		{"merchant service reads its own database", "merchant-service", "MERCHANT_DATABASE_URL"},
		{"coupon service reads its own database", "coupon-service", "COUPON_DATABASE_URL"},
		{"coffee machine service reads its own database", "coffee-machine-service", "COFFEE_MACHINE_DATABASE_URL"},
		{"order service reads its own database", "order-service", "ORDER_DATABASE_URL"},
		{"payment service reads its own database", "payment-service", "PAYMENT_DATABASE_URL"},
		{"account service reads its own database", "account-service", "ACCOUNT_DATABASE_URL"},
		{"lottery service reads its own database", "lottery-service", "LOTTERY_DATABASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 共享的那个变量同时设着，且刻意设成另一个值：这条用例要证的就是
			// **它不参与**。以前它会兜住所有缺失的服务，正是要修掉的行为。
			t.Setenv("DATABASE_URL", "shared-that-must-not-be-read")
			t.Setenv(tc.envName, "owned")
			got, err := resolveDatabase(tc.service)
			if err != nil {
				t.Fatalf("resolveDatabase(%q) returned error: %v", tc.service, err)
			}
			if got != "owned" {
				t.Fatalf("resolveDatabase(%q) = %q, want %q", tc.service, got, "owned")
			}
		})
	}
}

// 缺变量必须让 Load 失败，而不是回退。回退到共享 DATABASE_URL 的后果不是「服务起不来」，
// 而是它把自有的表建到身份库里、再从那里读——一个看起来完全正常的错误。
func TestResolveDatabaseRefusesToFallBack(t *testing.T) {
	t.Setenv("DATABASE_URL", "shared-that-must-not-be-read")
	t.Setenv("LOTTERY_DATABASE_URL", "")
	if _, err := resolveDatabase("lottery-service"); err == nil {
		t.Fatal("resolveDatabase(lottery-service) with no LOTTERY_DATABASE_URL returned no error")
	} else if !strings.Contains(err.Error(), "LOTTERY_DATABASE_URL") {
		t.Fatalf("error should name the variable to set, got: %v", err)
	}

	// 全是空白等同于没设：一个只由空格组成的连接串不是连接串。
	t.Setenv("LOTTERY_DATABASE_URL", "   ")
	if _, err := resolveDatabase("lottery-service"); err == nil {
		t.Fatal("a blank LOTTERY_DATABASE_URL was accepted")
	}
}

// 不在表里的服务名报错。新增服务却忘了登记时，这条是唯一的拦网——旧行为是
// 悄悄读共享变量，而那个既不会失败也不会有人发现。
func TestResolveDatabaseRejectsUnknownServices(t *testing.T) {
	if _, err := resolveDatabase("brand-new-service"); err == nil {
		t.Fatal("an unregistered service name was accepted")
	}
}

// Load 这一层也要拦住，而不只是 resolveDatabase：这段逻辑存在的全部意义就是
// 「进程起不来」，一个只测到内部函数的用例证明不了这件事。
//
// 用的就是现实里出过的那种配置：lottery-service 的必填项全配齐，只有
// LOTTERY_DATABASE_URL 没设（本机 .env 到今天就是这个样子）。
func TestLoadRefusesAServiceWithNoDatabase(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("REDIS_DB", "")
	t.Setenv("PANDA_ENV", "test")
	t.Setenv("USER_GRPC_ADDR", "127.0.0.1:19081")
	t.Setenv("ACCOUNT_GRPC_ADDR", "127.0.0.1:19098")
	t.Setenv("MERCHANT_INTERNAL_TOKEN", "test-only-internal-credential-32-bytes")
	t.Setenv("LOTTERY_DATABASE_URL", "")
	// 共享的那份设着：它正是以前会兜住这个缺口的东西。
	t.Setenv("DATABASE_URL", "postgres://localhost/panda_identity")

	_, err := Load("lottery-service")
	if err == nil {
		t.Fatal("lottery-service started with no LOTTERY_DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "LOTTERY_DATABASE_URL") {
		t.Fatalf("the error should name the variable to set, got: %v", err)
	}

	// 补上就能起来——否则上面那条通过的可能是别的原因。
	t.Setenv("LOTTERY_DATABASE_URL", "postgres://localhost/panda_lottery")
	cfg, err := Load("lottery-service")
	if err != nil {
		t.Fatalf("lottery-service should start once its database is set: %v", err)
	}
	if cfg.ServiceDatabaseURL != "postgres://localhost/panda_lottery" {
		t.Fatalf("ServiceDatabaseURL = %q", cfg.ServiceDatabaseURL)
	}
}

// 不拥有数据库的服务是**列出来**的，不是「不在表里」推出来的。
func TestResolveDatabaseAllowsServicesThatOwnNoDatabase(t *testing.T) {
	got, err := resolveDatabase("gateway-service")
	if err != nil {
		t.Fatalf("gateway-service should be allowed to own no database: %v", err)
	}
	if got != "" {
		t.Fatalf("gateway-service = %q, want empty", got)
	}
}

func TestLoadEnvironmentOverridesDotEnv(t *testing.T) {
	t.Setenv("PANDA_ENV", "test")
	t.Setenv("MERCHANT_INTERNAL_TOKEN", "test-token-which-is-at-least-32-bytes-long")
	t.Setenv("MERCHANT_SERVICE_URL", "http://merchant.test:8080")
	dir := t.TempDir()
	// .env 与进程环境给**同一个键**不同的值，读到的必须是环境里那个。这里原本用的是
	// DATABASE_URL，字段删掉之后换 MERCHANT_SERVICE_URL：上面那条 t.Setenv 本来就在设它，
	// 只是从来没被断言过，等于白设。
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MERCHANT_SERVICE_URL=http://file-value:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	cfg, err := Load("gateway-service")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MerchantServiceURL != "http://merchant.test:8080" {
		t.Fatalf("MerchantServiceURL = %q, .env 把进程环境里的值盖掉了", cfg.MerchantServiceURL)
	}
}

func TestLoadRejectsInvalidDotEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("not-an-assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	// 同样用 gateway-service：这条要证的失败**只能**来自 .env 的解析，
	// 用一个不认识的服务名会让它因为别的原因通过。
	if _, err := Load("gateway-service"); err == nil {
		t.Fatal("expected .env parsing error")
	}
}
