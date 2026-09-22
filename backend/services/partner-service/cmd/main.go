// partner-service 拥有**开放平台（合作方接入）**这一域：合作方账号、API 密钥、调用日志，
// 以及合作方调进来的那几条开放接口（方案 §3）。
//
// # 它对外有三个入口，方向与性质都不同
//
//   - **入（后台）**：/v1/admin/*。挂满四层（认证 → 平台账号闸门 → 实时授权 → 权限码），
//     读的几条挂 partner:read（合作方、密钥掩码、调用日志），写的挂 partner:manage（新增/
//     修改/启停合作方、签发新密钥、改白名单与限流）。
//
//   - **入（开放接口）**：/v1/openapi/*。**故意不挂我们的认证**——合作方没有我们的令牌，
//     它带的是自己算的签名（X-API-Key / X-Timestamp / X-Nonce / X-Signature）。整棵树被
//     ingress.Guard 包着，验签、时间窗、nonce 去重、启停、过期、IP 白名单、限流七道都在
//     那一段里。**这是本服务唯一一条从公网打进来的路**：网关是它唯一的入口（不在白名单里
//     的来源即使签名正确也会被拒，见 ingress/auth.go）。
//
//     这一棵树上今天三条：一条查询（GET /v1/openapi/member-price-entitlement）与两条**写**
//     （POST /v1/openapi/device/sync-order 线下刷卡机、POST /v1/openapi/device/pickup 取货码，
//     方案 §四）。两条写的共同点是**钱都不在我们的渠道那儿**——刷卡机是钱已经在机器上收过了，
//     取货码是扣这台设备的咖啡余额——所以两条都直接落单、没有 pending 可等，本服务也都不
//     碰金额计算（除了取货码那条：它的定价反而必须我们做，见 internal/service/pickup.go）。
//
//   - **出**：gRPC 调 membership-service（查会员权益）、order-service（记设备刷卡订单与取货码
//     订单），以及调 user-service（后台的实时鉴权）。**只有 order 那两条是写**，其余都是只读的转发。
//
// # 这一域不复制任何业务数据
//
// 合作方的订单、券、会员资格都在各自的域里，本服务一个字段都不存（见 internal/model 的包
// 注释）。开放接口做的事只有三件：校验参数形状、取出合作方身份、把请求转出去。
//
// # 三条已知缺口（没做到的地方，明说）
//
//  1. 方案 §3.2 的三条开放接口里**只有「会员权益查询」落得下**：order-service 与
//     coupon-service 的**查询** RPC 到今天都还没有（order 那边到今天的两条 RPC 都是写——
//     CreateDeviceOrder 与 CreatePickupOrder，分别被设备刷卡回执与取货码那两条写路径调），
//     没有可以调的东西。硬造 RPC 或
//     直连对方的库都不在允许范围内，而一个假接口比没有接口更坏——它会以「已经支持」的样子
//     出现在对接文档里。补齐它们需要先在那边加 RPC（推荐：order-service 的 GetOrderByNo /
//     coupon-service 的 Issue）。
//
//  2. **partner_id 传不到下游**：GetMemberPriceEntitlementRequest 只有 user_id 一个字段，
//     而本仓库没有「操作人元数据」这样的约定（platform/auth 与 platform/client 里都没有
//     WithOperator 之类的东西）。所以会员域看不到「这次查询是哪个合作方发起的」。本服务
//     把 partnerId 放进响应体，让**我们这边**的调用日志能对上（见 service/openapi.go）。
//     要真正补上得给 proto 加字段或定一层元数据约定，两件事都不在本轮范围内。
//
//     两条设备写路径同样：CreateDeviceOrderRequest 与 CreatePickupOrderRequest 里都没有合作方
//     （合同明文规定内部 RPC 不接受调用方自报的身份字段），订单域因此只知道「哪台设备」，
//     不知道「哪个合作方」——要挂上合作方得先在 proto 上加位置，见 client/order.go 的说明。
//
//  3. **调用日志没有保留期清理**：partner_call_logs 是只增表，而它会按流量增长（报文各自
//     截断在 8 KiB）。到期删除需要一个 worker 与一条保留策略，本轮没做——这一条写在
//     migrations/partner/001 的文末。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	kgrpc "github.com/go-kratos/kratos/v2/transport/grpc"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	"github.com/panda-dev/panda-v2/backend/platform/cache"
	platformclient "github.com/panda-dev/panda-v2/backend/platform/client"
	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/platform/database/migrate"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/platform/ratelimit"
	"github.com/panda-dev/panda-v2/backend/platform/registry"
	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/platform/server"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/service"
	"github.com/panda-dev/panda-v2/migrations"
)

const (
	// userServiceDialTimeout 限制启动时建立那条共享连接的时间。取值与 payment-service /
	// membership-service 那几条一致——它管的是启动时把连接拨通，不是每一次调用的超时。
	userServiceDialTimeout = 5 * time.Second
	// membershipServiceDialTimeout 同上。
	membershipServiceDialTimeout = 5 * time.Second
	// orderServiceDialTimeout 同上（设备刷卡回执那条写路径）。
	orderServiceDialTimeout = 5 * time.Second
	// authorizationTimeout 限制单次实时鉴权查询，不是缓存 TTL：每个后台请求都重新问一次
	// user-service。
	authorizationTimeout = 5 * time.Second
	// entitlementTimeout 是单次「查会员权益」的超时。
	//
	// 由本服务自己给，不是靠合作方的超时：合作方那头可能永远不超时，而这条链路上每一跳都
	// 必须有自己的上限（见 client.MembershipEntitlementReader）。
	entitlementTimeout = 5 * time.Second
	// deviceOrderTimeout 是单次「记一笔设备刷卡订单」的超时，与 entitlementTimeout 同一个
	// 理由：由本服务自己给，不靠合作方的超时。
	//
	// 它比查询那条更值得有一个上限：这条是**写**，而写超时之后的处置是有结论的——幂等键在
	// 对方单号上，超时后合作方原样重投不会多出一张单（见 client.DeviceOrderCreator）。
	deviceOrderTimeout = 5 * time.Second
	// startupContextTimeout 限制启动期那几次外部往返（连 Redis、Ping）。
	//
	// 与别的服务不同，本服务的启动**必须真的连上 Redis**：nonce 去重是防重放的全部，
	// 而它失败关闭（见下面 NewNonceStore 那一段）。一个连不上 Redis 的进程要么起不来、
	// 要么起得很慢，不能是「起来了但每个请求都 503」。
	startupContextTimeout = 5 * time.Second
)

func main() {
	cfg, err := config.Load("partner-service")
	if err != nil {
		log.Fatalf("partner-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("partner-service: init database: %v", err)
	}
	defer db.Close()

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("partner-service: database does not support the partner API")
	}
	// 先迁移再读表：仓储假定表已经存在。生产由发布流程离线迁移，所以这里默认关闭，只有
	// DB_MIGRATE_ON_START 打开时才跑。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.Partner); err != nil {
			log.Fatalf("partner-service: migrate: %v", err)
		}
	}

	// 仓储带 recorder：合作方与密钥的每一次改动都要留痕（方案 11.6）。两条**读**路径也走
	// 同一个仓储（验签时的那一次 JOIN、后台的列表），它们当然不记审计——记的是写方法。
	//
	// 审计进的是本地 outbox（platform/audit），由 runtime 的 relay 投给身份库。所以下面的
	// Options 里**必须**有 Outbox：少了它，recorder 照样每次返回成功，而行会一直堆在
	// message_outbox 里没人投递——那是「审计看起来记了、实际没到」这类缺口里最难查的一种。
	partnerRepo := repository.NewPostgresRepository(pool.Pool(), audit.NewRecorder())

	// 加密签名密钥的主密钥（platform/secret）。在这里 Parse、不在第一次用到时：格式不对是
	// 一个**启动期**错误。带着一把解不开的主密钥跑起来，表现是每一把新签的密钥都解不开
	// （验签一律 SIGNATURE_MISMATCH），而日志里只有「签名不对」——排查会从合作方那边找起，
	// 而问题在我们自己的配置里。
	//
	// config.Load 已经保证了它非空（partner-service 少这一项直接拒绝启动），这里管的是形状：
	// 32 字节，base64 或 64 位 hex。都不过就把原话打出来，不打印密钥本身。
	keyring, err := secret.Parse(cfg.PartnerSecretKey)
	if err != nil {
		log.Fatalf("partner-service: PARTNER_SECRET_KEY: %v", err)
	}

	// Redis：限流与 nonce 去重共用一条连接池。
	//
	// **这里不复用别的服务那种「RedIS 没有就退化成进程内」的容忍**：nonce 去重的失败关闭是
	// 硬要求（方案 1.4 第 1 条），而一个连不上 Redis 的进程在运行期会把**每一次**开放接口
	// 调用都拒掉（503 NONCE_STORE_UNAVAILABLE）。与其那样，不如在启动时说话。
	//
	// 三步都不吞错：NewFromEnv 返回 Noop（REDIS_ADDR 为空）→ NewNonceStore 认出它并报错；
	// 地址写了但连不上（Ping 失败）→ 同样报错。这是「本机 .env 里 REDIS_ADDR 经常是空的」
	// 那件事在代码里的落点。
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupContextTimeout)
	defer cancelStartup()
	cacheClient, err := cache.NewFromEnv(startupCtx)
	if err != nil {
		log.Fatalf("partner-service: init cache: %v", err)
	}
	nonces, err := ingress.NewNonceStore(startupCtx, cacheClient)
	if err != nil {
		log.Fatalf("partner-service: nonce store: %v", err)
	}

	// 限流器按额度缓存（每把密钥一行）。client 传的是同一个（真 Redis 时走
	// platform/ratelimit 的脚本；Noop 会退化成进程内令牌桶，但上面那条已经让 Noop 起不来了）。
	limiters := ingress.NewLimiters(cacheClient)

	// 可信代理网段：合作方永远是从网关进来的，所以判定来源必须看 XFF 里的那一跳，而
	// 「谁写的 XFF 可以信」只能由配置回答。配错（比如把一个域名写进来）直接起不来——
	// 悄悄退回「只看 RemoteAddr」的表现是：所有请求的来源都是网关的地址，于是 IP 白名单
	// 要么全拒要么全放，而两者都不会报错（见 ingress/auth.go 与 platform/ratelimit）。
	trust, err := ratelimit.NewProxyTrust(splitList(cfg.PartnerTrustedProxyCIDRs))
	if err != nil {
		log.Fatalf("partner-service: PARTNER_TRUSTED_PROXY_CIDRS: %v", err)
	}

	// 验签中间件。**任何一项依赖缺失都会让它构造失败**——包括上面那几步（它们已经先失败了）。
	// 一个少了 nonce 检查的 Guard 能跑、能返回 200，而它防重放的那一层是空的；那种服务比
	// 起不来的服务危险得多。
	guard, err := ingress.New(ingress.Options{
		Lookup: credentialLookup(partnerRepo),
		// Keyring 是**非指针判空**的那个：platform/secret 的 Keyring 在 nil 接收者上返回
		// ErrMasterKeyMissing，于是「忘了配主密钥」的表现是每次验签都 SECRET_UNREADABLE
		// （503），而不是一个空指针 panic。
		Keyring:        keyring,
		Nonces:         nonces,
		Limiters:       limiters,
		CallLogs:       partnerRepo,
		TrustedProxies: trust,
	})
	if err != nil {
		log.Fatalf("partner-service: partner ingress: %v", err)
	}

	// 治理层（后台）。keyring 与仓储都是上面那两个：签发密钥要在**同一个** Keyring 上封存，
	// 否则验签时解不开。
	admin := service.NewAdminService(partnerRepo, keyring)
	adminController := controller.NewAdminPartnerController(admin)

	// 注册中心适配器**共用一个**：两条 gRPC 连接与下面的 runtime 用的是同一份。每条连接各
	// 建一个也能跑，但那样一个进程里会多出两个 etcd 客户端，而它们谁都不会被关掉——启动期
	// 的泄漏在滚动发布里是「连接数慢慢涨」这种最难归因的样子（payment-service 同一处同一条
	// 理由）。
	reg := registry.New(cfg.RegistryEndpoint)

	// 会员权益的那条转发。走**另一条**到 membership-service 的连接（不是上面 user-service
	// 那条，地址都不一样），带服务令牌——这次调用代表平台去问一个事实。
	membershipConn, err := platformclient.Dial(context.Background(), "membership-service",
		cfg.MembershipGRPCAddress, membershipServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("partner-service: dial membership-service: %v", err)
	}
	defer membershipConn.Close()
	// 令牌用 MERCHANT_INTERNAL_TOKEN：与下面 GRPCServerOptions 里验的那一枚、以及
	// membership-service 那边 auth.RequireService 认的那一枚是同一个（变量名带着 merchant
	// 是历史，改名要几个服务一起动，不在这次改动里做）。
	entitlements, err := client.NewMembershipEntitlementReader(membershipConn, cfg.MerchantInternalToken, entitlementTimeout)
	if err != nil {
		log.Fatalf("partner-service: membership entitlement reader: %v", err)
	}

	// 设备刷卡回执那条**写**路径（线下刷卡机，方案 §四）。又一条独立的连接，目标换成
	// order-service。
	//
	// 这是本服务唯一一处会让别域落数据的调用：HTTP 那一侧由 ingress.Guard 验完签（合作方的
	// 密钥在库里），这里再代表平台把既成事实交给订单域。合作方身份**进不了这条 RPC**
	// （CreateDeviceOrderRequest 里没有这个位置，也不该有——见 client/order.go）。
	//
	// 地址是必填项：config.Load 对 partner-service 拒空 ORDER_GRPC_ADDR，理由是缺了它这条
	// 回调会「验完签、建不出单」——而合作方看到的是一句 5xx，他会一直重投（钱早就收过了）。
	orderConn, err := platformclient.Dial(context.Background(), "order-service",
		cfg.OrderGRPCAddress, orderServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("partner-service: dial order-service: %v", err)
	}
	defer orderConn.Close()
	deviceOrders, err := client.NewDeviceOrderCreator(orderConn, cfg.MerchantInternalToken, deviceOrderTimeout)
	if err != nil {
		log.Fatalf("partner-service: device order creator: %v", err)
	}

	openapi := service.NewOpenAPIService(entitlements, deviceOrders)
	openapiController := controller.NewOpenAPIController(openapi)

	// 后台的实时鉴权，走另一条到 user-service 的连接。它验的是调用者自己的 access token，
	// 所以这条连接上的客户端不带服务令牌（见 client.AdminAccessResolver）。
	//
	// 本域的两枚码里 manage 能改「别人调用我们的凭据」，所以它的生效延迟必须是零——实时
	// 查一次，不读令牌里那份可能已经旧了一小时的 claims（理由写在那个文件里）。
	userConn, err := platformclient.Dial(context.Background(), "user-service",
		cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("partner-service: dial user-service: %v", err)
	}
	defer userConn.Close()
	authorizer := client.NewAdminAccessResolver(userConn)

	// access token 24 小时，与其余服务保持一致（见 payment-service 那一处的说明）。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("partner-service: init jwt: %v", err)
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// Outbox：审计（合作方与密钥的每一次改动）在业务事务里追加一条事件，relay 投给身份库。
		// 本服务**不发布任何领域事件**（它没有自己的业务事实可供别的域消费），这一项只为审计
		// 而存在——但它是必需的，理由见上面构造 partnerRepo 那一段。
		//
		// RABBITMQ_URL 为空时（dev 常见）Publisher 是 Noop，runtime 认出它「接受每一次投递但
		// 没人收」并**不启动** relay：审计事件会堆在 message_outbox 里，
		// 等服务接上 broker 再补投。这是刻意选的（比把每条标成已投递再丢掉好），但它意味着
		// **本地验收时审计查不到不是 bug**——要验审计得先给 RABBITMQ_URL。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			// 两棵树、两次注册，**顺序无关但要分开写**：开放接口树故意不挂我们的认证，后台
			// 树每条都挂。合并成一个 Register 会稀释 admin 那条「校验器为 nil 就全 401」的
			// 保证（见 routes 的包说明）。
			routes.RegisterOpenAPI(r, openapiController, guard)
			routes.RegisterAdmin(r, adminController,
				adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
		},
		// 本服务**没有入向 RPC**：它没有自己的 proto，下面没有一行 RegisterXxxServiceServer。
		// 但服务端令牌校验照样挂上，原因是它不能再被忘掉一次——将来给本服务加 RPC 的人若是
		// 忘了这一步，那个口子会以「匿名可调」的样子出现，而它调的是治理层。挂上之后加
		// gRPC 面的动作就只剩一行 GRPCRoutes。
		GRPCServerOptions: []kgrpc.ServerOption{
			auth.GRPCServerOption(jwtService, cfg.MerchantInternalToken),
		},
	}); err != nil {
		log.Fatalf("partner-service: %v", err)
	}
}

// credentialLookup 把仓储的查询适配成 ingress 认的那个签名。
//
// 存在的全部理由是**一个错误的翻译**：仓储说 ErrAPIKeyNotFound（「库里没有这一行」），
// ingress 说 ErrCredentialNotFound（「这个凭据认不出来」）。让 ingress 直接 import 仓储也能
// 省掉这个函数，但那样「验签中间件」与「表长什么样、错误值叫什么」就绑死了——今天它们只是
// 恰好都在这个服务里，而中间件那一层是将来最可能被搬走的一段。
//
// 其余错误**原样返回**：库连不上（基础设施故障）与查不到（凭据无效）在中间件里的处置相同
// （都拒），但调用日志里分开写（CREDENTIAL_LOOKUP_FAILED / UNKNOWN_API_KEY）——排查时那是
// 「我们的库挂了」与「有人在猜密钥」的区别。
func credentialLookup(repo *repository.PostgresRepository) ingress.CredentialLookup {
	return func(ctx context.Context, apiKey string) (*model.APIKey, *model.PartnerAccount, error) {
		key, partner, err := repo.FindCredential(ctx, apiKey)
		if errors.Is(err, repository.ErrAPIKeyNotFound) {
			return nil, nil, ingress.ErrCredentialNotFound
		}
		return key, partner, err
	}
}

// adminAuthorizer 是后台路由那四层中间件的组装：认证 → 平台账号闸门 → 实时授权 → 权限码。
//
// 与 payment-service / membership-service 等几个服务里的同名函数逐字
// 一致，各服务各留一份（理由见 internal/client/admin_access.go）。**层序不能调**：
//
//   - auth.Middleware 必须最先，它把令牌声明放进请求上下文，后面两层都读它。
//   - adminOnly 在它之后：它判的是「这是不是一个后台账号」，那要看已经解出来的身份。
//   - authz.Middleware 在最后一道，它要拿调用方的**原始令牌串**去问 user-service；放在前面
//     也能跑，但那样「不是后台账号」的请求会先去问一次授权，白花一次 RPC。
//   - RequirePermission 贴在最里面，它读的是上面那层刚取回来的权限列表。
//
// authenticate 传 nil 时 routes 那一侧会全 401（失败关闭），所以这里返回的闭包永远不是 nil
// ——「忘了装配」的表现是「谁都进不去」，不是「谁都能进」。
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
// 商户账号只要被授了 partner:manage 就能停用**别人**的密钥、改别人的 IP 白名单——权限码管的
// 是「能做什么」，这一道管的是「以谁的身份」，两者不能互相替代。
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

// splitList 读一个逗号分隔的配置项，空值返回 nil（= 什么都不信，见 ratelimit.NewProxyTrust）。
func splitList(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if entry := strings.TrimSpace(part); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}
