package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// InitiatePaymentInput 是一次发起支付的输入。
type InitiatePaymentInput struct {
	// OrderNo 是订单号，不是内部 id：发起支付这条链路的两端（小程序、支付服务）手上都
	// 只有它（理由见 repository.FindOrderByNo）。
	OrderNo string
	// UserID 是调用方的身份，由 controller 从 access token 解出来。**归属判定用它**，
	// 不用请求里的任何字段。
	UserID string
	// PaymentMethodID 是用户选的支付方式（payment_methods.id）。
	PaymentMethodID string
	// RequestID 是这次发起的幂等号，原样交给支付服务做支付单的幂等键。
	RequestID string
	TraceID   string
}

// InitiatePayment 为一张订单发起一次支付。
//
// 这一段**只读订单、不改订单**：校验一遍「这单现在能不能付、该付多少」，然后把权威金额交给
// 支付域去建支付单。订单变 paid 是支付结果事件回来之后的事（HandlePaymentEvent），不在这
// 次请求里——所以这里不需要幂等事务，重发的幂等由支付单的 request_id 保证：同一把钥匙重发
// 拿到的是同一张支付单，不是第二张。
//
// 判据的顺序是**从便宜到贵、从能拒到不能拒**：形状 → 读单 → 归属 → 状态 → 金额 → 期限 →
// 下游。归属放在状态前面：别人的单必须回「不存在」，如果先判状态，一个已支付的他人订单会
// 回 409 而泄露「这个单号存在」。
func (s *OrderService) InitiatePayment(ctx context.Context, in InitiatePaymentInput) (*dto.PayAction, error) {
	orderNo := strings.TrimSpace(in.OrderNo)
	if orderNo == "" {
		return nil, ErrOrderNotFound
	}
	if strings.TrimSpace(in.PaymentMethodID) == "" {
		return nil, ErrPaymentMethodRequired
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return nil, ErrIdempotencyKeyRequired
	}

	order, err := s.repository.FindOrderByNo(ctx, orderNo)
	if err != nil {
		return nil, mapWriteError(err)
	}
	if order.UserID != in.UserID {
		// 同 GetOrderDetail 与 CancelOrder：别人的单回「不存在」。回 403 等于确认了
		// 「这个单号是存在的」，那本身就是一次可以枚举的信息泄露。
		return nil, ErrOrderNotFound
	}
	if order.Status != model.OrderStatusPendingPayment {
		return nil, ErrOrderNotPending
	}
	if order.PayableAmount <= 0 {
		// 应付额为 0 的单（券抵完了）不该走到收银台。放它过去，支付侧会拿 amount<=0 拒掉，
		// 而那句报错是从「钱」的角度说的；这里说得更准：这一单本来就不用付钱。
		return nil, ErrOrderNotPayable
	}
	if order.ExpiresAt != nil && !s.now().Before(*order.ExpiresAt) {
		// 过了付款期限、但超时关单还没扫到它：行上还是待支付，事实上已经不是了。放它过去会
		// 造出「钱收了、单被关掉」——支付侧那张预支付单的期限是**从发起支付算起**的，可以
		// 远晚于订单的 expires_at，于是用户能在一张即将作废的单上把钱付掉。
		// 宁可让用户重新下一单；这一单马上就真的被关掉了。
		return nil, ErrOrderNotPending
	}
	if s.payments == nil {
		// 这个部署没接支付域。与「支付服务这次没答上来」是同一个结论：稍后可能成。
		return nil, ErrPaymentServiceUnavailable
	}

	// 金额只从这里出去：order.PayableAmount 是下单时算好并冻结在订单上的权威值。
	// 请求体里没有任何能影响它的字段——这是「客户端改不了付款金额」的全部依据。
	return s.payments.Create(ctx, client.CreatePaymentInput{
		OrderID:         order.ID,
		OrderNo:         order.OrderNo,
		UserID:          order.UserID,
		Amount:          order.PayableAmount,
		PaymentMethodID: strings.TrimSpace(in.PaymentMethodID),
		RequestID:       strings.TrimSpace(in.RequestID),
	})
}
