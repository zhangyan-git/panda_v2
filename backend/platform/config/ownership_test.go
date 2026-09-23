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
	for _, prefix := range []string{"USER", "MERCHANT", "COUPON", "COFFEE_MACHINE", "ORDER", "PAYMENT", "ACCOUNT", "LOTTERY", "MEMBERSHIP", "PARTNER"} {
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
	// payment-service also refuses to start without its own public callback base, and
	// that guard has nothing to do with ownership. Left unset, every payment row below
	// fails on it instead of on the thing it names — "payment needs account gRPC
	// address" would pass while testing nothing. Same reasoning as setOwnedDatabaseURLs.
	t.Setenv("PAYMENT_NOTIFY_BASE_URL", "http://gateway.test:8080")
	// And the same for partner-service's master key: its guard is the other thing that
	// would fire before any of the ownership cases below. Presence only, for the reason
	// above.
	t.Setenv("PARTNER_SECRET_KEY", "configured-for-the-ownership-cases")
	const token = "test-only-internal-credential-32-bytes"
	const userGRPC = "127.0.0.1:19081"
	const merchantGRPC = "127.0.0.1:19082"
	const coffeeMachineGRPC = "127.0.0.1:19085"
	const paymentGRPC = "127.0.0.1:19086"
	const membershipGRPC = "127.0.0.1:19089"
	const accountGRPC = "127.0.0.1:19087"
	const orderGRPC = "127.0.0.1:19090"
	// MERCHANT_SERVICE_URL is intentionally never set below: user-service reaches
	// merchant-service over gRPC now, so the HTTP URL belongs to the gateway only.
	tests := []struct {
		name, service, env, timeout, userGRPC, merchantGRPC, coffeeMachineGRPC, paymentGRPC, membershipGRPC, accountGRPC, orderGRPC, token string
		wantErr                                                                                                                            bool
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
		// orchestrates the pay flow (plan 7.2), so it dials payment-service as well,
		// and it reads the membership plan a membership line sells from
		// membership-service — that price and duration cannot come from the client
		// (they decide what the buyer gets for what they paid). And, since 纯豆出资
		// landed, it dials account-service too: a bean-funded order is settled in the
		// bean ledger, which this service does not own.
		// Every address and the token are required, and each case below omits exactly one.
		{name: "order needs user gRPC address", service: "order-service", env: "production", coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, membershipGRPC: membershipGRPC, accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "order needs coffee machine gRPC address", service: "order-service", env: "production", userGRPC: userGRPC, paymentGRPC: paymentGRPC, membershipGRPC: membershipGRPC, accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "order needs payment gRPC address", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, membershipGRPC: membershipGRPC, accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "order needs membership gRPC address", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "order needs account gRPC address", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, membershipGRPC: membershipGRPC, token: token, wantErr: true},
		{name: "order needs the service token", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, membershipGRPC: membershipGRPC, accountGRPC: accountGRPC, wantErr: true},
		{name: "order is fully configured", service: "order-service", env: "production", userGRPC: userGRPC, coffeeMachineGRPC: coffeeMachineGRPC, paymentGRPC: paymentGRPC, membershipGRPC: membershipGRPC, accountGRPC: accountGRPC, token: token},
		// payment-service presents the shared service token when verifying the pay
		// request order-service sends it, dials account-service for 纯豆出资, and —
		// since the admin slice landed — dials user-service to authorize every admin
		// route live (payment:read, see routes/admin.go). Three requirements, and the
		// three wantErr cases below are exactly that set.
		{name: "payment needs user gRPC address", service: "payment-service", env: "production", accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "payment needs the service token", service: "payment-service", env: "production", userGRPC: userGRPC, accountGRPC: accountGRPC, wantErr: true},
		{name: "payment needs account gRPC address", service: "payment-service", env: "production", userGRPC: userGRPC, token: token, wantErr: true},
		{name: "payment is fully configured", service: "payment-service", env: "production", userGRPC: userGRPC, accountGRPC: accountGRPC, token: token},
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
		//
		// It also dials merchant-service, and that one is not decoration: 抽奖 stores a
		// store's id and nothing else (migrations/lottery), so the store's name is
		// resolved live for every listing and the store's existence is checked before an
		// activation is accepted. Each case below omits exactly one requirement.
		{name: "lottery needs user gRPC address", service: "lottery-service", env: "production", merchantGRPC: merchantGRPC, accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "lottery needs account gRPC address", service: "lottery-service", env: "production", userGRPC: userGRPC, merchantGRPC: merchantGRPC, token: token, wantErr: true},
		{name: "lottery needs merchant gRPC address", service: "lottery-service", env: "production", userGRPC: userGRPC, accountGRPC: accountGRPC, token: token, wantErr: true},
		{name: "lottery needs the service token", service: "lottery-service", env: "production", userGRPC: userGRPC, merchantGRPC: merchantGRPC, accountGRPC: accountGRPC, wantErr: true},
		{name: "lottery is fully configured", service: "lottery-service", env: "production", userGRPC: userGRPC, merchantGRPC: merchantGRPC, accountGRPC: accountGRPC, token: token},
		// membership-service has coupon-service's console shape (authorize live on every
		// admin route: membership:read / manage / adjust, and the one RPC it calls
		// carries the caller's own access token), plus one requirement that shape does
		// not have: it is a *verifier* of the shared service token, because
		// order-service and coffee-machine-service ask it for 会员价资格 over gRPC.
		// An unset token there does not fail loudly at startup — the interceptor just
		// answers UNAUTHENTICATED forever — so it is required here instead.
		//
		// membership also dials user-service as a *caller* now — it reads the
		// miniapp openid that signs a payment agreement — but that is the same
		// address user-service is already required for, so it adds no case below.
		// payment-service is the fourth requirement and it is new: 连续包月的签约
		// (发起 / 回渠道核 / 解约) lives there, and without it the whole subscription
		// path is dead.
		//
		// order-service is membership's fifth requirement and it is the newest: 一期代扣成功
		// 之后要先在订单域记一张单，再在本库续这一期会员（券的发放判据就是那个订单号）。
		// 缺了它，钱在渠道扣走了而这一期记不成账——那条事件重投到死信为止。
		{name: "membership needs user gRPC address", service: "membership-service", env: "production", merchantGRPC: merchantGRPC, paymentGRPC: paymentGRPC, orderGRPC: orderGRPC, token: token, wantErr: true},
		{name: "membership needs merchant gRPC address", service: "membership-service", env: "production", userGRPC: userGRPC, paymentGRPC: paymentGRPC, orderGRPC: orderGRPC, token: token, wantErr: true},
		{name: "membership needs payment gRPC address", service: "membership-service", env: "production", userGRPC: userGRPC, merchantGRPC: merchantGRPC, orderGRPC: orderGRPC, token: token, wantErr: true},
		{name: "membership needs order gRPC address", service: "membership-service", env: "production", userGRPC: userGRPC, merchantGRPC: merchantGRPC, paymentGRPC: paymentGRPC, token: token, wantErr: true},
		{name: "membership needs the service token", service: "membership-service", env: "production", userGRPC: userGRPC, merchantGRPC: merchantGRPC, paymentGRPC: paymentGRPC, orderGRPC: orderGRPC, wantErr: true},
		{name: "membership is fully configured", service: "membership-service", env: "production", userGRPC: userGRPC, merchantGRPC: merchantGRPC, paymentGRPC: paymentGRPC, orderGRPC: orderGRPC, token: token},
		// partner-service authorizes live on every admin route (partner:read /
		// partner:manage) plus membership-service's verifier shape
		// turned around — it is a *caller* of membership-service's gRPC face, so it
		// presents the shared service token rather than checking it. Its own master key is
		// a fourth requirement, satisfied by the t.Setenv above. Each case below omits
		// exactly one requirement.
		//
		// It asks for neither order-service's nor coupon-service's address: the 订单查询 /
		// 优惠券发放 forwarders have no RPC to call yet (those two protos are still empty
		// placeholders), so requiring their addresses would be a configuration line nobody
		// can explain. Those two land here when the RPCs land.
		{name: "partner needs user gRPC address", service: "partner-service", env: "production", membershipGRPC: membershipGRPC, token: token, wantErr: true},
		{name: "partner needs membership gRPC address", service: "partner-service", env: "production", userGRPC: userGRPC, orderGRPC: orderGRPC, token: token, wantErr: true},
		// 设备刷卡回调（方案 §四）在 partner-service 验完签之后要经 gRPC 把订单记到
		// order-service。缺了它那条回调建不出单，而钱已经在机器上收过了。
		{name: "partner needs order gRPC address", service: "partner-service", env: "production", userGRPC: userGRPC, membershipGRPC: membershipGRPC, token: token, wantErr: true},
		{name: "partner needs the service token", service: "partner-service", env: "production", userGRPC: userGRPC, membershipGRPC: membershipGRPC, orderGRPC: orderGRPC, wantErr: true},
		{name: "partner is fully configured", service: "partner-service", env: "production", userGRPC: userGRPC, membershipGRPC: membershipGRPC, orderGRPC: orderGRPC, token: token},
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
			t.Setenv("MEMBERSHIP_GRPC_ADDR", tt.membershipGRPC)
			t.Setenv("ACCOUNT_GRPC_ADDR", tt.accountGRPC)
			t.Setenv("ORDER_GRPC_ADDR", tt.orderGRPC)
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
