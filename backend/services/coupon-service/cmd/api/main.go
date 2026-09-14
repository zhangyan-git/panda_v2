package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	platformclient "github.com/panda-dev/panda-v2/backend/platform/client"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	couponclient "github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/service"
)

// userServiceDialTimeout bounds establishing the shared connection at startup.
const userServiceDialTimeout = 5 * time.Second

func main() {
	cfg, err := config.Load("coupon-service")
	if err != nil {
		log.Fatalf("coupon-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("coupon-service: init database: %v", err)
	}
	defer db.Close()
	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("coupon-service: database does not support coupon API")
	}

	batchRepo, couponRepo, idempotencyRepo := repository.NewPostgresRepository(pool.Pool())
	couponService := service.New(batchRepo, couponRepo, idempotencyRepo)
	adminCoupon := controller.NewAdminCouponController(couponService)
	// access token 24 小时：与 user-service / merchant-service 保持一致（见那两处的说明）。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("coupon-service: init jwt: %v", err)
	}

	// 一个注册中心、一条连接供所有实时鉴权查询使用；注册中心未启用发现时回落到静态地址。
	reg := registry.New(cfg.RegistryEndpoint)
	conn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("coupon-service: dial user-service: %v", err)
	}
	defer conn.Close()
	authorizer := couponclient.NewAdminAccessResolver(conn)
	authorizationTimeout := time.Duration(cfg.AuthorizationTimeoutMS) * time.Millisecond

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		Outbox:      messaging.NewPostgreSQL(pool.Pool()),
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			routes.RegisterAdmin(r, adminCoupon, adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
		},
	}); err != nil {
		log.Fatalf("coupon-service: %v", err)
	}
}

// adminAuthorizer builds the middleware every admin coupon route runs through,
// in this order:
//
//	认证 → 平台账号闸门 → 实时授权 → 权限码
//
// adminOnly sits outside the live lookup so a merchant token is refused locally
// instead of costing a gRPC round trip (merchant-service's handler.Register is
// wired the same way). The live lookup sits outside RequirePermission because it
// has to overwrite the identity's grants first — that overwrite is what makes
// the permission check read current data instead of the token's stale snapshot.
func adminAuthorizer(jwtService *auth.Service, authorizer authz.Resolver, timeout time.Duration) func(...string) func(http.Handler) http.Handler {
	authMW := auth.Middleware(jwtService)
	live := authz.Middleware(authorizer, timeout)
	return func(permissions ...string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return authMW(adminOnly(live(auth.RequirePermission(permissions...)(next))))
		}
	}
}

func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.IdentityFromRequest(r)
		if !ok || strings.TrimSpace(identity.Subject) == "" || identity.Tenant != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
