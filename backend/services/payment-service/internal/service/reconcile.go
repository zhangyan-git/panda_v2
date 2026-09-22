package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// ReconcileStats 是一轮主动查单的结果计数。
//
// 拆这么细是因为几种「什么都没推进」的含义完全不同：Inconclusive 是正常的（渠道说钱还没到，
// 下一轮再问），Skipped 是「这一笔我们压根没问成」——可能是部署缺了凭据，也可能是这一族的
// 渠道没有查单接口。并成一个数字的话，运维看到「一轮问了 20 笔、0 推进」时分不出是没到还是坏了。
type ReconcileStats struct {
	// Scanned 是这一轮从库里取出来问过的笔数。
	Scanned int
	// Succeeded / Failed 是这一轮真的改了支付单状态的笔数。
	Succeeded int
	Failed    int
	// Inconclusive 是「问了，渠道说还没结」或者「这次没问成」的笔数。它们留在 pending，
	// 下一轮还会被取出来再问。
	Inconclusive int
	// Skipped 是没问成的笔数：支付方式解不出来、渠道没有适配器、这一族没有查单接口、
	// 或者拿到结论之后落库失败。与 Inconclusive 分开，是因为它们指向的问题不一样。
	Skipped int
}

// ReconcilePendingPayments 向渠道逐笔问一遍「这些停在 pending 的单到底成了没有」，把有结论的
// 推到位。
//
// # 它补的是哪一段
//
// 一笔渠道支付的结果本来只从回调回来。而回调这条路并不保证到达：它会丢、会被网络挡在外面、
// 会因为这一侧的验签没配好而被我们主动拒收（银联商务那条要第二把密钥，部署漏配就一笔都收不
// 下来——见 catalog 里 commKey 与 ErrChannelIncomplete 的说明）。超时关单则只说「我们不再等
// 了」，从不说钱没收到。这条路上唯一有结论的动作是**去问渠道**，provider.Querier 就是为它
// 存在的。
//
// 老系统靠的正是这一条：每 60 秒扫一批卡在 confirmed/paying 超 5 分钟的咖啡订单去查单
// （panda_serve:miniapp/routes.go:354 → order_service.go:1988 RecoverStuckCoffeeOrder），
// 而它的回调入口连签名都不验（同文件 handlers/order_handler.go:1043）。那里能收三年钱靠的是
// 查单，不是回调——回调只是「早一点知道」。这一段是把它补回来。
//
// # 有结论才动，没结论什么都不做
//
// 与 settleByQuery 逐字相同的规矩：明确成功才推进 succeeded，明确失败（TRADE_CLOSED）才落
// failed，其余一律维持 pending。**把「我查不到」当成「没付钱」是这条路上唯一不能犯的错**——
// 那会把一笔已经收了的钱从账上抹掉。
//
// staleBefore 是「多久没动过才值得问一句」，由调用方给（见 worker 里的滞后常量）：要短到用户
// 还在收银页上等的时候就问到，又要长到不至于对一笔刚发起的支付做无谓的往返。
func (s *PaymentService) ReconcilePendingPayments(ctx context.Context, staleBefore time.Time, limit int) (ReconcileStats, error) {
	var stats ReconcileStats
	payments, err := s.repository.ListStalePendingPayments(ctx, staleBefore, limit)
	if err != nil {
		return stats, err
	}
	for i := range payments {
		stats.Scanned++
		s.reconcilePendingPayment(ctx, &payments[i], &stats)
	}
	return stats, nil
}

// reconcilePendingPayment 问一笔，并把它可能的结论落下去。
//
// 每笔各自计数、各自处理，任何一笔的任何失败都不打断这一轮：一批里有一笔渠道配错了，不该让
// 后面十九笔也跟着不被问到。
func (s *PaymentService) reconcilePendingPayment(ctx context.Context, payment *model.Payment, stats *ReconcileStats) {
	// 支付方式重新从常量表解一次，而不是存进支付单：支付方式与渠道是代码里的常量（见
	// internal/catalog），重解一次保证查单用的是**今天**这份配置，并且顺带把那几项部署没填全
	// 时该拒绝的判断（MissingEnv）也走一遍——配不全就不该带着空商户号去问渠道。
	route, err := s.resolveMethod(payment.PaymentMethod)
	if err != nil {
		stats.Skipped++
		slog.WarnContext(ctx, "cannot reconcile a pending payment: its payment method does not resolve",
			"payment_no", payment.PaymentNo, "payment_method", payment.PaymentMethod, "error", err)
		return
	}
	adapter, err := s.providers.Lookup(route.Provider())
	if err != nil {
		stats.Skipped++
		slog.WarnContext(ctx, "cannot reconcile a pending payment: no adapter for its provider",
			"payment_no", payment.PaymentNo, "provider", route.Provider(), "error", err)
		return
	}
	querier, ok := adapter.(provider.Querier)
	if !ok {
		// 这一族的适配器没有查单接口（见 provider.Querier：四家饭卡渠道里有几家只有回调）。
		// 这不是故障，是这一族的形状——那一笔继续留在 pending、由超时关单收走，与引入这条
		// 补偿任务之前的行为一致。只计数、不打日志：打了就是一分钟一条，而它每天都该是这样。
		stats.Skipped++
		return
	}

	method := route.providerMethod()
	secrets := resolveSecrets(s.resolveSecret, route.Channel, adapter, method)
	request := provider.QueryRequest{
		PaymentNo: payment.PaymentNo,
		OrderNo:   payment.OrderNo,
		RequestID: payment.RequestID,
		Method:    method,
		// 凭据按适配器声明的槽现场解一次，不存也不缓存。查单要签名，用的就是下单那几把；
		// 读不到时适配器自己会拒——那正是我们要的，绝不带着空密钥去问。
		Secrets: secrets,
	}

	startedAt := s.now()
	result, callErr := querier.Query(ctx, request)
	duration := int(s.now().Sub(startedAt).Milliseconds())
	// 查单也落一条渠道调用流水，与发起那条同形（operation 区分）。这一步花的是出网时间，而
	// 「这一笔到底问过几次、每次渠道怎么答的」是事后唯一查得回来的东西。
	s.recordProviderCall(ctx, audit.TraceIDFromContext(ctx), payment, route, model.CallOperationQuery,
		providerCallFacts{
			PaymentNo: payment.PaymentNo,
			OrderNo:   payment.OrderNo,
			Amount:    payment.Amount,
			RequestID: payment.RequestID,
			Method:    method,
		}, result, callErr, duration)
	if callErr != nil {
		// 这次调用根本没发出去（或者出网就断了）：我们比问之前并没有更知道，维持 pending。
		stats.Inconclusive++
		slog.WarnContext(ctx, "payment reconcile query did not go out",
			"payment_no", payment.PaymentNo, "provider", route.Provider(), "error", callErr)
		return
	}

	switch result.Result {
	case provider.ResultSuccess:
		// 渠道说这一笔成了。金额先对一遍再落：适配器把渠道回的 totalAmount 放进摘要
		// （见 ums 的 interpretQuery），它与我们记的应付对不上就说明这份应答说的不是这一笔
		// ——老系统同样处置（RecoverStuckCoffeeOrder:2021 对不上就转人工重试）。摘要里没有
		// 金额（渠道没回）时不因此拒绝，与回调那条路的判据一致：见 repository.SettlePayment
		// 里「两边都非空才比」那一段。
		if amount, reported := reconciledAmount(result); reported && amount != payment.Amount {
			stats.Inconclusive++
			slog.ErrorContext(ctx, "provider reported success for a different amount; refusing to settle",
				"payment_no", payment.PaymentNo,
				"expected_amount", payment.Amount, "reported_amount", amount)
			return
		}
		s.settleReconciledPayment(ctx, payment, route, true, result, stats)
	case provider.ResultFailed:
		// 明确失败（TRADE_CLOSED）：渠道说这笔没成。这一条可以信，与那些含糊状态不同。
		s.settleReconciledPayment(ctx, payment, route, false, result, stats)
	default:
		// 还是不明（WAIT_BUYER_PAY / NEW_ORDER / 读不懂的应答）。什么都不做，下一轮再问。
		stats.Inconclusive++
	}
}

// settleReconciledPayment 把一次查单得到的结论落成支付单的状态。
//
// 它复用回调那条路的 SettlePayment，只把 NotificationID 留空：这一条结论不是回调带来的，
// payment_notifications 里没有对应的行可标（仓储在 id 为空时跳过那一步）。复用而不是另写一段的
// 理由，是两条路在锁内判定、渠道一致性、金额校验、出资行、资金流水与 outbox 上**不可能漂移**
// ——查单收下的钱与回调收下的钱，在账上必须长得一模一样。
func (s *PaymentService) settleReconciledPayment(ctx context.Context, payment *model.Payment, route paymentRoute, succeeded bool, result provider.CreateResult, stats *ReconcileStats) {
	failureCode, failureMessage := "", ""
	if !succeeded {
		// 失败那一侧才带原因。成功时这两个字段会被仓储清空，传过去也只是噪音。
		failureCode = result.FailureCode
		failureMessage = result.FailureMessage
	}

	settlement, err := s.repository.SettlePayment(ctx, repository.SettleNotificationParams{
		// NotificationID 留空，见上面那段注释。
		PaymentNo: payment.PaymentNo,
		// Provider 取这一单按 payment_method 解出来的那条渠道，仓储会拿它与支付单上的
		// provider 对一遍。对不上就拒绝——那说明这一单当初不是这条渠道收的，查单的结论自然
		// 也不算数。
		Provider:              route.Provider(),
		Succeeded:             succeeded,
		Amount:                payment.Amount,
		ProviderTransactionID: result.ProviderTransactionID,
		FailureCode:           failureCode,
		FailureMessage:        failureMessage,
		TraceID:               audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		// 最常见的一种是 ErrPaymentAlreadySettled：这一笔在我们读出来之后被超时关单收走了，
		// 而渠道说钱收了。那是真事故（钱收了、单关了），按 Error 记——它是这一条路上唯一需要
		// 人来看的结局。其余（事务失败、行锁冲突）一样留痕，下一轮会再问一次。
		stats.Skipped++
		slog.ErrorContext(ctx, "failed to settle a reconciled payment",
			"payment_no", payment.PaymentNo, "succeeded", succeeded, "error", err)
		return
	}
	if settlement.AlreadySettled {
		// 状态早就到了：并发下另一个副本、或者恰好抵达的那条回调先落的手。不是错误，
		// 也不该计进「这一轮推进了几笔」。
		return
	}
	if succeeded {
		stats.Succeeded++
	} else {
		stats.Failed++
	}
	slog.InfoContext(ctx, "settled a pending payment by querying the provider",
		"payment_no", payment.PaymentNo, "provider", route.Provider(), "succeeded", succeeded)
}

// reconciledAmount 从查单结果的摘要里取出渠道报的金额。
//
// 金额不像状态那样有专门的一栏——provider.CreateResult 描述的是一次**调用**，不是一笔**支付**
// （它的注释写着这一点），所以适配器把金额放进 ResponseSummary。第二个返回值为 false 表示渠道
// 没报金额，**那不是「金额是 0」**：调用方不能拿它去比，只能按「比不了」处置。
func reconciledAmount(result provider.CreateResult) (int64, bool) {
	amount, ok := result.ResponseSummary["totalAmount"].(int64)
	return amount, ok && amount > 0
}
