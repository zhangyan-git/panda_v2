package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
			cfg, err := Load("test")
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
	cfg, err := Load("test")
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
			cfg, err := Load("test")
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
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("PANDA_ENV=test\nDATABASE_URL='postgres://localhost/panda'\nREDIS_DB=3\n# comment\n"), 0o600); err != nil {
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
	previousDatabaseURL, hadDatabaseURL := os.LookupEnv("DATABASE_URL")
	previousRedisDB, hadRedisDB := os.LookupEnv("REDIS_DB")
	_ = os.Unsetenv("DATABASE_URL")
	_ = os.Unsetenv("REDIS_DB")
	t.Cleanup(func() {
		if hadDatabaseURL {
			_ = os.Setenv("DATABASE_URL", previousDatabaseURL)
		} else {
			_ = os.Unsetenv("DATABASE_URL")
		}
		if hadRedisDB {
			_ = os.Setenv("REDIS_DB", previousRedisDB)
		} else {
			_ = os.Unsetenv("REDIS_DB")
		}
	})

	cfg, err := Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://localhost/panda" || cfg.RedisDB != 3 {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestResolveDatabasePrefersTheServiceOwnedURL(t *testing.T) {
	for _, tc := range []struct {
		name                                                          string
		service, shared, user, merchant, coupon, coffeeMachine, order string
		want                                                          string
	}{
		{"user service reads its own database", "user-service", "shared", "identity", "merchant", "coupon", "coffee", "order", "identity"},
		{"merchant service reads its own database", "merchant-service", "shared", "identity", "merchant", "coupon", "coffee", "order", "merchant"},
		{"coupon service reads its own database", "coupon-service", "shared", "identity", "merchant", "coupon", "coffee", "order", "coupon"},
		{"coffee machine service reads its own database", "coffee-machine-service", "shared", "identity", "merchant", "coupon", "coffee", "order", "coffee"},
		{"order service reads its own database", "order-service", "shared", "identity", "merchant", "coupon", "coffee", "order", "order"},
		{"user service falls back when unset", "user-service", "shared", "", "merchant", "coupon", "coffee", "order", "shared"},
		{"merchant service falls back when unset", "merchant-service", "shared", "identity", "", "coupon", "coffee", "order", "shared"},
		{"coupon service falls back when unset", "coupon-service", "shared", "identity", "merchant", "", "coffee", "order", "shared"},
		{"coffee machine service falls back when unset", "coffee-machine-service", "shared", "identity", "merchant", "coupon", "", "order", "shared"},
		{"order service falls back when unset", "order-service", "shared", "identity", "merchant", "coupon", "coffee", "", "shared"},
		{"blank override is not an override", "user-service", "shared", "  ", "", "", "", "", "shared"},
		{"a service without an owned database stays shared", "gateway-service", "shared", "identity", "merchant", "coupon", "coffee", "order", "shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDatabase(tc.service, tc.shared, tc.user, tc.merchant, tc.coupon, tc.coffeeMachine, tc.order); got != tc.want {
				t.Fatalf("resolveDatabase(%q, %q, %q, %q, %q, %q, %q) = %q, want %q",
					tc.service, tc.shared, tc.user, tc.merchant, tc.coupon, tc.coffeeMachine, tc.order, got, tc.want)
			}
		})
	}
}

func TestLoadEnvironmentOverridesDotEnv(t *testing.T) {
	t.Setenv("PANDA_ENV", "test")
	t.Setenv("MERCHANT_INTERNAL_TOKEN", "test-token-which-is-at-least-32-bytes-long")
	t.Setenv("MERCHANT_SERVICE_URL", "http://merchant.test:8080")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DATABASE_URL=file-value\n"), 0o600); err != nil {
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
	t.Setenv("DATABASE_URL", "environment-value")

	cfg, err := Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "environment-value" {
		t.Fatalf("DatabaseURL = %q", cfg.DatabaseURL)
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

	if _, err := Load("test"); err == nil {
		t.Fatal("expected .env parsing error")
	}
}
