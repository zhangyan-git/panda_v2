// coffee-machine-service 拥有设备域的主数据与状态（方案 5.14）：设备厂商及其接入
// 凭据、咖啡机设备与支付方式、饮品与供应关系、设备事件与余额流水。
//
// 出杯任务、出杯回调和重试不在这里，它们在 fulfillment-service（方案 5.11、5.14
// L547）。本服务对出杯链路的全部义务是**被读**：下单时校验设备状态（方案 5.8）。
package main

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	platformclient "github.com/panda-dev/panda-v2/backend/platform/client"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/database/migrate"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/rpc"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/service"
	coffeemachinev1 "github.com/panda-dev/panda-v2/contracts/proto/coffee_machine/v1"
	"github.com/panda-dev/panda-v2/migrations"
)

const (
	// userServiceDialTimeout 限制启动时建立那条共享连接的时间。
	userServiceDialTimeout = 5 * time.Second
	// merchantServiceDialTimeout 同理，只是这条连接给点位校验用。
	merchantServiceDialTimeout = 5 * time.Second
	// authorizationTimeout 限制单次实时鉴权查询，不是缓存 TTL：每个后台请求都重新
	// 查一次调用方的授权。
	authorizationTimeout = 5 * time.Second
	// storeLookupTimeout 限制一次点位校验。它跟着每一次带 storeId 的保存走，所以是
	// 每个请求都要付的往返预算：本地 gRPC 是毫秒级，2 秒足够容忍抖动，又短到不会把
	// 一个卡住的商户服务拖成一次长时间挂起。超时按「没问到」处理——不落库。
	storeLookupTimeout = 2 * time.Second
)

func main() {
	cfg, err := config.Load("coffee-machine-service")
	if err != nil {
		log.Fatalf("coffee-machine-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("coffee-machine-service: init database: %v", err)
	}
	defer db.Close()

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("coffee-machine-service: database does not support the device API")
	}
	// 先迁移再读表：仓储和 gRPC 服务都假定表已经存在。生产由发布流程离线迁移，
	// 所以这里默认关闭，只有 DB_MIGRATE_ON_START 打开时才跑。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.CoffeeMachine); err != nil {
			log.Fatalf("coffee-machine-service: migrate: %v", err)
		}
	}

	// 一个注册中心、两条共享连接：一条问 user-service 要实时授权，一条问
	// merchant-service 要点位事实。注册中心未启用发现时都回落到静态地址。
	reg := registry.New(cfg.RegistryEndpoint)
	conn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("coffee-machine-service: dial user-service: %v", err)
	}
	defer conn.Close()
	authorizer := client.NewAdminAccessResolver(conn)
	// 同一条连接上的另一半：商户请求问的是「这个账号能看见哪些点位」，问的是同一个
	// 服务，但那是另一条 RPC，答案也不是同一种东西。
	merchantAccess := client.NewMerchantAccessResolver(conn)

	merchantConn, err := platformclient.Dial(context.Background(), "merchant-service", cfg.MerchantGRPCAddress, merchantServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("coffee-machine-service: dial merchant-service: %v", err)
	}
	defer merchantConn.Close()
	storeResolver, err := client.NewStoreResolver(merchantConn, cfg.MerchantInternalToken, storeLookupTimeout)
	if err != nil {
		log.Fatalf("coffee-machine-service: init store resolver: %v", err)
	}

	masterData := service.NewMasterDataService(repository.NewPostgresRepository(pool.Pool()))
	adminMasterData := controller.NewAdminMasterDataController(masterData)
	// 扣余额是 gRPC 面上唯一的写口（取货码那条路，方案 §四）。它没有 HTTP 面：调用方
	// 是 order-service，不是人，所以也不经过 AdminService 那套审计。
	deviceBalance := service.NewDeviceBalanceService(repository.NewDeviceBalanceRepository(pool.Pool()))
	// 写路径单独一个仓储，因为它多了审计依赖：读路径（含 gRPC 的 GetDevice）不该
	// 为了一个用不到的 recorder 而多传一个参数。
	adminService := service.NewAdminService(
		repository.NewAdminRepository(pool.Pool(), audit.NewRecorder()), storeResolver)
	adminWrite := controller.NewAdminWriteController(adminService)
	// 商户端的读面与它共用同一个仓储，但走的是另一条服务：范围过滤不是可选参数，
	// 所以它不能是 MasterDataService 上多出来的一个空。
	merchantDevices := controller.NewMerchantDeviceController(
		service.NewMerchantDeviceService(repository.NewPostgresRepository(pool.Pool())))

	// access token 24 小时，与 user-service / merchant-service / coupon-service 保持
	// 一致（见那几处的说明）。四处 auth.NewService 的取值必须一致。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("coffee-machine-service: init jwt: %v", err)
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// 只给 outbox：供给它，runtime 才会起 relay 把事件投出去。
		//
		// 本服务的 outbox 不是可选件：后台的每一次增改都会经 platform/audit 在业务
		// 事务内往这里追加一条 admin.operation.logged，其中余额调整属于方案 11.6
		// L893 必审清单。relay 投到 RabbitMQ、再落到身份库的 admin_operation_logs。
		// 没有它，余额调整就没有留痕的地方。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			routes.RegisterAdmin(r, adminMasterData, adminWrite, adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
			routes.RegisterMerchant(r, merchantDevices, merchantAuthorizer(jwtService, merchantAccess.Resolve, authorizationTimeout))
		},
		GRPCRoutes: func(s *kgrpc.Server) {
			coffeemachinev1.RegisterCoffeeMachineServiceServer(s, rpc.NewCoffeeMachineService(masterData, deviceBalance))
		},
		// 内部 RPC 的共享服务令牌。沿用 MERCHANT_INTERNAL_TOKEN：它在这个仓库里已经
		// 是「服务之间互认」的那一枚，user-service 与 merchant-service 两个方向都用
		// 它。变量名带着 merchant 是历史，改名要四个服务一起动，不在这次改动里做。
		GRPCServerOptions: []kgrpc.ServerOption{auth.GRPCServerOption(jwtService, cfg.MerchantInternalToken)},
	}); err != nil {
		log.Fatalf("coffee-machine-service: %v", err)
	}
}

// adminAuthorizer 组装每条后台路由都要走的中间件，顺序是：
//
//	认证 → 平台账号闸门 → 实时授权 → 权限码
//
// adminOnly 放在实时查询外面，这样商户 token 在本地就被挡掉，不用付一次 gRPC
// 往返（merchant-service 的 handler.Register 也是这个次序）。实时查询放在
// RequirePermission 外面，是因为它要先覆盖 token 里的授权快照——那次覆盖正是权限
// 校验读到当前数据而不是过期声明的原因。
func adminAuthorizer(jwtService *auth.Service, authorizer authz.Resolver, timeout time.Duration) func(...string) func(http.Handler) http.Handler {
	authMW := auth.Middleware(jwtService)
	live := authz.Middleware(authorizer, timeout)
	return func(permissions ...string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return authMW(adminOnly(live(auth.RequirePermission(permissions...)(next))))
		}
	}
}

// merchantAuthorizer 组装商户端路由的中间件，顺序是：认证 → 实时取数据范围。
//
// 到 RequirePermission 为止——商户域没有权限码，授权只由数据范围表达（009 删掉商户
// 角色表之后，商户账号只有范围、没有角色）。也**没有单独的 realm 闸门**：MerchantMiddleware
// 在发起那次 gRPC 之前就先看 realm 与 tenant，非商户 realm 与空 tenant 都在本地被挡成
// 403，再加一层只是把同一条规则写两遍。
func merchantAuthorizer(jwtService *auth.Service, resolve authz.MerchantResolver, timeout time.Duration) func(http.Handler) http.Handler {
	authMW := auth.Middleware(jwtService)
	live := authz.MerchantMiddleware(resolve, timeout)
	return func(next http.Handler) http.Handler {
		return authMW(live(next))
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
