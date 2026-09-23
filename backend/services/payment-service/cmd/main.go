// payment-service 拥有支付域的事实（方案 5.9）：支付单、资金行、渠道流水、回调通知、记账
// 流水。**支付方式与渠道不再是数据**——它们是代码里的常量表（见 internal/catalog），
// 这个服务因此没有「配置支付渠道」这个面。资金的**真相**只有一处——这一张 payments 表，以及它下面
// 那些资金行；订单服务不复制它，它只收 `payment.succeeded` / `payment.failed` 两条事件。
//
// 它对外有三个入口，方向各不相同：
//
//   - **入**：渠道回调 POST /v1/payments/callback/{provider}。这条**不挂认证**——渠道
//     没有我们的令牌，凭据是它自己的签名，验签在 service 里做（见 routes 包的注释）。
//   - **入（后台）**：/v1/admin/*。挂满四层（认证 → 平台账号闸门 → 实时授权 → 权限码），
//     全部挂 payment:read（支付单列表与详情）。**一条写路由都没有**——那是钱的既成事实，
//     见 docs/architecture.md 支付那一段。
//   - **出**：gRPC 的 CreatePayment。order-service 在锁内校验完订单后同步调它，把权威金额
//     交过来；客户端要立刻拿到支付参数（方案 7.2），所以这条路不能走 MQ。
//
// 它还有一个**出网**方向：选中的支付方式 action=account 时，它同步调 account-service 的
// DeductCoffeeBeans 扣豆（见 internal/client）。这条调用也是同步的、也在 PG 事务之外，
// 理由与「渠道调用绝不放在 PG 事务里」逐字相同。
//
// 退款、代扣、对账本轮都不做。那三件事的表已经建好，没有代码也没有 worker——写它们
// 需要真实渠道凭据，没有凭据时写出来的只是参数构造，那不是验证，是猜。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
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
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/ums"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/wechatpay"
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
	// userServiceDialTimeout 限制启动时建立那条共享连接的时间。取值与 membership-service /
	// membership-service / lottery-service 那几条一致。
	userServiceDialTimeout = 5 * time.Second
	// authorizationTimeout 限制单次实时鉴权查询，不是缓存 TTL：每个后台请求都重新查一次
	// 调用方的授权。
	authorizationTimeout = 5 * time.Second
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

	// 支付方式与渠道的**目录**（catalog）。它不是从这里读出来的，是代码里的常量表
	// ——四个支付方式、六个 code、一条银联商务渠道，见那个包的说明。
	//
	// 拼装它要的只有一组部署值（商户号、应用标识）。它们从环境变量来而不是从库里来：
	// 从前运营在后台「建一行、填 15–30 格」，今天部署清单里九行 PAYMENT_UMS_*，其中七行
	// 留空是合法的（见 .env.example）。**两把密钥不在这九行里**，它们按名字取、只在下单
	// 那一次进内存（见 secretResolver）。
	directory := catalog.FromConfig(catalog.Config{
		UMSBaseURL:    cfg.PaymentUMSBaseURL,
		UMSAppID:      cfg.PaymentUMSAppID,
		UMSMID:        cfg.PaymentUMSMID,
		UMSTID:        cfg.PaymentUMSTID,
		UMSSourceCode: cfg.PaymentUMSSourceCode,
		UMSDomainName: cfg.PaymentUMSDomainName,
		UMSSceneType:  cfg.PaymentUMSSceneType,
		UMSMerAppName: cfg.PaymentUMSMerAppName,
		UMSMerAppID:   cfg.PaymentUMSMerAppID,

		// 微信直连委托代扣那组（见 .env.example 的 WECHAT_PAY_*）。同样是账户值与证书路径，
		// 密钥不在其中。
		WeChatPayBaseURL:              cfg.WeChatPayBaseURL,
		WeChatPayAppID:                cfg.WeChatPayAppID,
		WeChatPayMchID:                cfg.WeChatPayMchID,
		WeChatPayCertPath:             cfg.WeChatPayCertPath,
		WeChatPayKeyPath:              cfg.WeChatPayKeyPath,
		WeChatPaySignMiniProgramAppID: cfg.WeChatPaySignMiniProgramAppID,
	})

	// 渠道适配器在这里注册，之后只读。
	//
	// 注册的粒度是**协议族**，不是渠道：一个适配器实现一种报文格式（怎么签、怎么验、
	// 怎么读成功判据），同族的渠道只是配置不同。今天只剩一族：
	//
	//   - ums：银联商务全民付。同一个商户号下的两条协议（小程序支付 POST+JSON+OPEN-BODY-SIG、
	//     H5 支付 GET+参数全拼+OPEN-FORM-PARAM），在收银台上是六个 code（见 catalog）。
	//   - wechatpay：微信直连委托代扣。**XML + MD5 的 APIv2**，与 ums 那份 JSON 报文毫无关系，
	//     而且它不是收单通道——它签的是「以后每个月自动扣」这份授权（见 catalog 里
	//     CodeWechatPapay 与那个包的注释）。
	//
	// 从前这里还注册着 manual / form_md5 / hmac_body / wechat_v3 四族，它们服务的渠道今天
	// 一个都不接，代码留在树里就是死代码，所以删了——真要用时从 git 历史取回。注意
	// wechat_v3（APIv3：JSON + RSA）与这里的 wechatpay（APIv2：XML + MD5）**不是同一个东西**：
	// 委托代扣只在 APIv2 上，所以这不是把删掉的那族加回来。
	//
	// 接一个**新协议族**（比如以后要接的优联）= 写一个实现 provider.Provider 的包 + 在上面
	// catalog 里加一条 Channel 与几条 Method + 在 .env 里加一组变量。service、controller、
	// 路由、表约束一行都不用动——这正是这次收口刻意留住的那个接缝。
	providers := provider.NewRegistry(
		ums.New(),
		wechatpay.New(),
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

	// 后台的实时鉴权，走**另一条**到 user-service 的连接。它验的是调用者自己的 access
	// token，所以这条连接上的客户端不带服务令牌（见 client.AdminAccessResolver）。
	//
	// 为什么不复用上面那条 accountConn：那是去 account-service 的，地址都不一样。
	// 「与 user-service 的连接」在这个服务里今天只有这一条用途，等以后有业务调用再谈共享。
	userConn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("payment-service: dial user-service: %v", err)
	}
	defer userConn.Close()
	authorizer := client.NewAdminAccessResolver(userConn)

	payments := service.New(paymentRepo, directory, providers, secretResolver(), beans, service.Options{
		// 回调基址从配置读，不由渠道配置决定：同一个渠道在 dev / staging / prod 的域名不同，
		// 写进 payment_channels.config 就得多一套配置。config.Load 已经拒绝了空值——拼出
		// 一个相对路径发给渠道的后果是回调永远到不了（见 PaymentNotifyBaseURL 的注释）。
		NotifyBaseURL: cfg.PaymentNotifyBaseURL,
		// 结果页是**可选的**，与上面那条不同：没配时回跳照验签、照回 200，只是不跳转
		// （见 service.Options.ReturnPageURL）。它会失败的地方只有用户体验，不会丢钱，
		// 所以不在 config.Load 里拦。
		ReturnPageURL: cfg.PaymentReturnPageURL,
	})

	callback := controller.NewCallbackController(payments)
	// 结果页回跳的入口。它与 callback 是两个类型（见 controller.ReturnController）：
	// 一个会改支付状态、一个结构上改不了，共用一个类型会把那个差别藏起来。
	returns := controller.NewReturnController(payments)

	// 协议变更通知（签约 / 解约）的入口。第三个回调类类型，理由与上面那条一样：它收的
	// 是另一种报文、改的是另一张表，与 callback 只是**恰好**共用同一个 service 指针。
	agreementNotify := controller.NewAgreementNotifyController(payments)

	// 扣款结果通知（某一期扣到了没有）的入口。第四个回调类类型，理由同上：它改的是
	// payment_agreement_charges 那一行，与协议通知只是**恰好**共用同一套四步骨架。
	//
	// 它与上面那条是**两个地址**，不是一条路上的两个分支：微信那边签约与扣款的 notify_url
	// 本来就分别配（见 controller.AgreementChargeNotifyPath 的注释）。
	agreementChargeNotify := controller.NewAgreementChargeNotifyController(payments)

	// 后台的只读查询服务。**与上面那个 payments 是两个独立的结构体**：写路径那些依赖
	// （providers / secrets / beans）后台读路径一个都不需要，且它自己一行写方法都没有
	// （见 service.AdminQueryRepository）。
	adminQuery := service.NewAdminQueryService(paymentRepo, directory)
	adminPayments := controller.NewAdminPaymentController(adminQuery, directory)

	// 分账后台：规则与账户可改、任务只读。
	//
	// 它是**第三个**后台结构体（前两个是 adminQuery 与 adminPayments），与那两个分开的理由
	// 与它们互相分开的理由是同一条：这一半要的是写方法（它自己的 AdminSettlementRepository
	// 里一个读方法都不少，但读的都是分账那几张表），而支付单那两个结构体一行写路径都不碰。
	// 另起一个也让「分账的写入口只有这一处」在 main.go 里一眼看得见。
	adminSettlement := controller.NewAdminSettlementController(
		service.NewAdminSettlementService(paymentRepo, directory))

	// 超时关单必须有人做：没有它，到点未支付的单会一直停在待支付，而渠道那边那张预支付单
	// 还有效——用户能在一个我们以为该作废的收银页上把钱付了。周期与批量用默认值。
	expiryWorker := worker.NewExpiryWorker(payments, worker.DefaultSweepInterval, worker.DefaultSweepBatch)

	// 主动查单也必须有人做：回调会丢、会被挡在外面、会因为这一侧验签没配好而被拒，而超时关单
	// 只说「我们不再等了」，从不说明那笔钱没收到。没有这个任务，一笔用户真的付了钱、回调却没
	// 进来的支付单就永远停在待支付——老系统能收三年钱靠的正是它（见 service.ReconcilePendingPayments）。
	// 三个参数都用默认值：60 秒一轮、每轮 20 笔、发起满 5 分钟才问。
	reconcileWorker := worker.NewReconcileWorker(payments,
		worker.DefaultReconcileInterval, worker.DefaultReconcileBatch, worker.DefaultReconcileStaleAfter)

	// 退款查询同样必须有人做，而且它比上面那两个更不可省：**退款的回调不存在**——银联商务
	// 对退款是同步应答，规范里没有退款通知这回事。所以一笔退款一旦在应答里是
	// PROCESSING / UNKNOWN，或者那次调用超时了，除了主动去问没有第二条路能拿到结论。没有
	// 这个任务，那些退款单会永远停在 processing、售后单永远停在 refunding，钱退没退回去
	// 没人知道（见 service.ReconcileProcessingRefunds）。
	// 三个参数都用默认值：5 分钟一轮、每轮 20 笔、发起满 5 分钟才问。
	refundQueryWorker := worker.NewRefundQueryWorker(payments,
		worker.DefaultRefundQueryInterval, worker.DefaultRefundQueryBatch, worker.DefaultRefundQueryStaleAfter)

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
		Workers: []runtime.Runner{expiryWorker, reconcileWorker, refundQueryWorker},
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			// 两棵树、两次注册，**顺序无关但要分开写**：回调树故意不挂认证，后台树每条
			// 都挂。合并成一个 Register 会稀释 admin 那条「校验器为 nil 就全 401」的保证
			// （见 routes 的包说明）。
			routes.RegisterPayment(r, callback, returns, agreementNotify, agreementChargeNotify)
			routes.RegisterAdmin(r, adminPayments, adminSettlement,
				adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
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

// adminAuthorizer 是后台路由那四层中间件的组装：认证 → 平台账号闸门 → 实时授权 → 权限码。
//
// 与 membership-service / partner-service / lottery-service / order-service /
// coffee-machine-service 里的同名函数逐字一致，各服务各留一份（理由见
// internal/client/admin_access.go）。**层序不能调**：
//
//   - auth.Middleware 必须最先，它把令牌声明放进请求上下文，后面两层都读它。
//   - adminOnly 在它之后：它判的是「这是不是一个后台账号」，那要看已经解出来的身份。
//   - authz.Middleware 在最后一道，它要拿调用方的**原始令牌串**去问 user-service；
//     放在前面也能跑，但那样「不是后台账号」的请求会先去问一次授权，白花一次 RPC。
//   - RequirePermission 贴在最里面，它读的是上面那层刚取回来的权限列表。
//
// authenticate 传 nil 时 routes 那一侧会全 401（失败关闭），所以这里返回的闭包永远不是
// nil——「忘了装配」的表现是「谁都进不去」，不是「谁都能进」。
func adminAuthorizer(jwtService *auth.Service, authorizer authz.Resolver, timeout time.Duration) func(...string) func(http.Handler) http.Handler {
	authMW := auth.Middleware(jwtService)
	live := authz.Middleware(authorizer, timeout)
	return func(permissions ...string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return authMW(adminOnly(live(auth.RequirePermission(permissions...)(next))))
		}
	}
}

// adminOnly 挡掉**商户端**的令牌。
//
// 判据是 Tenant 非空：平台后台账号没有租户，而商户端账号一定有一个。少了这一道，任何一个
// 商户账号只要被授了 payment:read 就能从后台接口读到**全平台**的支付单——权限码管的是
// 「能做什么」，这一道管的是「以谁的身份」，两者不能互相替代。
//
// 回 403 而不是 401：令牌是有效的，只是不属于这一侧。回 401 会让前端把用户登出。
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

// secretResolver 把一条渠道上的一把凭据解析成密钥本身。
//
// 今天只有一个来源：**环境变量**——目录里的 Channel 声明槽名到环境变量名的映射
// （见 catalog.Channel.SecretEnv），这里照着它读。从前那条链是「库里的密文 → secret_ref
// 指的环境变量」，前半段随着 payment_channels 那张表一起没了：凭据不再入库，于是没有密文
// 可解、没有槽名可回落，主密钥（PAYMENT_SECRET_KEY）也就成了一件不需要存在的东西。
//
// 只剩一步，但**失败关闭**那条规矩一个字都没变：读不到就是空串，而空串在适配器那边是
// 「验不了签」，一律拒。绝不能有「配不出密钥就跳过验签」这种降级——那等于没有防重放。
//
// 仍然做成一个函数而不是让适配器自己去读环境：适配器因此是可测的纯函数（见 provider.Method
// 的注释），密钥从哪来是装配处的事。**明文只在这条调用栈上活一次**：它被塞进那一个请求
// 结构体，不入库、不进摘要、不进日志（见 service.recordProviderCall）。
func secretResolver() service.SecretResolver {
	return func(channel *catalog.Channel, slot string) string {
		if channel == nil {
			return ""
		}
		name := strings.TrimSpace(channel.SecretEnv[slot])
		if name == "" {
			// 这个槽没有对应任何环境变量：不是错误，只是这条路没有密钥可用（比如某条
			// 只跑小程序的渠道用不上入站验签的那一把）。空串在下游是拒绝，不是放行。
			return ""
		}
		// 环境变量的值也 trim：`.env` 里一行 `PAYMENT_UMS_APP_KEY=abc ` 的尾巴会进待签串，
		// 而那个签出来的东西谁都验不过——包括我们自己。silently 带着一个尾随空格去签名，
		// 表现是每一笔都「签名不对」而配置看上去完全正常。
		return strings.TrimSpace(os.Getenv(name))
	}
}
