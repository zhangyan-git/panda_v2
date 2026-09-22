// order-service 拥有订单域的事实（方案 5.4、5.8）：合并订单（咖啡 + 幸运杯套）、会员
// 订单、订单行上挂的优惠券、支付流水、状态流水。
//
// 支付、优惠券发放、履约出杯三条链路都不在本服务里。本服务的边界是：下单时把校验过的
// 事实**冻结**成订单（价格、套餐、券的归属都成为快照），支付结果经事件进来落单，其余
// 服务需要订单事实时读这里。所以它对外只有三块入口——小程序端与后台端的 HTTP、一条
// 支付结果的事件消费，以及一条**入向 gRPC**：线下刷卡机的设备单
// （`order.v1.OrderService/CreateDeviceOrder`，partner-service 验完厂商签名之后调它）。
//
// 有一处**出网**的例外要记住：发起支付（`POST /v1/miniapp/orders/{orderNo}/pay`）由本服务
// 编排——它读订单、判归属与状态、把权威金额同步交给 payment-service 去建支付单。编排放
// 在这里的理由是那三个判据全是订单的事实（而且金额不能由客户端给），而支付域拿到的是一个
// 值引用，它不读订单库。注意这**不是**「订单服务拥有支付事实」：本服务只发起，支付单、
// 资金行、对账凭据全在 panda_payment 里。
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
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/controller"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/routes"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/rpc"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/worker"
	orderv1 "github.com/panda-dev/panda-v2/contracts/proto/order/v1"
	"github.com/panda-dev/panda-v2/migrations"
)

const (
	// userServiceDialTimeout 限制启动时建立那条共享连接的时间。
	userServiceDialTimeout = 5 * time.Second
	// coffeeMachineServiceDialTimeout 同理，这条连接给下单时的设备校验用。
	coffeeMachineServiceDialTimeout = 5 * time.Second
	// paymentServiceDialTimeout 同理，这条连接给发起支付用。
	paymentServiceDialTimeout = 5 * time.Second
	// membershipServiceDialTimeout 同理，这条连接给下单时读会员套餐用。
	membershipServiceDialTimeout = 5 * time.Second
	// accountServiceDialTimeout 同理，这条连接给「受理退款申请前问一句福卡还冻得上吗」用。
	accountServiceDialTimeout = 5 * time.Second
	// fortuneCardQuoteTimeout 限制一次冻结预览。与 deviceLookupTimeout 同口径：本地 gRPC
	// 是毫秒级，2 秒足够容忍抖动，又短到不会让一次卡住的下游把「申请退款」拖成长时间挂起。
	// 超时按「没问到」处理（503，见 service.ErrFortuneCardQuoteUnavailable）——**不是**按
	// 「卡用过了」处理，后者会冤枉一个什么都没做错的用户。
	fortuneCardQuoteTimeout = 2 * time.Second
	// membershipPlanLookupTimeout 限制一次套餐读取。它跟每一次带会员行的下单走，口径与
	// deviceLookupTimeout 一致：本地 gRPC 是毫秒级，2 秒足够容忍抖动，又短到不会让一次卡住的
	// 下游把下单接口拖成长时间挂起。超时按「没问到」处理——**不拒单也成不了单**，回 503，
	// 而不是当成「这个套餐买不了」（见 service.ErrMembershipPlanUnavailable）。
	membershipPlanLookupTimeout = 2 * time.Second
	// paymentCreateTimeout 限制一次发起支付。
	//
	// 比 deviceLookupTimeout 长：这一次往返里支付服务还要同步去问渠道（一次第三方网络调用），
	// 2 秒会把正常但稍慢的渠道误判成「结果不明」。5 秒是「用户按下支付按钮之后愿意等多久」的
	// 上限，超时按「没问到」处理而不是「支付失败」——支付服务可能已经建好了支付单，重发会
	// 命中同一个幂等号拿回那一张（见 client.ErrPaymentUncertain）。
	paymentCreateTimeout = 5 * time.Second
	// authorizationTimeout 限制单次实时鉴权查询，不是缓存 TTL：每个后台请求都重新查
	// 一次调用方的授权。
	authorizationTimeout = 5 * time.Second
	// walletIdentityLookupTimeout 限制一次微信身份读取。与 deviceLookupTimeout 同口径：
	// 本地 gRPC 是毫秒级，2 秒足够容忍抖动，又短到不会让一次卡住的下游把发起支付拖成长时间
	// 挂起。超时按「没问到」处理（503），**不是**按「这个人没绑微信」处理——后者会落成一次
	// 支付失败，把我们的抖动说成用户的账户问题。
	walletIdentityLookupTimeout = 2 * time.Second
	// deviceLookupTimeout 限制一次设备读取。它跟每一次带饮品行的下单走，是每个下单请求
	// 都要付的往返预算：本地 gRPC 是毫秒级，2 秒足够容忍抖动，又短到不会让一次卡住的
	// 下游把下单接口拖成长时间挂起。超时按「没问到」处理——拒单，但回 503 而不是
	// 「设备不存在」，见 service.ErrDeviceLookupUnavailable。
	deviceLookupTimeout = 2 * time.Second
)

func main() {
	cfg, err := config.Load("order-service")
	if err != nil {
		log.Fatalf("order-service: load config: %v", err)
	}
	db, err := database.New(context.Background(), cfg.ServiceDatabaseURL)
	if err != nil {
		log.Fatalf("order-service: init database: %v", err)
	}
	defer db.Close()

	pool, ok := db.(*database.PGXPool)
	if !ok {
		log.Fatal("order-service: database does not support the order API")
	}
	// 先迁移再读表：仓储假定表已经存在。生产由发布流程离线迁移，所以这里默认关闭，
	// 只有 DB_MIGRATE_ON_START 打开时才跑。
	if cfg.MigrateOnStart {
		if err := migrate.Apply(context.Background(), pool.Pool(), migrations.Order); err != nil {
			log.Fatalf("order-service: migrate: %v", err)
		}
	}

	// 一个注册中心、两条共享连接：一条问 user-service 要后台的实时授权，一条问
	// coffee-machine-service 要设备事实。注册中心未启用发现时都回落到静态地址。
	reg := registry.New(cfg.RegistryEndpoint)
	userConn, err := platformclient.Dial(context.Background(), "user-service", cfg.UserGRPCAddress, userServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("order-service: dial user-service: %v", err)
	}
	defer userConn.Close()
	authorizer := client.NewAdminAccessResolver(userConn)
	// 同一条连接上的另一半：商户请求问的是「这个账号能看见哪些点位」，问的是同一个服务，
	// 但那是另一条 RPC，答案也不是同一种东西。
	merchantAccess := client.NewMerchantAccessResolver(userConn)
	// 同一条连接上的第二种令牌：发起支付时要问付款人的 openid，那一次调用代表订单域
	// （共享服务令牌），不代表用户自己。理由与下面几个读端一样，见 client.WalletIdentityReader。
	wallets, err := client.NewWalletIdentityReader(userConn, cfg.MerchantInternalToken, walletIdentityLookupTimeout)
	if err != nil {
		log.Fatalf("order-service: init wallet identity reader: %v", err)
	}

	deviceConn, err := platformclient.Dial(context.Background(), "coffee-machine-service", cfg.CoffeeMachineGRPCAddress, coffeeMachineServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("order-service: dial coffee-machine-service: %v", err)
	}
	defer deviceConn.Close()
	// 用共享服务令牌：这次调用代表基础设施去问一台机器的事实，不代表某个用户。
	// 沿用 MERCHANT_INTERNAL_TOKEN——它在这个仓库里已经是「服务之间互认」的那一枚，
	// user-service 与 merchant-service 两个方向都用它（变量名带着 merchant 是历史，
	// 改名要几个服务一起动，不在这次改动里做）。
	devices, err := client.NewDeviceReader(deviceConn, cfg.MerchantInternalToken, deviceLookupTimeout)
	if err != nil {
		log.Fatalf("order-service: init device reader: %v", err)
	}

	// 第三条共享连接：发起支付时告诉支付域一个事实。config.Load 已经拒绝了空地址——
	// 一个空地址的 Dial 会在第一次发起支付时才炸，而那一次请求正卡在用户的收银台上。
	paymentConn, err := platformclient.Dial(context.Background(), "payment-service", cfg.PaymentGRPCAddress, paymentServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("order-service: dial payment-service: %v", err)
	}
	defer paymentConn.Close()
	// 同样是共享服务令牌：这一次调用代表订单域去建支付单，不代表某个用户。发起支付那条路上
	// 的用户身份在 service.InitiatePayment 里已经用过了（它判归属），到这里只剩一个 user_id
	// 值引用——支付侧拿它记账，不拿它做授权。
	payments, err := client.NewPaymentCreator(paymentConn, cfg.MerchantInternalToken, paymentCreateTimeout)
	if err != nil {
		log.Fatalf("order-service: init payment creator: %v", err)
	}

	// 第四条共享连接：下单时问会员域两件事——带会员行的那一单，价格、时长与会员价路径全从
	// 这条连接取回来（见 service.applyMembershipPlan）；带饮品行的那一单，还要问「这个人此刻
	// 算不算会员价」（见 service.applyDrinkPricing）——这些事实只有会员域有，收客户端给的那份
	// 等于让客户端定价。同样是共享服务令牌：这次调用代表订单域去问一个商品事实，不代表用户。
	membershipConn, err := platformclient.Dial(context.Background(), "membership-service", cfg.MembershipGRPCAddress, membershipServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("order-service: dial membership-service: %v", err)
	}
	defer membershipConn.Close()
	plans, err := client.NewMembershipPlanReader(membershipConn, cfg.MerchantInternalToken, membershipPlanLookupTimeout)
	if err != nil {
		log.Fatalf("order-service: init membership plan reader: %v", err)
	}

	// 第四条共享连接：受理退款申请之前问账户域一句「这一单赠送的福卡现在还冻得上吗」。
	// 它是一条**同步读**而不是事件，因为这一问的结果要当场决定受不受理——用户点下「申请
	// 退款」那一刻就得知道行不行（见 service.assertFortuneCardsUnused）。config.Load 已经
	// 拒绝了空地址：空地址的 Dial 会在用户点下那个按钮时才炸。
	accountConn, err := platformclient.Dial(context.Background(), "account-service", cfg.AccountGRPCAddress, accountServiceDialTimeout, reg)
	if err != nil {
		log.Fatalf("order-service: dial account-service: %v", err)
	}
	defer accountConn.Close()
	fortuneCards, err := client.NewFortuneCardQuoter(accountConn, cfg.MerchantInternalToken, fortuneCardQuoteTimeout)
	if err != nil {
		log.Fatalf("order-service: init fortune card quoter: %v", err)
	}

	// 仓储带 recorder：后台取消订单要留痕（方案 11.6 的人工干预必审清单），C 端用户
	// 取消自己不需要——那不是一次需要追溯的越权。
	orderRepo := repository.NewPostgresRepository(pool.Pool(), audit.NewRecorder())
	orderService := service.New(orderRepo, devices, payments, plans, wallets, fortuneCards, service.Options{})
	adminOrders := controller.NewAdminOrderController(orderService)
	miniappOrders := controller.NewMiniappOrderController(orderService)
	// 商户端的读面与它共用同一个 service，但走的是另一条链：范围过滤不是可选参数，
	// 所以它不能是 OrderService 上多出来的一个空。
	merchantOrders := controller.NewMerchantOrderController(service.NewMerchantOrderService(orderService))
	// 售后与订单是两个控制器：它们挂在两棵路径树上（订单号 vs 售后单号），共用同一个
	// service。合并成一个会把两套分发搅在一起。
	adminAfterSales := controller.NewAdminAfterSaleController(orderService)
	miniappAfterSales := controller.NewMiniappAfterSaleController(orderService)
	// 超时关单必须有人做：没有它，到点未支付的订单会一直停在待支付，把它占的券和
	// 设备时段一直占着。周期与批量用默认值，见 worker 包里的说明。
	expiryWorker := worker.NewExpiryWorker(orderService, worker.DefaultSweepInterval, worker.DefaultSweepBatch)
	// 入向 gRPC 面：目前只有一条 CreateDeviceOrder（partner-service 验完厂商签名之后建设备单）。
	// 地址是配置里本来就有的 GRPCAddress（compose 里是 :9090，与 HTTP 分开一条口），
	// 这里多出来的只有「把实现挂上去」这一行——注册漏了的话，那条 RPC 在运行时是
	// Unimplemented，而调用方只会看到一次失败的重试。
	orderRPC := rpc.NewOrderService(orderService)

	// access token 24 小时，与 user-service / merchant-service / coupon-service /
	// coffee-machine-service 保持一致（见那几处的说明）。五处 auth.NewService 的取值
	// 必须一致。
	jwtService, err := auth.NewService([]byte(cfg.JWTSecret), cfg.JWTIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		log.Fatalf("order-service: init jwt: %v", err)
	}

	if err := server.RunWithOptions(cfg, runtime.Options{
		Database:    db,
		OwnDatabase: false,
		Registry:    reg,
		OwnRegistry: true,
		// Outbox：每一单的创建/支付/取消/关单都会在业务事务里追加一条事件，relay 投出去。
		// 这些事件是优惠券核销、履约出杯、会员权益生效的触发点——没有它们，订单成了事实
		// 却没有人知道。与消息队列同理，这里不配 publisher 时事件留在表里等重启再投。
		Outbox: messaging.NewPostgreSQL(pool.Pool()),
		// ConsumerInbox：支付结果会重投（broker 重连、处理完但没 ack 时崩溃都会），
		// 而落单是要写状态的。没有它，同一次支付会被应用两次。
		ConsumerInbox: messaging.NewPostgreSQL(pool.Pool()),
		// 消费的是支付结果。事件类型由 payment 侧决定，本服务只认自己认识的那几种，
		// 其余原样 ack（见 service.HandlePaymentEvent）。
		ConsumerHandler: orderService.HandlePaymentEvent,
		Workers:         []runtime.Runner{expiryWorker},
		HTTPRoutes: func(r *runtime.HTTPRouter) {
			// 后台：认证 → 平台账号闸门 → 实时授权 → 权限码。
			routes.RegisterAdmin(r, adminOrders, adminAfterSales, adminAuthorizer(jwtService, authorizer.Resolve, authorizationTimeout))
			// 小程序：只套认证。C 端的授权不是权限码能表达的（见 routes.RegisterMiniapp），
			// realm 与归属由 controller 和 service 判定。
			routes.RegisterMiniapp(r, miniappOrders, miniappAfterSales, auth.Middleware(jwtService))
			// 商户端：认证 → 实时取数据范围。到 RequirePermission 为止——商户域没有
			// 权限码，授权只由数据范围表达（见 routes.RegisterMerchant）。
			routes.RegisterMerchant(r, merchantOrders, merchantAuthorizer(jwtService, merchantAccess.Resolve, authorizationTimeout))
		},
		GRPCRoutes: func(s *kgrpc.Server) {
			orderv1.RegisterOrderServiceServer(s, orderRPC)
		},
		// 服务端验服务令牌：这个 gRPC 面只对内部开放（目前只有 partner-service 会调）。
		// 沿用 MERCHANT_INTERNAL_TOKEN——它在这个仓库里已经是「服务之间互认」的那一枚，
		// 与 membership-service / payment-service 同一条口径（变量名带着 merchant 是历史，
		// 改名要几个服务一起动，不在这次改动里做）。
		GRPCServerOptions: []kgrpc.ServerOption{
			auth.GRPCServerOption(jwtService, cfg.MerchantInternalToken),
		},
	}); err != nil {
		log.Fatalf("order-service: %v", err)
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
// 角色表之后，商户账号只有范围、没有角色）。也**没有单独的 realm 闸门**：
// MerchantMiddleware 在发起那次 gRPC 之前就先看 realm 与 tenant，非商户 realm 与空
// tenant 都在本地被挡成 403，再加一层只是把同一条规则写两遍。
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
