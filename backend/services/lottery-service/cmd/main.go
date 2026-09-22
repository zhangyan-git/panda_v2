// lottery-service 拥有抽奖域的事实（方案 §5.7）：门店开通、抽奖活动与奖池、期次、参与
// 记录、开奖记录、中奖记录。它把「订单完成 → 发福卡 → 用福卡参与抽奖 → 开奖 → 中奖」
// 这条链闭到底——在此之前，福卡发得出去、扣得掉，但全仓没有一个地方可以花。
//
// # 边界
//
// 福卡余额**不在这个库里**：参与时调 account-service 的 DeductFortuneCards，把返回的
// entry_id 存下来当值引用。门店、咖啡机、订单、用户都只存不透明 UUID 与展示用快照名，
// 不建外键（migrations 的跨库边界测试会拦）。
//
// # 它的两个入口都是 HTTP
//
// 后台一棵树（/v1/admin/lottery/...，认证 → 平台账号闸门 → 实时授权 → 权限码）、小程序
// 一棵树（/v1/miniapp/lottery/...，只套认证）。**没有入向 gRPC**，理由写在 internal/rpc/doc.go
// ——它是唯一一个只出向、不入向的服务。有两条出向的同步调用：扣福卡（account-service），
// 以及问门店（merchant-service：开通前查存在性、读列表时解名字）。
//
// # 本轮不做的
//
// 核销 / 领取 / 换奖整块延后（中奖停在 pending，数据模型与状态机照最终形态建全）；退款
// 追回不接（不配 ConsumerHandler，规则写在迁移与 draw.go 的注释里）；商户端一个字节不动。
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
	"github.com/panda-dev/panda-v2/backend/platform/database/migrate"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/service"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/worker"
	"github.com/panda-dev/panda-v2/migrations"
)

const (
	// accountServiceDialTimeout 限制启动时建立那条共享连接的时间。取值与 payment-service /
	// order-service 那几条一致——它管的是启动时把连接拨通，不是每一次调用的超时。
	accountServiceDialTimeout = 5 * time.Second
	// userServiceDialTimeout 同理，这条连接给后台的实时鉴权查询用。
	userServiceDialTimeout = 5 * time.Second
	// merchantServiceDialTimeout 同理，这条连接给「门店存在吗」「这几家店叫什么」用。
	merchantServiceDialTimeout = 5 * time.Second
	// authorizationTimeout 限制单次实时鉴权查询，不是缓存 TTL：每个后台请求都重新查一次
	// 调用方的授权。
	authorizationTimeout = 5 * time.Second
)

func main() {
	cfg, err := config.Load("lottery-service")
	if err != nil {
		log.Fatalf("lottery-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("lottery-service: init database: %v", err)
	}
	defer db.Close()

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("lottery-service: database does not support the lottery API")
	}
	// 先迁移再读表：仓储假定表已经存在。生产由发布流程离线迁移，所以这里默认关闭，
	// 只有 DB_MIGRATE_ON_START 打开时才跑。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.Lottery); err != nil {
			log.Fatalf("lottery-service: migrate: %v", err)
		}
	}

	// 一个注册中心、两条共享连接。
	reg := registry.New(cfg.RegistryEndpoint)

	// 后台的实时鉴权。它验的是调用者自己的 access token，所以这条连接上的客户端不带服务
	// 令牌（见 client.AdminAccessResolver）。
	userConn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("lottery-service: dial user-service: %v", err)
	}
	defer userConn.Close()
	authorizer := client.NewAdminAccessResolver(userConn)

	// 账户域是**同步**依赖：一次参与就是当场扣一张福卡，扣不了就不能把这次参与记成成交。
	// 注册中心未启用发现时回落到静态地址；config.Load 已经拒绝了空地址——空地址会让每一次
	// 参与都在运行期变成 503，与 account-service 对 USER_GRPC_ADDR 的那条理由逐字相同。
	accountConn, err := platformclient.Dial(context.Background(), "account-service", cfg.AccountGRPCAddress, accountServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("lottery-service: dial account-service: %v", err)
	}
	defer accountConn.Close()
	// 令牌用 MERCHANT_INTERNAL_TOKEN：它在这个仓库里已经是「服务之间互认」的那一枚，而
	// account-service 那边 auth.RequireService 认的也是它。
	cards := client.NewFortuneCardClient(accountConn, cfg.MerchantInternalToken)

	// 商户域也是同步依赖，但它只在两条路上：开通前问一次「这家店存在吗」（问不到就不受理），
	// 以及每次读列表时解一页门店名（解不到只让名字空着，见 service.resolveStoreNames）。
	// 门店名不落库（migrations/lottery/003），所以这条连接不是可选的装饰。
	merchantConn, err := platformclient.Dial(context.Background(), "merchant-service", cfg.MerchantGRPCAddress, merchantServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("lottery-service: dial merchant-service: %v", err)
	}
	defer merchantConn.Close()
	stores, err := client.NewStoreClient(merchantConn, cfg.MerchantInternalToken, merchantServiceDialTimeout)
	if err != nil {
		log.Fatalf("lottery-service: init store client: %v", err)
	}

	// 仓储带 recorder：开奖与作废是人工介入用户资产归属的动作（方案 §11.6 的人工干预必审
	// 清单），要留痕。参与与回调不需要——那是用户自己的操作。
	lotteryRepo := repository.NewPostgresRepository(pool.Pool(), audit.NewRecorder())
	lotteryService := service.New(lotteryRepo, cards, stores, service.Options{})

	adminLottery := controller.NewAdminLotteryController(lotteryService)
	miniappLottery := controller.NewMiniAppLotteryController(lotteryService)

	// 开奖必须有人做：没有它，达标的期次会永远停在 closed，参与的用户以为自己在等一个结果，
	// 其实没有人在算。修复 worker 同理——它收的是「卡可能已经扣了、参与没进期次」那一条，
	// 没有它用户的福卡就悬在那里。两个都用默认周期与批量。
	drawWorker := worker.NewDrawWorker(lotteryService, worker.DefaultDrawInterval, worker.DefaultDrawBatch)
	repairWorker := worker.NewRepairWorker(lotteryService, worker.DefaultRepairInterval, service.DefaultSweepBatch)

	// access token 24 小时，与其余六个服务保持一致（见那几处的说明）。七处 auth.NewService
	// 的取值必须一致。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("lottery-service: init jwt: %v", err)
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// Outbox：每一次开奖都在业务事务里追加一条 lottery.round.drawn，与中奖记录同生共死。
		// **本轮没有消费者**（谁需要知道开奖了？核销还没做，而退款追回本轮不接），但事件
		// 照样要落——中奖记录的下游动作迟早会出现，而「开奖了却没有留下事件」这件事一旦
		// 发生，补不回来。relay 没有配 publisher 时事件留在表里等重启再投。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		// 不配 ConsumerInbox / ConsumerHandler / RABBITMQ_QUEUE：本服务本轮不消费任何事件。
		// 理由见 internal/rpc/doc.go 与 PANDA_V2_REFACTOR_PLAN.md §7.4 的回写。
		Workers: []runtime.Runner{drawWorker, repairWorker},
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			// 后台：认证 → 平台账号闸门 → 实时授权 → 权限码。
			routes.RegisterAdmin(r, adminLottery, adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
			// 小程序：只套认证。C 端的授权不是权限码能表达的（见 routes.RegisterMiniapp），
			// realm 与归属由 controller 的 requireConsumer 判定。
			routes.RegisterMiniapp(r, miniappLottery, auth.Middleware(jwtService))
		},
		// 没有 GRPCRoutes / GRPCServerOptions：本服务本轮没有入向 gRPC，开一个空的 gRPC
		// 端口只会让注册中心多一个永远不会被调用的实例（见 internal/rpc/doc.go）。
	}); err != nil {
		log.Fatalf("lottery-service: %v", err)
	}
}

// adminAuthorizer 组装每条后台路由都要走的中间件，顺序是：
//
//	认证 → 平台账号闸门 → 实时授权 → 权限码
//
// adminOnly 放在实时查询外面，这样商户 token 在本地就被挡掉，不用付一次 gRPC 往返
// （merchant-service 的 handler.Register 也是这个次序）。实时查询放在 RequirePermission
// 外面，是因为它要先覆盖 token 里的授权快照——那次覆盖正是权限校验读到当前数据而不是
// 过期声明的原因。
//
// 与 order-service / coffee-machine-service 是同一份实现，各服务各留一份：它把四个平台包
// 的中间件按一个具体次序拼起来，而那个次序是**这个服务的安全属性**，不该被一个共享实现
// 的改动悄悄改掉。
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
