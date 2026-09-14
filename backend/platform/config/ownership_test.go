package config

import (
	"strings"
	"testing"
)

// TestAuthorizationTimeoutConfiguration covers AUTHZ_TIMEOUT_MS. The value is
// only read by the services that authorize live, and it must stay bounded: too
// short turns a blip into an outage, too long leaves a stuck lookup holding the
// request.
func TestAuthorizationTimeoutConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("REDIS_DB", "")
	tests := []struct {
		name, service, env, timeout string
		want                        int
		wantErr                     bool
	}{
		{name: "coupon default", service: "coupon-service", env: "test", want: 2000},
		{name: "coupon minimum", service: "coupon-service", env: "test", timeout: "500", want: 500},
		{name: "coupon maximum", service: "coupon-service", env: "test", timeout: "30000", want: 30000},
		{name: "coupon too short", service: "coupon-service", env: "test", timeout: "499", wantErr: true},
		{name: "coupon too long", service: "coupon-service", env: "test", timeout: "30001", wantErr: true},
		{name: "coupon non numeric", service: "coupon-service", env: "test", timeout: "soon", wantErr: true},
		// A stack that sets this for every service must not break the services
		// that have no live authorization to bound: the invalid value is ignored
		// and the code default stands, exactly like MERCHANT_OWNERSHIP_TIMEOUT_MS.
		// gateway-service is the stand-in here because it is the one service that
		// requires nothing at all; a service that acquires a requirement (as
		// order-service did when it started asking for live grants) stops being a
		// valid example for this case.
		{name: "unrelated service ignores it", service: "gateway-service", env: "test", timeout: "invalid", want: 2000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PANDA_ENV", tt.env)
			t.Setenv("AUTHZ_TIMEOUT_MS", tt.timeout)
			if tt.service == "coupon-service" {
				t.Setenv("USER_GRPC_ADDR", "127.0.0.1:19081")
			}
			cfg, err := Load(tt.service)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if cfg.AuthorizationTimeoutMS != tt.want {
				t.Fatalf("AuthorizationTimeoutMS = %d, want %d", cfg.AuthorizationTimeoutMS, tt.want)
			}
		})
	}
}

func TestOwnershipConfiguration(t *testing.T) {
	// Do not discover the developer's .env or depend on service credentials.
	t.Chdir(t.TempDir())
	t.Setenv("REDIS_DB", "")
	const token = "test-only-internal-credential-32-bytes"
	const userGRPC = "127.0.0.1:19081"
	const merchantGRPC = "127.0.0.1:19082"
	const coffeeMachineGRPC = "127.0.0.1:19085"
	// MERCHANT_SERVICE_URL is intentionally never set below: user-service reaches
	// merchant-service over gRPC now, so the HTTP URL belongs to the gateway only.
	tests := []struct {
		name, service, env, timeout, userGRPC, merchantGRPC, coffeeMachineGRPC, token string
		wantErr                                                                       bool
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
		// coupon-service asks user-service for live grants on every admin request,
		// so it needs the address but never the service token: that token is for
		// the infrastructure RPCs coupon-service does not call.
		{name: "coupon needs user gRPC address", service: "coupon-service", env: "production", wantErr: true},
		{name: "coupon needs no service token", service: "coupon-service", env: "production", userGRPC: userGRPC},
		// order-service asks user-service for live grants (like coupon-service) and
		// coffee-machine-service for the device facts a drink order is validated
		// against, presenting the shared service token for the second one. Both
		// addresses and the token are required, and each case below omits exactly one.
		{name: "order needs user gRPC address", service: "order-service", env: "production", coffeeMachineGRPC: coffeeMachineGRPC, token: token, wantErr: true},
		{name: "order needs coffee machine gRPC address", service: "order-service", env: "production", userGRPC: userGRPC, token: token, wantErr: true},
		{name: "order needs the service token", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, wantErr: true},
		{name: "order is fully configured", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, token: token},
		// gateway-service is the stand-in for "a service with no special settings":
		// it owns no database and dials nothing. Do not reuse a domain service name
		// here — each one acquires requirements over time and stops being unrelated.
		{name: "unrelated service needs no merchant settings", service: "gateway-service", env: "production"},
		{name: "unrelated service ignores ownership settings", service: "gateway-service", env: "production", timeout: "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PANDA_ENV", tt.env)
			t.Setenv("MERCHANT_OWNERSHIP_TIMEOUT_MS", tt.timeout)
			t.Setenv("USER_GRPC_ADDR", tt.userGRPC)
			t.Setenv("MERCHANT_GRPC_ADDR", tt.merchantGRPC)
			t.Setenv("COFFEE_MACHINE_GRPC_ADDR", tt.coffeeMachineGRPC)
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
