package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// RefundReconcileStats 是一轮退款查询的结果计数。
//
// 分档与 ReconcileStats 逐字同形，理由也是同一个：**「问了，还没结」与「压根没问成」必须
// 分开**。前者是正常的（托管退款就是这样，渠道要几天），后者是坏了（渠道没适配器、凭据没配、
// 落库失败）。并成一个数字的话，「一轮问了 20 笔、0 推进」这句话读不出任何东西。
type RefundReconcileStats struct {
	Scanned      int
	Succeeded    int
	Failed       int
	Inconclusive int
	Skipped      int
}

// ReconcileProcessingRefunds 向渠道逐笔问一遍「这些发起过、但一直没有结论的退款到底退成了
// 没有」，把有结论的推到位。
//
// # 它比查单那条路更不可省
//
// 支付的结论至少还有回调这条路（虽然回调会丢，所以才有查单兜底）。**退款的回调根本不存在**
// ——银联商务的退款是同步应答，规范里没有退款通知这回事。所以一笔退款一旦在应答里是
// PROCESSING / UNKNOWN，或者那次调用超时了，**除了问没有第二条路能拿到结论**。没有这条任务，
// 那些退款单会永远停在 processing，售后单永远停在 refunding，钱退没退回去没人知道。
//
// # 有结论才动，没结论什么都不做
//
// 与 settleByQuery / ReconcilePendingPayments 逐字相同的规矩：明确成功才落 succeeded，明确
// 失败才落 failed，其余（还是 PROCESSING、读不懂、连不上）**维持 processing**。把「我查不到」
// 当成「退失败了」是这条路上唯一不能犯的错——那会把一笔已经退成的钱在账上恢复成「没退」，
// 售后单落 failed，而用户手里已经拿到钱了。
//
// # 没有结论的那些要被推到队尾
//
// 与查单那条路不同，这里对「还是没结论」的每一笔都做一次 TouchRefund（把 updated_at 推到
// 此刻）。这不是记账，是**排队**：候选是按 updated_at 升序取的，而一笔托管中的退款可能要问
// 上几十轮才有结论。不推的话，一批 20 笔全卡在同几笔上，让后面那些本来能问出结果的退款
// 永远排不到。代价是那一笔的下一次询问晚一个周期——它本来就是「还不知道」，早问一轮没有用。
func (s *PaymentService) ReconcileProcessingRefunds(ctx context.Context, staleBefore time.Time, limit int) (RefundReconcileStats, error) {
	var stats RefundReconcileStats
	refunds, err := s.repository.ListStaleProcessingRefunds(ctx, staleBefore, limit)
	if err != nil {
		return stats, err
	}
	for i := range refunds {
		stats.Scanned++
		s.reconcileProcessingRefund(ctx, &refunds[i], &stats)
	}
	return stats, nil
}

// reconcileProcessingRefund 问一笔，并把它可能的结论落下去。
//
// 每笔各自计数、各自处理，任何一笔的任何失败都不打断这一轮（与 reconcilePendingPayment 同）。
func (s *PaymentService) reconcileProcessingRefund(ctx context.Context, refund *model.Refund, stats *RefundReconcileStats) {
	payment, err := s.repository.FindPaymentByNo(ctx, refund.PaymentNo)
	if err != nil {
		// 支付单读不出来是**数据异常**，不是「还没结论」：这张退款单指向的支付单不见了，
		// 没有任何东西能让它再往前走。计进 Skipped 并记 Error，让人来看。
		stats.Skipped++
		slog.ErrorContext(ctx, "cannot reconcile a processing refund: its payment is gone",
			"refund_no", refund.RefundNo, "payment_no", refund.PaymentNo, "error", err)
		return
	}

	// 渠道适配器按支付方式**重新解一次**，不存进退款单：理由与查单那条路逐字相同——用的必须
	// 是今天这份常量表，而且顺带把 MissingEnv 那条「这次部署没配齐」的判断走一遍。
	operator, route, method, secrets, err := s.refundOperator(payment)
	if err != nil {
		stats.Skipped++
		slog.WarnContext(ctx, "cannot reconcile a processing refund: no operator for its channel",
			"refund_no", refund.RefundNo, "payment_no", refund.PaymentNo, "error", err)
		return
	}

	// 出资行要重新读一次：这一笔停下之后什么都没变过（停在 processing 的退款单不改出资行），
	// 但它们决定了**将来结算时哪些行不走渠道**（见 MarkRefundSucceeded 的 NoOpLineTypes）。
	_, fundings, err := s.repository.FindRefundByNo(ctx, refund.RefundNo)
	if err != nil {
		stats.Skipped++
		slog.ErrorContext(ctx, "cannot reconcile a processing refund: its funding lines are unreadable",
			"refund_no", refund.RefundNo, "error", err)
		return
	}
	_, accountLineTypes := splitRefundFundings(fundings)

	request := provider.OperationRequest{
		Operation: provider.OperationQueryRefund,
		PaymentNo: payment.PaymentNo,
		OrderNo:   payment.OrderNo,
		RefundNo:  refund.RefundNo,
		// 查询不需要金额（渠道按退款单号认这一笔），但 request_id 要对得上：它进
		// payment_provider_calls，是「哪一次调用问出这个结论」的唯一依据。用退款单自己的
		// request_id，与发起那次同源。
		RequestID: refund.RequestID,
		Method:    method,
		Secrets:   secrets,
	}

	startedAt := s.now()
	result, callErr := operator.Execute(ctx, request)
	// 查询也落一条渠道调用流水，与发起那次同形（operation 区分）。这一步花的是出网时间，而
	// 「这一笔退款到底问过几次、每次渠道怎么答的」是事后唯一查得回来的东西。
	s.recordOperationCall(ctx, refund.RequestID, payment, route, model.CallOperationQueryRefund,
		operationFactsOf(request), result, callErr,
		int(s.now().Sub(startedAt).Milliseconds()))

	if callErr != nil {
		// 这次调用根本没发出去（或者出网就断了）：我们比问之前并没有更知道。推一下队尾，
		// 下一轮再问。
		stats.Inconclusive++
		s.refreshProcessingRefund(ctx, refund)
		slog.WarnContext(ctx, "refund query did not go out",
			"refund_no", refund.RefundNo, "provider", route.Provider(), "error", callErr)
		return
	}

	// 结论落库那一段与发起退款是**同一个收口**，只是 from 换成 processing：查询问出来的结论
	// 在状态流水上出发自哪一步是不一样的（见 refundTransitions）。
	//
	// ResultUnknown 也走这里，而它会落进 settleRefundOperation 的 default 分支——把退款单从
	// processing 再写一次 processing。所以这里要把「没有结论」这件事单独判一次，好让它去
	// 排队尾而不是被当成推进。
	if err := s.settleRefundOperation(ctx, payment, refund, accountLineTypes,
		model.RefundProcessing, result, nil); err != nil {
		stats.Skipped++
		slog.ErrorContext(ctx, "failed to settle a reconciled refund",
			"refund_no", refund.RefundNo, "result", string(result.Result), "error", err)
		return
	}

	switch result.Result {
	case provider.ResultSuccess:
		stats.Succeeded++
		slog.InfoContext(ctx, "settled a processing refund by querying the provider",
			"refund_no", refund.RefundNo, "payment_no", payment.PaymentNo,
			"provider", route.Provider(), "succeeded", true)
	case provider.ResultFailed:
		stats.Failed++
		slog.InfoContext(ctx, "settled a processing refund by querying the provider",
			"refund_no", refund.RefundNo, "payment_no", payment.PaymentNo,
			"provider", route.Provider(), "succeeded", false)
	default:
		// 还是 PROCESSING / UNKNOWN，或者一个我们不认识的词。什么都没推进，推一下队尾。
		stats.Inconclusive++
		s.refreshProcessingRefund(ctx, refund)
	}
}

// refreshProcessingRefund 把一笔还没有结论的退款单推到队尾（见 TouchRefund）。
//
// 失败只记日志，不往上抛：推不动队尾的后果是这一笔下一轮又被捞出来问一次，而那是**多做**
// 一次查询，不是做错一件事。让它把整轮扫断掉才是真的有害。
func (s *PaymentService) refreshProcessingRefund(ctx context.Context, refund *model.Refund) {
	if err := s.repository.TouchRefund(ctx, refund.ID); err != nil {
		slog.WarnContext(ctx, "failed to push an inconclusive refund to the back of the queue",
			"refund_no", refund.RefundNo, "error", err)
	}
}
