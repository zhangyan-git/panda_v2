package rpc

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
)

// CreateRefund 是 order-service 在售后单**审核通过**那一刻走的那条路。
//
// 它是同步的，而且必须是同步的：渠道对退款是**同步应答**（规范里没有退款回调），而
// 「审核通过」这个动作在用户那边是立刻要有反馈的。这条契约与 CreatePayment 是同一个
// gRPC 服务上的两个方法，共用 serviceError 那一套状态码。
//
// **幂等键是 after_sale_no，不是 request_id**：一次退款只该发生一次，而售后单是它唯一的
// 出处（payment_refunds.after_sale_no 整表唯一）。调用方在某一步失败之后重发同一个售后单号
// 拿回的是同一张退款单——这正是「审核通过」那一步可以安全重试的全部依据。
//
// 渠道拒绝**不是这个 RPC 的错误**：它返回 status='failed' 与 failure_code/message，
// 与契约里那句「failed = 渠道拒绝或余额不足」对应。返回 error 会让 order-service 把一次
// 正常的业务拒绝当成服务端故障去重试，而重试退不了一笔被拒的钱。
func (s *PaymentService) CreateRefund(ctx context.Context, req *paymentv1.CreateRefundRequest) (*paymentv1.CreateRefundResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.payments.CreateRefund(ctx, service.RefundRequest{
		AfterSaleNo: req.GetAfterSaleNo(),
		PaymentNo:   req.GetPaymentNo(),
		Amount:      req.GetAmount(),
		Reason:      req.GetReason(),
		OrderLineID: req.GetOrderLineId(),
		// request_id 只用于流水与日志，不参与幂等。用 trace 而不是让调用方再传一个字段：
		// 这条路上真正需要「哪一次调用问出这个结论」的场合都发生在服务端排查里，而 trace
		// 本来就在 ctx 上。
		RequestID: audit.TraceIDFromContext(ctx),
		TraceID:   audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		return nil, serviceError(err)
	}
	return &paymentv1.CreateRefundResponse{
		RefundNo:         result.RefundNo,
		Status:           result.Status,
		ProviderRefundId: result.ProviderRefundID,
		FailureCode:      result.FailureCode,
		FailureMessage:   result.FailureMessage,
	}, nil
}
