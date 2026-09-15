package config

import (
	"strings"
	"testing"
)

// setOwnedDatabaseURLs gives a value to the variable of every service that owns a
// database. resolveDatabase has no fallback — a service whose variable is missing
// refuses to start — so a case that wants to reach some *other* startup guard has
// to satisfy this one first, or it passes for the wrong reason.
//
// Leaving one blank on purpose is a test of its own: see
// TestLoadRefusesAServiceWithNoDatabase.
func setOwnedDatabaseURLs(t *testing.T) {
	t.Helper()
	for _, prefix := range []string{"USER", "MERCHANT", "COUPON", "COFFEE_MACHINE", "ORDER", "PAYMENT", "ACCOUNT", "LOTTERY"} {
		t.Setenv(prefix+"_DATABASE_URL", "postgres://localhost/test-"+strings.ToLower(prefix))
	}
}

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
			setOwnedDatabaseURLs(t)
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
	const paymentGRPC = "127.0.0.1:19086"
	const accountGRPC = "127.0.0.1:19087"
	// MERCHANT_SERVICE_URL is intentionally never set below: user-service reaches
	// merchant-service over gRPC now, so the HTTP URL belongs to the gateway only.
	tests := []struct {
		name, service, env, timeout, userGRPC, merchantGRPC, coffeeMachineGRPC, paymentGRPC, accountGRPC, token string
		wantErr                                                                                                 bool
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
		// against, presenting the shared service token for the second one. It also
		// orchestrates the pay flow (plan 7.2), so it dials payment-service as well.
		// Every address and the token are required, and each case below omits exactly one.
		{name: "order needs user gRPC address", service: "order-service", env: "production", coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, token: token, wantErr: true},
		{name: "order needs coffee machine gRPC address", service: "order-service", env: "production", userGRPC: userGRPC, paymentGRPC: paymentGRPC, token: token, wantErr: true},
		{name: "order needs payment gRPC address", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, token: token, wantErr: true},
		{name: "order needs the service token", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, wantErr: true},
		{name: "order is fully configured", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, token: token},
		// payment-service presents the shared service token when verifying the pay
		// request order-service sends it, and dials nothing of its own: the callback
		// is inbound HTTP and the create path is an inbound RPC.
		{name: "payment needs the service token", service: "payment-service", env: "production", accountGRPC: accountGRPC, wantErr: true},
		{name: "payment needs account gRPC address", service: "payment-service", env: "production", token: token, wantErr: true},
		{name: "payment needs nothing else", service: "payment-service", env: "production", accountGRPC: accountGRPC, token: token},
		// account-service asks user-service for live grants on every admin request
		// (reading 福卡账户与流水 is behind account:read), so it needs that address.
		// It also verifies the shared service token on its gRPC face — the 扣减/冲正
		// RPCs that 抽奖 and 退款 will call — so a token it could never match is a
		// startup error too, exactly like payment-service.
		{name: "account needs user gRPC address", service: "account-service", env: "production", token: token, wantErr: true},
		{name: "account needs the service token", service: "account-service", env: "production", userGRPC: userGRPC, wantErr: true},
		{name: "account is fully configured", service: "account-service", env: "production", userGRPC: userGRPC, token: token},
		// lottery-service is the first caller account-service's 扣减/冲正 RPCs ever had
		// (fortune_card.proto sat unused until now): every 参与 deducts a 福卡 inside
		// the request, so both the address and the token are required, and the admin
		// face authorizes live like every other console-backed service.
		{name: "lottery needs user gRPC address", service: "lottery-service", env: "production", accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "lottery needs account gRPC address", service: "lottery-service", env: "production", userGRPC: userGRPC, token: token, wantErr: true},
		{name: "lottery needs the service token", service: "lottery-service", env: "production", userGRPC: userGRPC, accountGRPC: accountGRPC, wantErr: true},
		{name: "lottery is fully configured", service: "lottery-service", env: "production", userGRPC: userGRPC, accountGRPC: accountGRPC, token: token},
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
			setOwnedDatabaseURLs(t)
			t.Setenv("USER_GRPC_ADDR", tt.userGRPC)
			t.Setenv("MERCHANT_GRPC_ADDR", tt.merchantGRPC)
			t.Setenv("COFFEE_MACHINE_GRPC_ADDR", tt.coffeeMachineGRPC)
			t.Setenv("PAYMENT_GRPC_ADDR", tt.paymentGRPC)
			t.Setenv("ACCOUNT_GRPC_ADDR", tt.accountGRPC)
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
