// membership-service 拥有会员域的事实：会员套餐、会员资格本身、会员变更流水、以及连续包月的
// 订阅（签约与代扣，见下）。
//
// # 这一刀的形状
//
// **买 → 生效 → 到点失效**：订单支付成功的事件进来，在下单人的会员上续期（第一次就是开户），
// 到点由扫描改成已过期。会员资格是**成交快照**：买的时候是什么套餐、什么价、会员价走哪条路
// （自动享还是发券），全部抄在 memberships 那一行上，之后后台改套餐不追溯改变已购会员。
//
// # 会员价本身不在这个库里
//
// 本库只有**资格**（谁、到什么时候、走哪条路）。饮品上有原价 / 会员价 / 提货码价三列，价格
// 归 coffee-machine-service；券归 coupon-service。所以本服务对外只回答一个问题——「这个用户
// 此刻算不算会员价」（GetMemberPriceEntitlement），其余一概是别的域的事。
//
// # 两个入口，两棵树
//
// 后台（/v1/admin/membership-*，认证 → 平台账号闸门 → 实时授权 → 权限码）与小程序
// （/v1/miniapp/membership*，只套认证）。加一条件入向 gRPC：那条查询是同步依赖，order-service
// 下单定价与 coffee-machine-service 展示都要现问（见 internal/rpc 的包说明）。
//
// # 连续包月：签约与代扣都通了
//
// 小程序那边两条路（/v1/miniapp/membership/subscriptions 发起、.../confirm 确认，见
// internal/service/signing.go）能把一条真实签约走完：本服务找 payment-service 建协议、拿跳转
// 参数，用户签完回来确认，订阅从 pending_sign 变成 active。
//
// 代扣那一半是**一进一出**两个方向，缺一个都不成立：renewalWorker 扫到点的订阅、向支付服务发起
// 这一期的扣款（只发起，一个字节都不改本库），扣款的结果由渠道推回来、经支付域转成
// payment.agreement.charge_succeeded / charge_failed 两条事件进 HandleEvent，那里才续会员、记
// 失败、连到阈值就停扣。只上 worker 的表现是「钱扣了、会员没续」，只上消费的表现是「永远不去扣」。
//
// 扣款成功之后是**两步**，顺序不能反：先在订单域记一张续费单（订单管理里看得见这笔钱，且拿到
// 一个订单号），再在本库续这一期会员，并把那个订单号写进变更流水与 membership.renewed 事件。
// 那个订单号是**下游发券的判据**——coupon-service 拿到空的 orderId 会当成「后台人工改了有效期，
// 不是一次成交」而安静地不发券，正是这一刀之前连续包月用户每期扣了钱却一张会员价券都没拿到的
// 原因。反过来（先结算再建单）更糟：重投时结算那一步的幂等会先把它安静地 ack 掉，建单永远轮不到
// （见 internal/service/event.go 里那段）。
//
// 渠道那边改了口径有两条路能进来：后台点一下
// /v1/admin/membership-subscriptions/{id}/sync（回渠道核一次、按渠道的结论纠正本地），以及
// 支付域推过来的 payment.agreement.signed / terminated（见 service.HandleEvent）。两条路与
// 小程序的 confirm 落的是同一个结论（repository.SettleSubscription），判据只有一套。
//
// # memberships.auto_renew 是订阅的投影
//
// 会员中心那个用户可关的开关与订阅行是同一件事的两半：签约生效就在同一个事务里拨开，解约
// （用户自己关、后台取消、渠道作废）就在同一个事务里拨灭（见 repository.applyAutoRenew）。
// 所以「用户在小程序点关闭自动续费」不是翻一个 flag 而是**解约**——先让支付域把渠道那份协议
// 解掉，成功了才改本地（见 service.SetAutoRenewByUser / terminateForSubscription）。权威的
// 判据永远是订阅行，不是这个 flag。
//
// 同样不做的：会员价券的发放（归 coupon-service，本域只在自己的变更流水里记「这一期该发
// 几张」）、支付退款后的会员回退（沿用 payment 那一刀的口径，未接）、以及**扣款的查单兜底**
// （通知丢了那一期就永远停在「已受理、等通知」，只能人工去微信商户平台核，见
// worker/renewal.go 里那段）。
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
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/rpc"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/worker"
	membershipv1 "github.com/panda-dev/panda-v2/contracts/proto/membership/v1"
	"github.com/panda-dev/panda-v2/migrations"
)

const (
	// userServiceDialTimeout 限制启动时建立那条共享连接的时间。取值与 lottery-service、
	// payment-service 那几条一致——它管的是启动时把连接拨通，不是每一次调用的超时。
	userServiceDialTimeout = 5 * time.Second
	// authorizationTimeout 限制单次实时鉴权查询，不是缓存 TTL：每个后台请求都重新查一次
	// 调用方的授权。
	authorizationTimeout = 5 * time.Second
	// merchantServiceDialTimeout 既是拨通那条共享连接的时间上限，也直接用作单次门店查询
	// （存在性、一页名字）的超时——lottery-service 就是这么传的（那边也是同一个常量走两个
	// 用途），两个服务对同一个商户域面问的是同一类问题，超时不该有两个说法。
	merchantServiceDialTimeout = 5 * time.Second
	// paymentServiceDialTimeout 既是拨通 payment-service 那条连接的时间上限，也直接用作单次
	// 签约 RPC（CreateAgreement / QueryAgreement）的超时——与上面 merchant 那条同一个口径。
	// 5 秒是「拨号 + 一次内部 gRPC」的尺度：签约本身还要打微信，那段耗时在 payment 那侧自己
	// 用渠道超时管，不走这个数。
	paymentServiceDialTimeout = 5 * time.Second
	// orderServiceDialTimeout 是同一口径的第三个：既是拨通 order-service 那条连接的时间上限，
	// 也直接用作单次建单 / 查单的超时。
	//
	// **这个数不是随手取的**：扣款事件那条路上的 gRPC 调用发生在消息 handler 里，而平台的收件箱
	// lease 只有 1 分钟（见 internal/service/event.go 与 platform/messaging）。超过它，消息会在
	// 我们还在等订单域的时候被重新投出去，两条线程同时建同一张单（幂等键挡得住，但那是白折腾）。
	// 5 秒对一次内部 gRPC 是够的，也是上面两条共用的口径。
	orderServiceDialTimeout = 5 * time.Second
)

func main() {
	cfg, err := config.Load("membership-service")
	if err != nil {
		log.Fatalf("membership-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("membership-service: init database: %v", err)
	}
	defer db.Close()

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("membership-service: database does not support the membership API")
	}
	// 先迁移再读表：仓储假定表已经存在。生产由发布流程离线迁移，所以这里默认关闭，
	// 只有 DB_MIGRATE_ON_START 打开时才跑。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.Membership); err != nil {
			log.Fatalf("membership-service: migrate: %v", err)
		}
	}

	// 一个注册中心、一条共享连接。
	reg := registry.New(cfg.RegistryEndpoint)

	// 后台的实时鉴权。它验的是调用者自己的 access token，所以这条连接上的客户端不带服务
	// 令牌（见 client.AdminAccessResolver）。
	userConn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("membership-service: dial user-service: %v", err)
	}
	defer userConn.Close()
	authorizer := client.NewAdminAccessResolver(userConn)

	// 后台开通会员之前要问一次「这家门店在不在」（归属门店是一条要能对得上账的留痕，不能
	// 放一个查无此店的 id 进去），会员列表与详情要解一页门店名——会员库只存门店 id，名字是
	// 商户域的事实（见 client.StoreClient）。地址缺席时 config 那边直接拒绝启动，不是
	// 「少个可选依赖」：那两条路都会坏。
	merchantConn, err := platformclient.Dial(context.Background(), "merchant-service", cfg.MerchantGRPCAddress, merchantServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("membership-service: dial merchant-service: %v", err)
	}
	defer merchantConn.Close()
	stores, err := client.NewStoreClient(merchantConn, cfg.MerchantInternalToken, merchantServiceDialTimeout)
	if err != nil {
		log.Fatalf("membership-service: init store client: %v", err)
	}

	// 微信委托代扣的签约**不在这个库里**：协议住在 payment-service（payment_agreements），
	// 本服务只在订阅上留两个值引用。签约要签的是「用谁的名义」，所以那条路上有两件服务间的
	// 事实：协议由支付域建（paymentConn），openid 由身份域给（userConn，与上面的鉴权解析器
	// 共用同一条连接——它们是同一个服务的两个 RPC）。两个地址缺席时 config 那边拒绝启动：
	// 签约那条路整条不可用，而**没有签约就扣不了款**。
	paymentConn, err := platformclient.Dial(context.Background(), "payment-service", cfg.PaymentGRPCAddress, paymentServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("membership-service: dial payment-service: %v", err)
	}
	defer paymentConn.Close()
	agreements, err := client.NewAgreementClient(paymentConn, cfg.MerchantInternalToken, paymentServiceDialTimeout)
	if err != nil {
		log.Fatalf("membership-service: init agreement client: %v", err)
	}
	wallets, err := client.NewWalletIdentityReader(userConn, cfg.MerchantInternalToken, userServiceDialTimeout)
	if err != nil {
		log.Fatalf("membership-service: init wallet identity reader: %v", err)
	}

	// 续费那笔账记在**订单域**：一期代扣成功之后，先在这条连接上建一张续费单（订单管理里看得见、
	// 下游 coupon-service 拿到订单号才发会员价券），再回本库续这一期会员。地址缺席时 config 那边
	// 拒绝启动——钱已经在渠道扣走了，而这一期记不成账。
	//
	// 同一个客户端还服务后台订阅详情里「首月支付信息」那一段（读订单、拿 payment_no）。那是只读
	// 展示，失败只让那一块空着，与上面那条写路径的处置不同（见 service/subscription_detail.go）。
	orderConn, err := platformclient.Dial(context.Background(), "order-service", cfg.OrderGRPCAddress, orderServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("membership-service: dial order-service: %v", err)
	}
	defer orderConn.Close()
	orders, err := client.NewOrderClient(orderConn, cfg.MerchantInternalToken, orderServiceDialTimeout)
	if err != nil {
		log.Fatalf("membership-service: init order client: %v", err)
	}
	// 后台订阅详情要的两块支付域只读（续费明细、首月那笔的渠道流水）：**复用上面 paymentConn 那条
	// 连接**，只是换一个只读的能力面（见 client.ChargeReader）。分一个类型而不是往 AgreementClient
	// 上挂两个方法，是为了让「这条链路上哪些调用会改别的域的状态」在目录上一眼可见。
	charges, err := client.NewChargeReader(paymentConn, cfg.MerchantInternalToken, paymentServiceDialTimeout)
	if err != nil {
		log.Fatalf("membership-service: init charge reader: %v", err)
	}

	// 仓储带 recorder：冻结 / 解冻 / 撤销 / 后台直接改有效期都是**人工干预**，而且这一枚
	// （membership:adjust）直接白送钱——把 expire_at 往后挪一年等于送一年会员价，没有任何订单、
	// 支付、流水跟着发生。方案 §11.6 把「影响用户资产归属的人工操作」列进必审清单，所以它
	// 每一次都写一条平台审计（进 admin_operation_logs），同时在自己的流水里留一条。
	//
	// 开通与续期**不记审计**：那是支付带来的正常动作，每一次都记只会把真正要看的那几条淹掉。
	// 用户自己关自动续费也不记（见 service.SetAutoRenewByUser）。
	membershipRepo := repository.NewPostgresRepository(pool.Pool(), audit.NewRecorder())
	membershipService := service.New(membershipRepo, service.Options{
		Stores:     stores,
		Agreements: agreements,
		Wallets:    wallets,
		Orders:     orders,
		Charges:    charges,
	})

	adminMembership := controller.NewAdminMembershipController(membershipService)
	miniappMembership := controller.NewMiniappMembershipController(membershipService)
	membershipRPC := rpc.NewMembershipService(membershipService)

	// 到期扫描必须有人做：没有它，会员的状态会永远停在 active。它**不影响用户能不能享会员价**
	// （判定读的是 expire_at，见 worker 包里的说明），影响的是后台按状态筛人。周期与批量用
	// 默认值。
	expiryWorker := worker.NewExpiryWorker(membershipService, worker.DefaultSweepInterval, worker.DefaultSweepBatch)

	// 续费扫描：到点的订阅向渠道发起这一期的扣款。它是「签约那一刀」之后缺的那一半——没有它，
	// 一条订阅签得成、生效得了、**却一分钱都不会去扣**（见 service/charge.go 开头的说明）。
	//
	// 它只发起，不结算：钱到没到由渠道推回来的通知决定，那两条事件经 MQ 进来（见 service/event.go）。
	// 所以这个 worker 与 HandleEvent 是配套的——只上其中一个，表现分别是「扣了钱不续会员」与
	// 「会员永远续不上」。
	renewalWorker := worker.NewRenewalWorker(membershipService, worker.DefaultRenewalInterval, repository.DefaultChargeBatch)

	// access token 24 小时，与其余八个服务保持一致（见那几处的说明）。各处 auth.NewService
	// 的取值必须一致。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("membership-service: init jwt: %v", err)
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// Outbox：每一次开通 / 续期 / 到期都在业务事务里追加一条事件，与会员状态、变更流水
		// 同生共死。**本轮没有消费者**（券的发放归 coupon-service，还没接），但事件照样要落
		// ——券迟早要发，而「会员开了却没留下事件」这件事一旦发生，补不回来。relay 没有配
		// publisher 时事件留在表里等重启再投。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		// ConsumerInbox：订单支付成功的事件会重投（broker 重连、处理完但没 ack 时崩溃都会），
		// 而续期是要写状态的。没有它，同一次支付会把会员续两期。
		ConsumerInbox: messaging.NewPostgreSQL(pool.Pool()),
		// 消费的是订单支付成功。事件类型由 order-service 决定，本服务只认自己认识的那一种，
		// 其余原样 ack（见 service.HandleEvent）。
		ConsumerHandler: membershipService.HandleEvent,
		Workers:         []runtime.Runner{expiryWorker, renewalWorker},
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			// 后台：认证 → 平台账号闸门 → 实时授权 → 权限码。
			routes.RegisterAdmin(r, adminMembership, adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
			// 小程序：只套认证。C 端的授权不是权限码能表达的（见 routes.RegisterMiniapp），
			// 「谁」来自令牌、路径上没有 user_id。
			routes.RegisterMiniapp(r, miniappMembership, auth.Middleware(jwtService))
		},
		// 入向 gRPC：order-service 下单定价与 coffee-machine-service 展示都要问「这个用户现在
		// 算不算会员价」。注册中心未启用发现时按 MEMBERSHIP_GRPC_ADDR 静态地址接受调用。
		GRPCRoutes: func(s *kgrpc.Server) {
			membershipv1.RegisterMembershipServiceServer(s, membershipRPC)
		},
		// 服务端验服务令牌：这个 gRPC 面只对内部开放。沿用 MERCHANT_INTERNAL_TOKEN——它在这个
		// 仓库里已经是「服务之间互认」的那一枚，payment-service、lottery-service、user-service
		// 几个方向都用它（变量名带着 merchant 是历史，改名要几个服务一起动，不在这次改动里做）。
		GRPCServerOptions: []kgrpc.ServerOption{
			auth.GRPCServerOption(jwtService, cfg.MerchantInternalToken),
		},
	}); err != nil {
		log.Fatalf("membership-service: %v", err)
	}
}

// adminAuthorizer 组装每条后台路由都要走的中间件，顺序是：
//
//	认证 → 平台账号闸门 → 实时授权 → 权限码
//
// adminOnly 放在实时查询外面，这样商户 token 在本地就被挡掉，不用付一次 gRPC 往返。实时查询
// 放在 RequirePermission 外面，是因为它要先覆盖 token 里的授权快照——那次覆盖正是权限校验
// 读到当前数据而不是过期声明的原因。这里尤其重要：membership:adjust 管的正是「谁能把一个人的
// 会员有效期改到明年」，一个小时的授权延迟都不能接受。
//
// 与 order-service / lottery-service / payment-service 是同一份实现，各服务各留一份：它把
// 四个平台包的中间件按一个具体次序拼起来，而那个次序是**这个服务的安全属性**，不该被一个共享
// 实现的改动悄悄改掉。
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
