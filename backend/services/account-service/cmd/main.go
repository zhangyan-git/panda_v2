// account-service 拥有资产账户域的两半（方案 5.6）：福卡（余额、不可变流水，以及
// 发放 / 扣减 / 冲正 / 冻结四条写路径）与咖啡豆（余额、不可变流水，以及扣减 / 冲正 /
// 后台人工调整三条写路径）。两半同形，差别在币种（张 vs 分）与词表（豆没有冻结）。
//
// 它的边界要说清楚，因为「账户」这个词很容易被理解得比实际更宽：
//
//   - 它**不拥有发放规则**。一单该送几张、基础送多少、哪个活动加赠多少，是订单域在
//     下单时冻结下来的承诺；order-service 把拆好的每一条发过来，本服务只记账，不解析
//     那份快照、也不重算张数。
//   - 它**不拥有抽奖规则**，也**不拥有支付规则**。扣几张、扣多少分都是调用方给的，
//     本服务只判「够不够扣」。
//
// 所以它的输入有三个：order.completed 事件（发放福卡）、order.after_sale.reviewed
// 事件（审核通过则冲正豆），以及 gRPC——福卡那三个方法今天还没有调用方（lottery 没建），
// 豆的 DeductCoffeeBeans 有真实调用方（payment-service 的纯豆出资）。对外读与后台调整
// 走 HTTP：小程序读自己的，后台按权限码 account:read 读、account:manage 改。
//
// 它**不发下游事件**：余额变化今天没有消费者（lottery 还没建），发出去只会被静默丢弃。
// 但**它开 outbox**，且那是必须的——后台的每一次人工调整都会经 platform/audit 在业务事务内
// 往这里追加一条 admin.operation.logged（方案 11.6 的必审清单），relay 再把它投到身份库的
// admin_operation_logs。不开 outbox，调整就没有留痕的地方。两件事不矛盾：outbox 走的是审计
// 事件，不是余额广播。
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
	accountclient "github.com/panda-dev/panda-v2/backend/services/account-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/rpc"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
	"github.com/panda-dev/panda-v2/migrations"
)

const (
	// userServiceDialTimeout 限制启动时建立那条共享连接的时间。
	userServiceDialTimeout = 5 * time.Second
	// authorizationTimeout 限制单次实时鉴权查询，不是缓存 TTL：每个后台请求都重新查
	// 一次调用方的授权。
	authorizationTimeout = 5 * time.Second
)

func main() {
	cfg, err := config.Load("account-service")
	if err != nil {
		log.Fatalf("account-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("account-service: init database: %v", err)
	}
	defer db.Close()

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("account-service: database does not support the account API")
	}
	// 先迁移再读表：仓储假定表已经存在。生产由发布流程离线迁移，所以这里默认关闭，
	// 只有 DB_MIGRATE_ON_START 打开时才跑。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.Account); err != nil {
			log.Fatalf("account-service: migrate: %v", err)
		}
	}

	accounts := service.New(repository.NewPostgresRepository(pool.Pool()))
	adminCards := controller.NewAdminFortuneCardController(accounts)
	miniappCards := controller.NewMiniappFortuneCardController(accounts)

	// 后台的豆调整走**另一个**仓储，因为它多了审计依赖：读路径（含那两个 gRPC 面）不该
	// 为了一个用不到的 recorder 而多传一个参数。这与 coffee-machine-service 的
	// adminService / adminMasterData 是同一个理由，改动时要一起改。
	beanAdmin := service.NewBeanAdmin(repository.NewBeanAdminRepository(
		repository.NewPostgresRepository(pool.Pool()), audit.NewRecorder()))
	adminBeans := controller.NewAdminCoffeeBeanController(accounts, beanAdmin)
	miniappBeans := controller.NewMiniappCoffeeBeanController(accounts)

	// 一个注册中心、一条共享连接：问 user-service 要后台的实时授权。注册中心未启用
	// 发现时回落到静态地址。config.Load 已经拒绝了空地址——本服务唯一的后台入口是
	// 福卡查询，而它每一次都要过实时授权，空地址会在第一次查询时才炸。
	reg := registry.New(cfg.RegistryEndpoint)
	userConn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("account-service: dial user-service: %v", err)
	}
	defer userConn.Close()
	authorizer := accountclient.NewAdminAccessResolver(userConn)

	// access token 24 小时，与 user-service / merchant-service / coupon-service /
	// coffee-machine-service / order-service / payment-service 保持一致（见那几处的
	// 说明）。七处 auth.NewService 的取值必须一致。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("account-service: init jwt: %v", err)
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// ConsumerInbox：order.completed 会重投（broker 重连、处理完但没 ack 时崩溃
		// 都会），而发放是要写余额的。没有它，同一次完成会被应用两遍。
		// 第二道防线是 entry_key 的唯一索引——两道都要有：inbox 挡住重复投递，
		// entry_key 挡住「事件真的被投了两次」（换过 event_id 的重投骗不过它）。
		ConsumerInbox:   messaging.NewPostgreSQL(pool.Pool()),
		ConsumerHandler: accounts.HandleOrderEvent,
		// 只给 outbox：供给它，runtime 才会起 relay 把事件投出去。
		//
		// 本服务的 outbox 不是可选件：后台的每一次豆调整都会经 platform/audit 在业务事务内
		// 往这里追加一条 admin.operation.logged，其中余额调整属于方案 11.6 的必审清单。
		// relay 投到 RabbitMQ、再落到身份库的 admin_operation_logs。没有它，调整就没有
		// 留痕的地方——写进去的行会永远躺在本地库里没人投。
		//
		// 它**不是**余额广播：豆与福卡的余额变化仍然不发下游事件（没有消费者），见包注释。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		// Workers 为空：账户域没有到点要做的动作。账户行是懒创建的，两半都没有「过期」
		// 这个概念——福卡不过期（原型也没给过期时间），豆永不过期（已拍板）。
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			// 后台：认证 → 平台账号闸门 → 实时授权 → 权限码（读 account:read，
			// 豆调整 account:manage）。
			routes.RegisterAdmin(r, adminCards, adminBeans, adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
			// 小程序：只套认证。C 端的授权不是权限码能表达的（见 routes.RegisterMiniapp），
			// realm 与归属由 controller 判定。
			routes.RegisterMiniapp(r, miniappCards, miniappBeans, auth.Middleware(jwtService))
		},
		GRPCRoutes: func(s *kgrpc.Server) {
			accountv1.RegisterFortuneCardServiceServer(s, rpc.NewFortuneCardService(accounts))
			accountv1.RegisterCoffeeBeanServiceServer(s, rpc.NewCoffeeBeanService(accounts))
		},
		// 服务端验服务令牌：这个 gRPC 面只对内部开放（将来的 lottery-service、退款单）。
		// 沿用 MERCHANT_INTERNAL_TOKEN——它在这个仓库里已经是「服务之间互认」的那一枚，
		// user-service、merchant-service、coffee-machine-service、payment-service 四个
		// 方向都用它（变量名带着 merchant 是历史，改名要几个服务一起动，不在这次改动里做）。
		GRPCServerOptions: []kgrpc.ServerOption{
			auth.GRPCServerOption(jwtService, cfg.MerchantInternalToken),
		},
	}); err != nil {
		log.Fatalf("account-service: %v", err)
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
