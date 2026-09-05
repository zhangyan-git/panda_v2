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
	t.Setenv("MERCHANT_SERVICE_URL", "http://merchant.test:8080")
	cfg, err := Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MerchantServiceURL != "http://merchant.test:8080" {
		t.Fatalf("MerchantServiceURL = %q", cfg.MerchantServiceURL)
	}
}

func TestLoadReadsDotEnv(t *testing.T) {
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

func TestLoadEnvironmentOverridesDotEnv(t *testing.T) {
	t.Setenv("PANDA_ENV", "test")
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
