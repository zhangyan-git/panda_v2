// payment-service 拥有支付域的事实（方案 5.9）：支付方式与渠道的路由、支付单、资金行、
// 渠道流水、回调通知、记账流水。资金的**真相**只有一处——这一张 payments 表，以及它下面
// 那些资金行；订单服务不复制它，它只收 `payment.succeeded` / `payment.failed` 两条事件。
//
// 它对外有两个入口，两个方向完全不同的口子：
//
//   - **入**：渠道回调 POST /v1/payments/callback/{channelCode}。这条**不挂认证**——渠道
//     没有我们的令牌，凭据是它自己的签名，验签在 service 里做（见 routes 包的注释）。
//   - **出**：gRPC 的 CreatePayment。order-service 在锁内校验完订单后同步调它，把权威金额
//     交过来；客户端要立刻拿到支付参数（方案 7.2），所以这条路不能走 MQ。
//
// 它还有一个**出网**方向：选中的支付方式 action=account 时，它同步调 account-service 的
// DeductCoffeeBeans 扣豆（见 internal/client）。这条调用也是同步的、也在 PG 事务之外，
// 理由与「渠道调用绝不放在 PG 事务里」逐字相同。
//
// 退款、代扣、对账本轮都不做。那三件事的表已经建好（002），没有代码也没有 worker——写它们
// 需要真实渠道凭据，没有凭据时写出来的只是参数构造，那不是验证，是猜。
package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	platformclient "github.com/panda-dev/panda-v2/backend/platform/client"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/database/migrate"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/manual"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/rpc"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/worker"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
	"github.com/panda-dev/panda-v2/migrations"
)

const (
	// accountServiceDialTimeout 限制启动时建立那条共享连接的时间。取值与 account-service
	// 那条 user-service 连接一致——它管的是启动时把连接拨通，不是每一次调用的超时。
	accountServiceDialTimeout = 5 * time.Second
)

func main() {
	cfg, err := config.Load("payment-service")
	if err != nil {
		log.Fatalf("payment-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("payment-service: init database: %v", err)
	}
	defer db.Close()

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("payment-service: database does not support the payment API")
	}
	// 先迁移再读表：仓储假定表已经存在。生产由发布流程离线迁移，所以这里默认关闭，只有
	// DB_MIGRATE_ON_START 打开时才跑。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.Payment); err != nil {
			log.Fatalf("payment-service: migrate: %v", err)
		}
	}

	// 仓储带 recorder：支付状态的每一次人工介入都要留痕（方案 11.6）。渠道回调那条路不带
	// 人工干预，它自己的留痕是 payment_notifications 与 payment_state_transitions。
	paymentRepo := repository.NewPostgresRepository(pool.Pool(), audit.NewRecorder())

	// 渠道适配器在这里注册，之后只读。本轮只有手工渠道——它做真的 HMAC 验签，所以
	// 「验签失败绝不改支付状态」与「重放被唯一键挡住」这两条是真的被验到了，不是靠桩。
	//
	// 接一家真实渠道 = 写一个实现 provider.Provider 的包 + 在这一行加一个构造 + 插两行数据
	// （payment_channels、payment_methods）。service、controller、表约束一行都不用动。
	providers := provider.NewRegistry(
		manual.New(),
	)

	// 账户域是**同步**依赖：纯豆出资要当场扣余额，扣不了就不能把支付单推进成功。注册中心
	// 未启用发现时回落到静态地址；config.Load 已经拒绝了空地址——空地址会让每一次豆支付都
	// 在运行期变成 5xx，与 account-service 对 USER_GRPC_ADDR 的那条理由逐字相同。
	reg := registry.New(cfg.RegistryEndpoint)
	accountConn, err := platformclient.Dial(context.Background(), "account-service", cfg.AccountGRPCAddress, accountServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("payment-service: dial account-service: %v", err)
	}
	defer accountConn.Close()
	// 令牌用 MERCHANT_INTERNAL_TOKEN：与下面 GRPCServerOptions 里验的那一枚、以及
	// account-service 那边 auth.RequireService 认的那一枚是同一个（见那一处的说明）。
	beans := client.NewCoffeeBeanClient(accountConn, cfg.MerchantInternalToken)

	payments := service.New(paymentRepo, providers, secretResolver, beans, service.Options{
		// 回调基址从配置读，不由渠道配置决定：同一个渠道在 dev / staging / prod 的域名不同，
		// 写进 payment_channels.config 就得多一套配置。config.Load 已经拒绝了空值——拼出
		// 一个相对路径发给渠道的后果是回调永远到不了（见 PaymentNotifyBaseURL 的注释）。
		NotifyBaseURL: cfg.PaymentNotifyBaseURL,
	})

	callback := controller.NewCallbackController(payments)
	// 超时关单必须有人做：没有它，到点未支付的单会一直停在待支付，而渠道那边那张预支付单
	// 还有效——用户能在一个我们以为该作废的收银页上把钱付了。周期与批量用默认值。
	expiryWorker := worker.NewExpiryWorker(payments, worker.DefaultSweepInterval, worker.DefaultSweepBatch)

	// access token 24 小时，与 user-service / merchant-service / coupon-service /
	// coffee-machine-service / order-service 保持一致（见那几处的说明）。六处 auth.NewService
	// 的取值必须一致。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("payment-service: init jwt: %v", err)
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// Outbox：每一次支付成功/失败都在业务事务里追加一条事件，relay 投给 order-service。
		// 它必须与状态改动在同一个事务里——支付状态改了而事件丢了，订单就永远停在
		// pending_payment，而钱已经收了。这正是 outbox 而不是「改完再发」的理由。
		Outbox:  messaging.NewPostgreSQL(pool.Pool()),
		Workers: []runtime.Runner{expiryWorker},
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			routes.RegisterPayment(r, callback)
		},
		GRPCRoutes: func(s *kgrpc.Server) {
			paymentv1.RegisterPaymentServiceServer(s, rpc.NewPaymentService(payments))
		},
		// 服务端验服务令牌：这个 gRPC 面只对内部开放（目前只有 order-service 调）。沿用
		// MERCHANT_INTERNAL_TOKEN——它在这个仓库里已经是「服务之间互认」的那一枚，user-service、
		// merchant-service、coffee-machine-service 三个方向都用它（变量名带着 merchant 是历史，
		// 改名要几个服务一起动，不在这次改动里做）。
		GRPCServerOptions: []kgrpc.ServerOption{
			auth.GRPCServerOption(jwtService, cfg.MerchantInternalToken),
		},
	}); err != nil {
		log.Fatalf("payment-service: %v", err)
	}
}

// secretResolver 把渠道行上的 secret_ref 解析成密钥本身。
//
// 从环境变量读、**读不到就返回空串**：空串在 provider 那边是「验不了签」，一律拒。这是
// 有意的——绝不能有「解不出密钥就跳过验签」这种降级，那等于没有防重放。
//
// 做成一个函数而不是让适配器自己去读环境：适配器因此是可测的纯函数（见 provider.Method
// 的注释），密钥从哪来是装配处的事，换了 Secret 系统也只改这一行。
func secretResolver(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	return os.Getenv(ref)
}
