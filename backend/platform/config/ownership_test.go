package config

import (
	"strings"
	"testing"
)

func TestOwnershipConfiguration(t *testing.T) {
	// Do not discover the developer's .env or depend on service credentials.
	t.Chdir(t.TempDir())
	t.Setenv("REDIS_DB", "")
	const token = "test-only-internal-credential-32-bytes"
	const userGRPC = "127.0.0.1:19081"
	const merchantGRPC = "127.0.0.1:19082"
	// MERCHANT_SERVICE_URL is intentionally never set below: user-service reaches
	// merchant-service over gRPC now, so the HTTP URL belongs to the gateway only.
	tests := []struct {
		name, service, env, timeout, userGRPC, merchantGRPC, token string
		wantErr                                                    bool
	}{
		{name: "remote default", service: "user-service", env: "production", merchantGRPC: merchantGRPC, token: token},
		{name: "remote missing gRPC address", service: "user-service", env: "production", token: token, wantErr: true},
		{name: "remote missing token", service: "user-service", env: "production", merchantGRPC: merchantGRPC, wantErr: true},
		{name: "remote short token", service: "user-service", env: "production", merchantGRPC: merchantGRPC, token: "short", wantErr: true},
		{name: "minimum timeout", service: "user-service", env: "test", timeout: "1000", merchantGRPC: merchantGRPC, token: token},
		{name: "maximum timeout", service: "user-service", env: "test", timeout: "30000", merchantGRPC: merchantGRPC, token: token},
		{name: "timeout too short", service: "user-service", env: "test", timeout: "999", merchantGRPC: merchantGRPC, token: token, wantErr: true},
		{name: "timeout too long", service: "user-service", env: "test", timeout: "30001", merchantGRPC: merchantGRPC, token: token, wantErr: true},
		{name: "timeout zero", service: "user-service", env: "test", timeout: "0", merchantGRPC: merchantGRPC, token: token, wantErr: true},
		{name: "timeout overflow", service: "user-service", env: "test", timeout: "999999999999999999999999", merchantGRPC: merchantGRPC, token: token, wantErr: true},
		{name: "merchant only requires own token", service: "merchant-service", env: "production", userGRPC: userGRPC, token: token},
		{name: "merchant missing user gRPC address", service: "merchant-service", env: "production", token: token, wantErr: true},
		{name: "merchant missing token", service: "merchant-service", env: "production", userGRPC: userGRPC, wantErr: true},
		{name: "unrelated service needs no merchant settings", service: "order-service", env: "production"},
		{name: "unrelated service ignores ownership settings", service: "order-service", env: "production", timeout: "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PANDA_ENV", tt.env)
			t.Setenv("MERCHANT_OWNERSHIP_TIMEOUT_MS", tt.timeout)
			t.Setenv("USER_GRPC_ADDR", tt.userGRPC)
			t.Setenv("MERCHANT_GRPC_ADDR", tt.merchantGRPC)
			t.Setenv("MERCHANT_INTERNAL_TOKEN", tt.token)
			cfg, err := Load(tt.service)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if tt.token != "" && strings.Contains(err.Error(), tt.token) {
					t.Fatal("configuration error exposes credential")
				}
				return
			}
			if tt.timeout == "" && cfg.MerchantOwnershipTimeoutMS != 5000 {
				t.Fatalf("timeout = %d, want 5000", cfg.MerchantOwnershipTimeoutMS)
			}
		})
	}
}
