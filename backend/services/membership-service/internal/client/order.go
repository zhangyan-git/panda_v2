package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	orderv1 "github.com/panda-dev/panda-v2/contracts/proto/order/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 订单域在本服务这条路上的四种答复。分档判据与 payment 那个客户端逐字相同：**调用方接下来该做
// 什么**。
//
//	ErrRenewalOrderRejected   改请求或改部署，原样重发没有用
//	ErrRenewalOrderConflict   同一把幂等钥匙被换过请求体（订单域回 FailedPrecondition）
//	ErrOrderNotFound          按 id 查订单查无此单 —— 是一次结论
//	ErrOrderUnavailable       没问到，等一会儿原样重投
//
// 前三个是「有结论」，最后一个是「没结论」。把没结论的当成结论在这条路上尤其贵：一次超时被读成
// 「建单失败」，而订单可能已经建好了——记账上就多出一张谁也不认识的续费单。
var (
	// ErrRenewalOrderRejected：订单域不接受这份报文（请求不合法），或者**这个渠道流水号已经挂在
	// 一张别种的订单上了**（FailedPrecondition）。
	//
	// 后一条是这条路上唯一一个「真的有结论」的失败：同一个流水号在订单域属于一张设备单或别的
	// 单，说明我们手里的号错了。**重投不会让它变对**——而这个判断归订单域做，本服务不预先核一遍
	// （那样两边各有一份判据，迟早走偏）。
	ErrRenewalOrderRejected = errors.New("order service rejected the renewal order")
	// ErrRenewalOrderConflict：同一把幂等钥匙被换过请求体（Aborted / AlreadyExists）。
	ErrRenewalOrderConflict = errors.New("renewal order conflicts with an earlier attempt")
	// ErrOrderNotFound：按订单 id 查不到（NotFound）。它只可能来自 GetOrder——建单那条路上
	// NotFound 不是一个正常答复，所以归进「没问到」。
	ErrOrderNotFound = errors.New("order not found")
	// ErrOrderUnavailable：订单域没答上来（连不上、内部错误、超时、应答是空的、版本错配）。
	ErrOrderUnavailable = errors.New("order service is unavailable")
)

// OrderClient 是本服务对订单域的两个动作：记一张续费单，以及回读一张订单的摘要。
//
// 带的是**服务令牌**（platform/auth.WithServiceToken）：这次调用代表会员域去告诉订单域一个事实
// （「这一期扣到钱了」），不代表某个用户。用户是谁由请求体里那个 userID 说——而它抄自本域的
// 订阅行，不是客户端给的（见 service.recordRenewalOrder）。
//
// 它与 payment.go 的 AgreementClient 是同一个形状：本层加超时（不靠调用方的 ctx——那条 ctx
// 来自消息消费，平台的收件箱 lease 只有 1 分钟，不能拿它当上限）。
type OrderClient struct {
	orders  orderv1.OrderServiceClient
	token   string
	timeout time.Duration
}

// NewOrderClient 复用调用方那条连接：main 只拨一次，两个调用点共享同一个 *grpc.ClientConn。
//
// 三个参数都拒绝空值/零值，与这个包里另外两个客户端同一条理由：一个没有令牌的客户端会在运行时
// 被订单域拒掉（Unauthenticated），而那看起来像一次权限事故而不是一次装配错误。
func NewOrderClient(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*OrderClient, error) {
	if conn == nil {
		return nil, errors.New("order service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("order service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("order service timeout must be positive")
	}
	return &OrderClient{orders: orderv1.NewOrderServiceClient(conn), token: token, timeout: timeout}, nil
}

// CreateRenewal 把一期扣款记成一张订单。
//
// # 它是幂等的，幂等键是渠道流水号
//
// 同一个流水号重投只会拿回同一张单（created=false）。所以调用方在**任何**拿不准的情况下原样重发
// 都是安全的——包括「上一次其实成功了、只是响应没回来」。这正是它必须被放在结算**之前**的前提：
// 见 service.recordRenewalOrder 里那段顺序说明。
func (c *OrderClient) CreateRenewal(ctx context.Context, in dto.RenewalOrderParams) (*dto.RenewalOrder, error) {
	if c == nil || c.orders == nil {
		return nil, errors.New("order client is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.orders.CreateRenewalOrder(ctx, &orderv1.CreateRenewalOrderRequest{
		ThirdPartyOrderNo: in.ThirdPartyOrderNo,
		UserId:            in.UserID,
		Amount:            in.Amount,
		Plan:              planSnapshotMessage(in.Plan),
		MembershipId:      in.MembershipID,
		Remark:            in.Remark,
	})
	if err != nil {
		return nil, mapOrderError(err)
	}
	// 一次「建了单却没给单号」的回答是坏的，不是一张没单号的订单：调用方拿不到 order_id 就没法
	// 把它写进流水与事件，而那一期扣款从此在库里查不到来源。当成没问到——重投安全（幂等键在
	// 渠道流水号上，命中的是同一张单）。
	if resp == nil || strings.TrimSpace(resp.GetOrderId()) == "" {
		return nil, fmt.Errorf("%w: order service returned an empty renewal order", ErrOrderUnavailable)
	}
	return &dto.RenewalOrder{
		OrderID: resp.GetOrderId(),
		OrderNo: resp.GetOrderNo(),
		Created: resp.GetCreated(),
	}, nil
}

// GetOrder 按订单 id 读一张订单的摘要（后台「首月支付信息」那一块的第一跳）。
//
// 查无此单回 ErrOrderNotFound 而**不是一个空的摘要**：调用方手里那个 id 抄自订阅行
// （first_payment_order_id），对不上说明那行数据坏了，而它对这两种结论的处置不同——前者要报警，
// 后者是正常的空档（那一列今天恒为空）。
func (c *OrderClient) GetOrder(ctx context.Context, orderID string) (*dto.OrderSummary, error) {
	if c == nil || c.orders == nil {
		return nil, errors.New("order client is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.orders.GetOrder(ctx, &orderv1.GetOrderRequest{OrderId: orderID})
	if err != nil {
		return nil, mapOrderError(err)
	}
	if resp == nil || strings.TrimSpace(resp.GetOrderId()) == "" {
		return nil, fmt.Errorf("%w: order service returned an empty order", ErrOrderUnavailable)
	}
	return &dto.OrderSummary{
		OrderID:       resp.GetOrderId(),
		OrderNo:       resp.GetOrderNo(),
		UserID:        resp.GetUserId(),
		Source:        resp.GetSource(),
		Status:        resp.GetStatus(),
		PaidAmount:    resp.GetPaidAmount(),
		PaymentMethod: resp.GetPaymentMethod(),
		PaymentNo:     resp.GetPaymentNo(),
		PaidAt:        resp.GetPaidAt(),
	}, nil
}

// planSnapshotMessage 把本域的套餐快照翻成订单域那个嵌套消息。
//
// 十个字段一个不少地照抄，中间不补默认值、不做换算：这份快照会被**原样**写进
// order_lines.membership_plan_snapshot，是这一期买了什么的唯一记录。
func planSnapshotMessage(plan dto.RenewalPlanSnapshot) *orderv1.MembershipPlanSnapshot {
	return &orderv1.MembershipPlanSnapshot{
		PlanId:                      plan.PlanID,
		PlanCode:                    plan.PlanCode,
		PlanName:                    plan.PlanName,
		PriceCents:                  plan.PriceCents,
		Period:                      plan.Period,
		PeriodCount:                 plan.PeriodCount,
		AutoRenew:                   plan.AutoRenew,
		MemberPriceMode:             plan.MemberPriceMode,
		MemberPriceCouponTemplateId: plan.MemberPriceCouponTemplateID,
		MemberPriceCouponsPerPeriod: plan.MemberPriceCouponsPerPeriod,
	}
}

// mapOrderError 把 gRPC 状态码翻成上面那四个结论。
//
// 与 payment 那个 mapAgreementError 一一对应，中间不重新解释一遍。**只有「有结论」的那几档带回
// 状态消息**：那些句子是订单域写好的固定说法（哪一个流水号、撞上了什么样的单），不是原始错误
// 文本；「没结论」那一档只留一个我们知道的结论，它说的话对调用方没有可执行的信息。
//
// 建单那条路上 NotFound 不是一个正常答复（订单域只有 GetOrder 会回它），所以它与其余几档一起
// 归进「没问到」——按 id 查订单那一侧会把这个码单独接成 ErrOrderNotFound（见 mapGetOrderError）。
func mapOrderError(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", ErrRenewalOrderRejected, status.Convert(err).Message())
	case codes.FailedPrecondition:
		// 订单域的「这个流水号挂在一张别种的单上」。它是有结论的，但**不是报文的格式问题**：
		// 重投不会让它变对，得有人去看那个号是从哪儿来的。
		return fmt.Errorf("%w: %s", ErrRenewalOrderRejected, status.Convert(err).Message())
	case codes.Aborted, codes.AlreadyExists:
		return fmt.Errorf("%w: %s", ErrRenewalOrderConflict, status.Convert(err).Message())
	case codes.NotFound:
		return ErrOrderNotFound
	case codes.Unimplemented:
		// 「这一档对面压根没有」——今天只可能是版本错配（我们比订单域新）。它长得像故障，就该
		// 长得像故障：归进「报文要改」会让人去翻数据，而真正该做的是对齐部署。
		return fmt.Errorf("%w: the order service does not implement this call", ErrOrderUnavailable)
	default:
		// Unavailable / DeadlineExceeded / Internal 都在这里。DeadlineExceeded 是**我们自己**
		// 那条线：订单域可能已经把单建好了，而我们已经放弃这次调用。所以它是「没结论」而不是
		// 「没建」——重投安全（幂等键在渠道流水号上）。
		return ErrOrderUnavailable
	}
}
