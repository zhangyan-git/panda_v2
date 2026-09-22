package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// HandlePaymentEvent 消费支付结果事件（方案 7.3）。
//
// 它是 runtime.Options.ConsumerHandler 的实现。返回值决定这条消息的归宿：nil 是 ack，
// 非 nil 是「没处理成功」——由平台重投或送进死信队列。所以这个函数里最主要的判断是
// 「哪些情况算处理成功」：
//
//   - 不认识的事件类型：ack。主题里可能有别的类型，那不是发给我们的。
//   - 已经处理过的订单：ack。broker 重连、消费者在 ack 前崩掉都会重投，重复回调是常态。
//   - 金额对不上、订单已经关了：报错。这两种要么是钱挂错了单，要么是数据坏了，必须有人看，
//     假装成功把它 ack 掉才是真正的丢钱。
func (s *OrderService) HandlePaymentEvent(ctx context.Context, event messaging.Envelope) error {
	switch event.EventType {
	case dto.EventPaymentSucceeded, dto.EventPaymentFailed:
		// 认识的事件，继续。
	case dto.EventPaymentRefundSucceeded, dto.EventPaymentRefundFailed:
		// 退款走自己的解码与推进（见 refund.go 的 HandleRefundEvent）。**在这里就分出去**，
		// 而不是往下走完了再用 if 拐一下：那两条事件的载荷是另一个结构体，共用一条解码路径
		// 只会让 DisallowUnknownFields 报出的字段名对不上真正出问题的那条消息。
		return s.HandleRefundEvent(ctx, event)
	default:
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	// 多发一个字段就报错：契约漂移要在联调时炸出来，而不是被静默忽略——被忽略的那次
	// 漂移可能正是 payment-service 换了分摊字段名，而我们还按旧名字在记账。
	decoder.DisallowUnknownFields()
	var payload dto.PaymentEventPayload
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPaymentEvent, err)
	}

	orderNo := strings.TrimSpace(payload.OrderNo)
	if orderNo == "" {
		return fmt.Errorf("%w: orderNo is required", ErrInvalidPaymentEvent)
	}

	params := repository.SettlePaymentParams{
		OrderNo:   orderNo,
		PaymentNo: strings.TrimSpace(payload.PaymentNo),
		Amount:    payload.Amount,
		// 一个值写两处：orders.payment_method 与 order_payment_lines.line_type。
		PaymentMethod:         strings.TrimSpace(payload.PaymentMethod),
		ProviderTransactionID: strings.TrimSpace(payload.ProviderTransactionID),
		RequestID:             event.EventID,
		TraceID:               event.TraceID,
		Outcome:               event.EventType,
		FailureCode:           payload.FailureCode,
		FailureMessage:        payload.FailureMessage,
	}
	if payload.PaidAtUnix > 0 {
		// 用渠道给的支付时间而不是 NOW()：补投或重放一条旧事件时，NOW() 会把「昨晚付的款」
		// 记成「刚才」。事件里有时间就用它，没有才退回 NOW()（仓储那边处理）。
		paidAt := time.Unix(payload.PaidAtUnix, 0).UTC()
		params.PaidAt = &paidAt
	}
	for _, funding := range payload.Fundings {
		params.Fundings = append(params.Fundings, repository.FundingLine{
			LineType:       strings.TrimSpace(funding.LineType),
			Amount:         funding.Amount,
			PaymentNo:      strings.TrimSpace(funding.PaymentNo),
			AccountEntryID: normalizedID(funding.AccountEntryID),
		})
	}

	_, _, err := s.repository.SettlePayment(ctx, params)
	return err
}
