package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
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

	batchRepo, couponRepo, idempotencyRepo := repository.NewPostgresRepository(pool.Pool(), audit.NewRecorder())
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
		// Outbox：后台发放的每一批券都在业务事务里追加一条 coupon.issued，与券、批次、库存
		// 流水同生共死。relay 没有 publisher 时事件留在表里等重启再投。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		// ConsumerInbox：本服务从 2026-09 起是**会员域事件的消费方**（会员价券与店铺码活动券）。
		// 这两条链路都会重投（broker 重连、处理完但没 ack 时崩溃），而发券是要写账的——没有它，
		// 同一条消息会发出第二批券，而券收回来只能靠逐张作废。
		//
		// 仓储那边另有幂等（coupon_idempotency_keys，键从业务凭据派生），两层各挡一种情况：
		// inbox 挡「同一条事件重投」，幂等键挡「同一笔业务被两条不同的事件带来」。**只留一层
		// 是不够的**——后者在事件 id 变了(重放时)时照样能挡住。
		ConsumerInbox: messaging.NewPostgreSQL(pool.Pool()),
		// 订阅哪些事件由 RABBITMQ_ROUTING_KEY（逗号分隔）决定，不认识的一律 ack（见
		// service.HandleEvent）。本服务不发券给「会员到期」「会员被撤销」这两种事件。
		ConsumerHandler: couponService.HandleEvent,
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
