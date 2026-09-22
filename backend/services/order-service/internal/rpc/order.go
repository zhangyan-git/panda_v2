// Package rpc 实现 order-service 的入向 gRPC 面。包名用 rpc 而不是 grpc，
// 以免遮蔽 google.golang.org/grpc。
//
// 这一层是**薄的**：验令牌、把 proto 消息翻成业务入参、把业务结论翻成响应与状态码，别的
// 什么都不做。它持的是同一个 *service.OrderService，与 HTTP 那两棵树共用一套判定——
// 两条路各写一份规则，就会出现「同一张单在小程序里下不了、在设备回调里下得了」。
//
// # 它有四条 RPC，都是**入向**的
//
// 前三条是「钱已在别处收过 → 建一张已支付的单」：CreateDeviceOrder（线下刷卡机）与
// CreatePickupOrder（取货码）由 partner-service 验完厂商签名之后调，CreateRenewalOrder
// （会员续费代扣）由 membership-service 在扣款成功之后调。第四条 GetOrder 是只读的订单摘要，
// 今天只有 membership-service 用。
//
// 这四条也是本服务**仅有的**被别人调用的 RPC——其余四条边（user、coffee-machine、payment、
// membership）都是本服务出网去问。走 gRPC 而不是 HTTP 的理由不是性能，是这两件事：
// partner-service 那边要的是「一次调用一个确定结论」（见下面那些分档表：两个「没有」各自的
// 档不同、可以重试的那一档又另算），以及这条路上没有用户令牌可带——它的身份是服务身份，
// 不是某个人。
//
// 三条写路径的分档表**各自独立**（createDeviceOrderError / createPickupOrderError /
// createRenewalOrderError），因为它们的调用方不同、可做的处置也不同：合作方拿到
// InvalidArgument 要去改报文，而 membership-service 拿到同样的档只能放弃这次投递。
// 合并成一张表的结果是每一档都要解释「在谁那边是什么意思」，而挑错档不会报错，只会让
// 调用方做错事。
//
// # 分档表是对着 partner-service 抄下来的，不是我们这边定的
//
// partner-service 的 internal/client/order.go 按状态码分档，internal/controller/openapi.go
// 的 writeOpenAPIError 再把它翻成对合作方说的话：InvalidArgument→400「这份报文不成立」、
// NotFound→404「这台机器没登记」、其余（含 Unavailable）→503「稍后再投」。所以下面每个
// status.Error 的选择都不是措辞问题——挑错档不会报错，只会给合作方一句误导的话。
//
// 只有服务令牌能调：这条 RPC 建的是订单、直接落成已支付，任何拿着用户令牌的调用方都
// 不该够得着它（见 auth.RequireService 与 cmd/main.go 上的 GRPCServerOptions）。
package rpc

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
	orderv1 "github.com/panda-dev/panda-v2/contracts/proto/order/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// OrderService 是订单服务对内的 gRPC 面。
type OrderService struct {
	orderv1.UnimplementedOrderServiceServer

	orders *service.OrderService
}

// NewOrderService 构造 gRPC 服务。
func NewOrderService(orders *service.OrderService) *OrderService {
	return &OrderService{orders: orders}
}

// CreateDeviceOrder 落一笔线下刷卡机卖出去的订单（钱已经收过了）。
//
// # 结论只有四种，合作方各自知道该做什么
//
//	InvalidArgument  报文要改。两个来源：三个必填里有空的（对方单号/序列号/饮品编号），
//	                 以及**这台机器上没有这个编号的饮品**——后者看着像「没有」，但对
//	                 合作方该做的事与前者一样：换一个编号再投。partner-service 拿这一档
//	                 回 400「the device order request is invalid」（见它的
//	                 client/order.go：InvalidArgument 的注释就是「字段缺失、饮品匹配不到」）。
//	NotFound         设备序列号查不到，**只此一种**。partner-service 拿它回 404
//	                 「device is not registered」，那句话只有在真的是设备没登记时才成立。
//	                 饮品编号在这一点上**不能**走这一档：那会让合作方收到「这台机器没登记」
//	                 而跑去重新同步一台早就登记好的机器。
//	Unavailable      我们没问到下游（设备或饮品读不到）。**这是一个可以重投的结论**，所以
//	                 它必须与上面两档分开——否则一次下游抖动会让合作方以为这杯下架了，
//	                 而钱已经收了。
//	Internal         我们自己的故障。同样可以退避重试，但与下游抖动分开，方便定位。
//
// **状态消息不回传原始错误文本**：里面的表名与 SQL 片段不该跨服务边界跑（与
// membership-service 那几条同一条规矩）。排查要用的信息在服务端日志里。
//
// # 幂等命中的返回
//
// 对方重投（网络重放、机器自己重试）时返回的是既有那张单，created=false，**不报错**：
// 报错会让它一直重投下去，而这一单其实早就落好了。调用方据此区分「新建」与「重投」。
func (s *OrderService) CreateDeviceOrder(ctx context.Context, req *orderv1.CreateDeviceOrderRequest) (*orderv1.CreateDeviceOrderResponse, error) {
	// 只认服务令牌。这面只对内部开放，与 payment-service 的 CreatePayment 同一条规矩：
	// 它建的是「已经收过钱的订单」，用户令牌在这里没有任何意义，也不能有用。
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}

	result, err := s.orders.CreateDeviceOrder(ctx, service.CreateDeviceOrderInput{
		ThirdPartyOrderNo: req.GetThirdPartyOrderNo(),
		DeviceSerial:      req.GetDeviceSerial(),
		DrinkCode:         req.GetDrinkCode(),
		Amount:            req.GetAmount(),
		BrewFailed:        req.GetBrewFailed(),
		Remark:            req.GetRemark(),
	})
	if err != nil {
		return nil, createDeviceOrderError(err)
	}
	return &orderv1.CreateDeviceOrderResponse{
		OrderId: result.OrderID,
		OrderNo: result.OrderNo,
		Created: result.Created,
	}, nil
}

// CreatePickupOrder 记一笔取货码订单（方案 §四的第二条路）：钱从这台设备的咖啡余额里扣。
//
// # 与 CreateDeviceOrder 的差别在这一层的分档表上
//
// 那条路只有三种结论（报文要改、设备没登记、没问到），因为收款在机器上、我们只负责记账。
// 这条路多出来的是**收款本身**，于是多了三档，而合作方对它们的处置各不相同：
//
//	PermissionDenied   取货码不对（或者这台机器没配过码）。重投无用——换一个码。
//	                   机器前面那句该是「取货码不对」，不是「系统错误」。
//	FailedPrecondition 这台机器上余额不够。也是重投无用，而且是**确定的结论**：
//	                   咖啡机域在同一个事务里判的，拒了就一个字段都没写（钱没动）。
//	                   ⚠️ 它**不能**降级成 Unavailable——那会让合作方一直重投一次
//	                   永远不可能成功的扣款，而机器前面那个人等的是「余额不足，请充值」。
//
// 其余与那条路一致：InvalidArgument 是报文要改（含这一杯没取货码价）、NotFound 只给
// 「设备没登记」一种情况、Unavailable 是我们没问到（这条唯一可以重投的一档）、Internal
// 是我们自己的故障。
//
// # 状态消息不回传原始错误文本
//
// 与 CreateDeviceOrder 同一条规矩：里面的表名与 SQL 片段不跨服务边界跑。
func (s *OrderService) CreatePickupOrder(ctx context.Context, req *orderv1.CreatePickupOrderRequest) (*orderv1.CreatePickupOrderResponse, error) {
	// 只认服务令牌，理由与 CreateDeviceOrder 那一条逐字相同：这条 RPC 会**动别人账上的
	// 钱**（设备余额），比建一张单更不该让拿着用户令牌的调用方够得着。
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}

	result, err := s.orders.CreatePickupOrder(ctx, service.CreatePickupOrderInput{
		ThirdPartyOrderNo: req.GetThirdPartyOrderNo(),
		DeviceSerial:      req.GetDeviceSerial(),
		DrinkCode:         req.GetDrinkCode(),
		// 验证码原样传下去，一个字符都不动（判空、trim、比对都不在这一层做，见 proto 的说明）。
		PickupPassword: req.GetPickupPassword(),
		Remark:         req.GetRemark(),
	})
	if err != nil {
		return nil, createPickupOrderError(err)
	}
	return &orderv1.CreatePickupOrderResponse{
		OrderId: result.OrderID,
		OrderNo: result.OrderNo,
		Created: result.Created,
	}, nil
}

// CreateRenewalOrder 落一笔会员续费的订单（钱已经被微信代扣收走了）。
//
// # 调用方是 membership-service，不是合作方
//
// 所以这一张分档表与前两张**不能共用**：前两张的 InvalidArgument 落到 partner-service 那里
// 是一句「报文要改」，那边确实能改；而这里的调用方手里那个渠道流水号是**微信给的**，改不了
// ——它拿到任何一档都只有一件事可做：放弃这次投递（返回非 nil，让平台重投到上限后进死信，
// 由人工去核这一笔）。
//
// 这不是说分档就没意义了。它决定的是**这次故障在日志与死信里长什么样**：
//
//	InvalidArgument    调用方把这次调用组错了（缺用户、金额为负、没带套餐快照）。是本服务的
//	                   调用方写坏了，而不是渠道或我们这边出了故障。
//	FailedPrecondition 这个渠道流水号上已经挂着一张**别的种类**的订单。这是一次必须立刻查的
//	                   串线（单号空间与设备那两条共用），不是重投能好的事。
//	                   ⚠️ 与设备那两条**有意不同档**：那边这一类回 InvalidArgument（合作方换
//	                   一个单号重投就好），这边换不了单号，回 InvalidArgument 会让调用方以为
//	                   是自己报文写错了。
//	Internal           我们自己的故障（含库上的约束没被满足）。可以退避重试。
//
// **这里没有 Unavailable**：这条路上本服务不调任何下游（金额与套餐快照都由调用方给全，
// 见 service.CreateRenewalOrder 的说明），所以不存在「下游抖了，稍后重投可能就好」这一档。
// 真出现一个我们没预料到的错误，它会落到 Internal——那对调用方是同一件事（重投），
// 但对排查的人是另一件事（不是下游的锅）。
//
// 状态消息不回传原始错误文本，与上面两条同一条规矩。
func (s *OrderService) CreateRenewalOrder(ctx context.Context, req *orderv1.CreateRenewalOrderRequest) (*orderv1.CreateRenewalOrderResponse, error) {
	// 只认服务令牌，理由与前两条逐字相同：这条 RPC 建的是「已经收过钱的订单」，
	// 任何拿着用户令牌的调用方都不该够得着它。
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}

	result, err := s.orders.CreateRenewalOrder(ctx, service.CreateRenewalOrderInput{
		ThirdPartyOrderNo: req.GetThirdPartyOrderNo(),
		UserID:            req.GetUserId(),
		Amount:            req.GetAmount(),
		Plan:              membershipPlanSnapshot(req.GetPlan()),
		MembershipID:      req.GetMembershipId(),
		Remark:            req.GetRemark(),
	})
	if err != nil {
		return nil, createRenewalOrderError(err)
	}
	return &orderv1.CreateRenewalOrderResponse{
		OrderId: result.OrderID,
		OrderNo: result.OrderNo,
		Created: result.Created,
	}, nil
}

// membershipPlanSnapshot 把 proto 里那份套餐快照翻成业务层的形状。
//
// req.GetPlan() 为 nil 时返回零值——service 那一层会把「没有 plan_id」判成
// ErrRenewalPlanRequired（InvalidArgument），所以这里不需要先判一次 nil 再报一次错：
// 校验只有一处，就是 service。
func membershipPlanSnapshot(plan *orderv1.MembershipPlanSnapshot) dto.MembershipPlanSnapshot {
	if plan == nil {
		return dto.MembershipPlanSnapshot{}
	}
	return dto.MembershipPlanSnapshot{
		PlanID:                      plan.GetPlanId(),
		PlanCode:                    plan.GetPlanCode(),
		PlanName:                    plan.GetPlanName(),
		PriceCents:                  plan.GetPriceCents(),
		Period:                      plan.GetPeriod(),
		PeriodCount:                 plan.GetPeriodCount(),
		AutoRenew:                   plan.GetAutoRenew(),
		MemberPriceMode:             plan.GetMemberPriceMode(),
		MemberPriceCouponTemplateID: plan.GetMemberPriceCouponTemplateId(),
		MemberPriceCouponsPerPeriod: plan.GetMemberPriceCouponsPerPeriod(),
	}
}

// createRenewalOrderError 把续费建单的业务结论翻成 gRPC 状态码。
//
// 分档的取舍逐条写在 CreateRenewalOrder 的说明里——这一张表比前两张**短**，因为这条路
// 上没有「问不到下游」那一类，也没有调用方能改的报文。
func createRenewalOrderError(err error) error {
	switch {
	case errors.Is(err, service.ErrThirdPartyOrderNoRequired):
		return status.Error(codes.InvalidArgument, "thirdPartyOrderNo is required")
	case errors.Is(err, service.ErrUserRequired):
		return status.Error(codes.InvalidArgument, "userId is required")
	case errors.Is(err, service.ErrRenewalAmountInvalid):
		return status.Error(codes.InvalidArgument, "renewal amount must not be negative")
	case errors.Is(err, service.ErrRenewalPlanRequired):
		return status.Error(codes.InvalidArgument, "the membership plan snapshot is required")
	// 这个流水号上已经挂着**别的种类**的订单。详见 CreateRenewalOrder 的说明：这一档与
	// 设备那两条不同，那些调用方换个单号就行，这个换不了——所以它是 FailedPrecondition
	// 而不是 InvalidArgument。
	case errors.Is(err, service.ErrThirdPartyOrderNoTaken):
		return status.Error(codes.FailedPrecondition, "thirdPartyOrderNo is already used by a different kind of order")
	case service.IsValidationError(err):
		// 兜底，理由与上面两张表相同：将来新增一条校验而这里忘了加 case，至少不会变成
		// Internal——那会让「调用方写错了」看起来像我们的故障。
		return status.Error(codes.InvalidArgument, "invalid request")
	default:
		return status.Error(codes.Internal, "order service error")
	}
}

// GetOrder 按订单 ID 取一张订单的摘要。**只读**，今天只有 membership-service 用
// （订阅详情的「首月支付」那一段要订单号、状态、实付、支付方式、支付时间与支付单号）。
//
// # 为什么它是个 gRPC 而不是让它去查库
//
// 订单事实归本服务，payment_no 也是值引用；让 membership-service 自己读 order 库等于绕开
// 领域边界（同 service 里那条「归属看这库是谁的」的规矩）。这一条读很便宜，也不引入新的
// 一致性要求——首月那段本来就只是展示。
//
// # 不校验归属
//
// 与终端用户那两条读不同，这里**没有 userID 这一档**：调用方是内部服务，而它拿着的
// first_payment_order_id 本来就来自数据库里那行订阅。真要有越权问题，也先是那行数据被改坏，
// 而这一条 RPC 给它的是「你手里那个 id 对应的订单」，不多给任何别的东西。
//
// # 分档
//
//	InvalidArgument  order_id 为空或不是 uuid。**不是** NotFound：形状不对的标识说明
//	                调用方手里那个值本身就坏了（比如空串），而它会把这个结论记成「订单
//	                不存在」——那会让一次数据损坏看起来像一次正常的缺失。
//	NotFound        形状对、库里没有。
//	Internal        我们自己的故障。
func (s *OrderService) GetOrder(ctx context.Context, req *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}

	orderID, err := parseOrderID(req.GetOrderId())
	if err != nil {
		return nil, err
	}

	order, err := s.orders.FindOrder(ctx, orderID)
	if err != nil {
		if errors.Is(err, service.ErrOrderNotFound) {
			return nil, status.Error(codes.NotFound, "order not found")
		}
		return nil, status.Error(codes.Internal, "order service error")
	}

	resp := &orderv1.GetOrderResponse{
		OrderId:       order.ID,
		OrderNo:       order.OrderNo,
		UserId:        userIDOrEmpty(order.UserID),
		Source:        order.Source,
		Status:        order.Status,
		PaidAmount:    order.PaidAmount,
		PaymentMethod: order.PaymentMethod,
		PaymentNo:     order.PaymentNo,
	}
	// 时间用 RFC3339 字符串（proto 的既定约定，见 order.proto 里 paid_at 的说明）。
	// 未支付时留空串——零值时间编成 "0001-01-01T00:00:00Z" 会被调用方当成一个真的时刻。
	if order.PaidAt != nil {
		resp.PaidAt = order.PaidAt.UTC().Format(time.RFC3339)
	}
	return resp, nil
}

// parseOrderID 校验并规整请求里那个订单 ID，形状不对时回一个 InvalidArgument。
//
// 单独拎出来是为了它能被直接测：这个判定在 handler 里排在 auth 之后，而 rpc 这一层没有
// 「造一个服务身份」的捷径（auth 的 context key 不导出），所以放在 handler 里的分支只能靠
// 一个真起的 gRPC 服务器才走得到。
//
// **空串与形状不对是同一档**：uuid.Parse("") 本来就报错，不用先把空串单拎出来。归
// InvalidArgument 而不是 NotFound——形状不对说明调用方手里那个值本身就坏了（比如空串），
// 把它记成「订单不存在」会让一次数据损坏看起来像一次正常的缺失。
func parseOrderID(raw string) (string, error) {
	orderID := strings.TrimSpace(raw)
	if _, err := uuid.Parse(orderID); err != nil {
		return "", status.Error(codes.InvalidArgument, "orderId must be a uuid")
	}
	return orderID, nil
}

// userIDOrEmpty 把一个可空的用户 ID 读成 proto 要的字符串（NULL 读成空串）。
//
// proto 的字段是 string 而不是 optional：这一格在 GetOrder 里只服务「显示首月那笔单的
// 归属」，而**没有用户**（设备单，user_id 为 NULL）与空串在这一格上不可能混淆——设备单
// 根本不会走到这条 RPC 上（首月支付那一笔有用户）。所以这里不需要再造一个 oneof 去区分
// 「没有」与「空」。
func userIDOrEmpty(userID *string) string {
	if userID == nil {
		return ""
	}
	return *userID
}

// createPickupOrderError 把取货码那条路的业务结论翻成 gRPC 状态码。
//
// 与 createDeviceOrderError 分开而不是合并：两条路的重合只有「报文不合法」与「设备没登记」
// 那几档，而这条路多出来的三档（验证码、余额、没配价）在那条路上根本不存在——合并的结果是
// 一张谁都读不完的 switch，而它的每一支都只在其中一条路上成立。
//
// 分档的基准仍然是 partner-service 那张表（见包注释）：挑错档不会报错，只会给合作方一句
// 误导的话，或者让他一直重投一条永远不会成功的取货。
func createPickupOrderError(err error) error {
	switch {
	case errors.Is(err, service.ErrThirdPartyOrderNoRequired):
		return status.Error(codes.InvalidArgument, "thirdPartyOrderNo is required")
	case errors.Is(err, service.ErrDeviceSerialRequired):
		return status.Error(codes.InvalidArgument, "deviceSerial is required")
	case errors.Is(err, service.ErrDrinkCodeRequired):
		return status.Error(codes.InvalidArgument, "drinkCode is required")
	// 饮品编号对不上是 InvalidArgument，理由与刷卡机那条相同（见 createDeviceOrderError）。
	case errors.Is(err, service.ErrDrinkNotFound):
		return status.Error(codes.InvalidArgument, "drinkCode does not match a drink on this device")
	// 这一杯没有取货码价、也没有目录价可回落。是 InvalidArgument 而不是 Unavailable：重投
	// 一百次它还是没价，而合作方要做的是**换一杯**（或者来找我们定价）——回 503 会让它一直
	// 重投，而机器前面那个人一直等。
	case errors.Is(err, service.ErrDrinkNotPickupPriced):
		return status.Error(codes.InvalidArgument, "drink has no pickup code price")
	// 这个单号已经被**另一类**设备单占了（刷卡机与取货码共用一个单号空间）。归 InvalidArgument，
	// 理由与刷卡机那条相同：合作方要换一个单号。
	//
	// 这一档发生在**扣款之前**：那条检查在 CreatePickupOrder 里排在最前面（钱一分没动），
	// 所以合作方拿到的是一句「报文要改」，而不是一笔建不出单的扣款。
	case errors.Is(err, service.ErrThirdPartyOrderNoTaken):
		return status.Error(codes.InvalidArgument, "thirdPartyOrderNo is already used by a different kind of order")
	case service.IsValidationError(err):
		// 兜底，理由同 createDeviceOrderError：将来新增一条校验而这里忘了加 case，至少不会
		// 变成 Internal——那会让「请求写错了」看起来像我们的故障。
		return status.Error(codes.InvalidArgument, "invalid request")
	// 验证码：两种（不对 / 没配）一个档，与咖啡机域那边的合并口径一致。
	case errors.Is(err, service.ErrDeviceBalancePasswordRejected):
		return status.Error(codes.PermissionDenied, "pickup password is not accepted")
	// 余额不够：确定的结论，且咖啡机域一个字段都没写。**不能**落到 Unavailable。
	case errors.Is(err, service.ErrDeviceBalanceNotEnough):
		return status.Error(codes.FailedPrecondition, "device balance is not enough")
	// 扣减那边报「参数不成立」：本服务已经校验过形状，走到这里多半是金额或设备号在竞态中
	// 变了，按「报文要改」回，与合作方要做的事（重新对一遍报文）一致。
	case errors.Is(err, service.ErrDeviceBalanceRejected):
		return status.Error(codes.InvalidArgument, "the deduction was rejected")
	// 设备在两次调用之间没了，与读那一步的 NotFound 同一个结论。
	case errors.Is(err, service.ErrDeviceBalanceDeviceMissing):
		return status.Error(codes.NotFound, "device not found")
	case errors.Is(err, service.ErrDeviceNotFound):
		return status.Error(codes.NotFound, "device not found")
	case errors.Is(err, service.ErrDeviceLookupUnavailable):
		return status.Error(codes.Unavailable, "device lookup is unavailable")
	case errors.Is(err, service.ErrDrinkLookupUnavailable):
		return status.Error(codes.Unavailable, "drink lookup is unavailable")
	case errors.Is(err, service.ErrDeviceBalanceServiceUnavailable):
		return status.Error(codes.Unavailable, "device balance service is unavailable")
	default:
		return status.Error(codes.Internal, "order service error")
	}
}

// createDeviceOrderError 把业务结论翻成 gRPC 状态码。
//
// 三种「没有」必须分得开——设备没有（NotFound）、饮品没有（InvalidArgument）、没问到下游
// （Unavailable）。前两种是确定的答案（重发一样、要动的东西不同），第三种是我们这边的抖动
// （重发可能就好）。混成一种，合作方那边就会做出错的处置：把「饮品编号不对」说成「设备没
// 登记」，它会去重新同步一台已经登记好的机器，而这一单永远建不出来。
//
// 分档的基准是 partner-service 那张表（见包注释），不是这里的措辞。
func createDeviceOrderError(err error) error {
	switch {
	case errors.Is(err, service.ErrThirdPartyOrderNoRequired):
		return status.Error(codes.InvalidArgument, "thirdPartyOrderNo is required")
	case errors.Is(err, service.ErrDeviceSerialRequired):
		return status.Error(codes.InvalidArgument, "deviceSerial is required")
	case errors.Is(err, service.ErrDrinkCodeRequired):
		return status.Error(codes.InvalidArgument, "drinkCode is required")
	// 饮品编号对不上**是** InvalidArgument，不是 NotFound：报文里的那个编号要换，而
	// partner-service 的 404 只有一句「这台机器没登记」，用在那里是误报。
	case errors.Is(err, service.ErrDrinkNotFound):
		return status.Error(codes.InvalidArgument, "drinkCode does not match a drink on this device")
	// 这个单号已经被**另一类**设备单占了（刷卡机与取货码共用一个单号空间）。归 InvalidArgument：
	// 合作方要做的是**换一个单号**重投，而重投同一个单号一百次也还是这一句——回 Unavailable
	// 会让它一直重投一条永远不可能成功的报文。
	case errors.Is(err, service.ErrThirdPartyOrderNoTaken):
		return status.Error(codes.InvalidArgument, "thirdPartyOrderNo is already used by a different kind of order")
	case service.IsValidationError(err):
		// 兜底：将来这条路新增一条校验而这里忘了加 case，至少不会变成 Internal——
		// 那会让「请求写错了」看起来像我们的故障。
		return status.Error(codes.InvalidArgument, "invalid request")
	case errors.Is(err, service.ErrDeviceNotFound):
		return status.Error(codes.NotFound, "device not found")
	case errors.Is(err, service.ErrDeviceLookupUnavailable):
		return status.Error(codes.Unavailable, "device lookup is unavailable")
	case errors.Is(err, service.ErrDrinkLookupUnavailable):
		return status.Error(codes.Unavailable, "drink lookup is unavailable")
	default:
		return status.Error(codes.Internal, "order service error")
	}
}
