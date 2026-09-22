package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// 退款这条链在订单侧是**两段**，中间隔着一次同步调用：
//
//	审核通过（ReviewAfterSale 的事务 A，落 approved）
//	  → 事务外调支付域建退款单（CreateRefund，幂等键是售后单号本身）
//	  → 事务 B 落 refunding，订单跟着进退款中
//	  → 等 payment-service 的退款结果事件，由 HandleRefundEvent 收口
//
// 中间那一步**必须**在事务外：渠道调用可能几秒钟，把它包在 PG 事务里就是让一次渠道抖动
// 顺手把订单那一行锁几秒（同 service/create.go 的三段事务，理由逐字相同）。
//
// 停下来的是哪一步都看得见：挂在事务 A 与调用之间 = approved（还没建单）；调用回来了但
// 事务 B 没成 = 还是 approved（下次重试会命中退款单的幂等键，不会退两次钱）。

// StartRefundInput 是「把一张已审核通过的售后单推去退款」的输入。它服务两个入口：
// 审核通过之后自动那一步（内部调用），以及后台那个重试按钮。
type StartRefundInput struct {
	AfterSaleNo string
	// ActorID 是推动这次退款的后台账号（审核人是审核人，重试是点重试的那个人）。
	ActorID string
	TraceID string
}

// StartRefund 把一张 approved 的售后单推去退款：建退款单（幂等）、落 refunding。
//
// 它是**重试出口**：审核通过之后如果那次发起没成（支付服务抖了一下、进程正好重启），
// 售后单会停在 approved。那个状态不是「卡住了」，而是一句准确的描述——同意退，但退款单
// 还没建成。后台在那一行上给一个「发起退款」按钮，按的就是这里。
//
// 重发是安全的：建退款单的幂等键就是售后单号（payment_refunds.after_sale_no 整表唯一），
// 已经建过的那一次会原样把同一张退款单还回来，不会再退一次钱。所以这条路既处理「上次压根
// 没建成」，也处理「建成了、但我们不知道」。
func (s *OrderService) StartRefund(ctx context.Context, in StartRefundInput) (*repository.AfterSaleRow, error) {
	in.AfterSaleNo = strings.TrimSpace(in.AfterSaleNo)
	if in.AfterSaleNo == "" {
		return nil, ErrAfterSaleNotFound
	}

	row, err := s.repository.FindAfterSaleByNo(ctx, in.AfterSaleNo)
	if err != nil {
		return nil, mapWriteError(err)
	}
	// 已经在退、或者已经退完了：这次点击（多半是列表刷新的空档里点的）什么都不用做。
	// 回现在的样子而不是报错——对点按钮的人来说，「它已经在退了」正是他想看到的结果。
	switch row.AfterSale.Status {
	case model.AfterSaleStatusRefunding, model.AfterSaleStatusRefunded:
		return row, nil
	case model.AfterSaleStatusApproved:
	default:
		return nil, fmt.Errorf("%w: after sale %s is %s",
			ErrAfterSaleNotApproved, row.AfterSale.AfterSaleNo, row.AfterSale.Status)
	}
	return s.startRefund(ctx, row, in.ActorID, in.TraceID)
}

// startRefund 是上面那条链的后两步，审核与重试两个入口共用。
//
// 传入的 row 必须是刚读出来的、状态还是 approved 的那一张：它的 PaymentNo 与 RefundAmount
// 是这次退款的全部依据，而从读到调之间隔着一次网络调用，所以**仓储那边在锁里会把状态再判
// 一遍**（StartRefund 的锁内检查），这里读到的那份只是用来构造请求。
func (s *OrderService) startRefund(ctx context.Context, row *repository.AfterSaleRow,
	actorID, traceID string) (*repository.AfterSaleRow, error) {
	item := row.AfterSale
	if strings.TrimSpace(row.PaymentNo) == "" {
		// 走到这里说明入口那道收窄被绕过了一次（老数据、或者有人手工改过库）。此时拒掉
		// 比拿一个空支付单号去问支付域好：那边只会回一句「支付单不存在」，而真正的问题是
		// 这一单根本没有支付单（见 repository.ErrOrderHasNoPayment）。
		return nil, fmt.Errorf("%w: order %s", ErrOrderHasNoPayment, item.OrderNo)
	}
	if s.payments == nil {
		// 这个部署没接支付域（New 那条注释里的第三种 nil）。与「支付服务这次没答上来」
		// 是同一个结论、同一个出口：稍后可能成，而审核**已经落库了**——那句话由
		// ErrRefundNotStarted 说出去。
		return nil, fmt.Errorf("%w: %w", ErrRefundNotStarted, ErrRefundServiceUnavailable)
	}

	outcome, err := s.payments.CreateRefund(ctx, client.CreateRefundInput{
		AfterSaleNo: item.AfterSaleNo,
		PaymentNo:   row.PaymentNo,
		Amount:      item.RefundAmount,
		Reason:      item.Reason,
		OrderLineID: derefString(item.OrderLineID),
	})
	if err != nil {
		// 两个 %w：**两个结论都要还查得到**。上面那个说「审核已生效、钱没上路」，下面那个
		// 说原因（被拒 / 没结论 / 故障）——controller 先认前者决定回哪句话，再认后者决定
		// 重试有没有意义。用 %v 串的话 errors.Is 到第二层就断了，原因只剩一句人话。
		return nil, fmt.Errorf("%w: %w", ErrRefundNotStarted, err)
	}

	updated, _, err := s.repository.StartRefund(ctx, repository.StartRefundParams{
		AfterSaleNo: item.AfterSaleNo,
		RefundNo:    outcome.RefundNo,
		ActorID:     actorID,
		RequestID:   traceID,
	})
	if err != nil {
		// 退款单**已经建成**了，只是我们没把这件事记下来。这与上面那条不是同一种失败：
		// 重试会命中幂等键拿回同一张单，然后接着把 refunding 写下去，所以提示是一样的。
		return nil, fmt.Errorf("%w: %w", ErrRefundNotStarted, err)
	}
	return updated, nil
}

// HandleRefundEvent 消费一条退款结果事件。
//
// 与 HandlePaymentEvent 同一处装配（同一个 ConsumerHandler 的 switch），理由也一样：这个
// 函数的返回值决定消息的归宿。退款这条路只有两种事件，而且**只有有结论时才发**——
// 渠道说「处理中」不发事件，所以这里不存在「收到一条什么都不能做的消息」那种情况。
//
// 三种「处理成功」：不认识的事件类型（主题里有别的类型）、已经落过结论的同号退款单
// （broker 重投是常态）、以及正常推进。其余一律报错让人看：退款单号对不上、售后单不在
// 退款流程里——这两种都说明有人绕过了一次状态机。
func (s *OrderService) HandleRefundEvent(ctx context.Context, event messaging.Envelope) error {
	succeeded := false
	switch event.EventType {
	case dto.EventPaymentRefundSucceeded:
		succeeded = true
	case dto.EventPaymentRefundFailed:
	default:
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	// 与支付事件同一条规矩（见 HandlePaymentEvent）：多发一个字段要在联调时炸出来。
	decoder.DisallowUnknownFields()
	var payload dto.RefundEventPayload
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPaymentEvent, err)
	}

	afterSaleNo := strings.TrimSpace(payload.AfterSaleNo)
	refundNo := strings.TrimSpace(payload.RefundNo)
	// 两个号都必须有，而且**缺一不可**：售后单号是这条链的落点（用退款单号反查会在
	// 「钱退成了、但 refund_no 那一步没回填」时找不到任何东西），退款单号是这条事件
	// 说的是哪一张退款单的凭据。少一个都只能是把消息投错了。
	if afterSaleNo == "" {
		return fmt.Errorf("%w: afterSaleNo is required", ErrInvalidPaymentEvent)
	}
	if refundNo == "" {
		return fmt.Errorf("%w: refundNo is required", ErrInvalidPaymentEvent)
	}

	params := repository.AdvanceRefundParams{
		AfterSaleNo:    afterSaleNo,
		RefundNo:       refundNo,
		Succeeded:      succeeded,
		FailureCode:    strings.TrimSpace(payload.FailureCode),
		FailureMessage: strings.TrimSpace(payload.FailureMessage),
		RequestID:      event.EventID,
		// 结论要发出去给账户域（追回福卡 / 解冻），没有 trace 那条消息在链路上断头。
		TraceID: event.TraceID,
	}
	if succeeded && payload.SucceededAtUnix > 0 {
		// 用渠道给的时刻而不是 NOW()：补投一条前几天的事件时，NOW() 会把「上周退的款」
		// 记成「刚才」——与支付那条同一条理由。
		params.RefundedAt = time.Unix(payload.SucceededAtUnix, 0).UTC()
	}

	// 错误一律原样返回，**不做任何「已知就 ack 掉」的豁免**。
	//
	// 重复投递不是错误：仓储那边认出「这张单已经有结论了」时返回 replayed=true 而不是
	// 报错。所以能走到这里的全是真问题——售后单不存在、状态被人绕过、退款单号对不上——
	// 而那三种都只能人来查。ack 掉它们等于让一次「钱退到别的地方去了」悄悄过去。
	_, _, err := s.repository.AdvanceRefund(ctx, params)
	return err
}
