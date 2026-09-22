package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// DefaultRefundQueryInterval 是退款查询的默认周期。
//
// 比查单那条路（一分钟）长得多，因为**这两条路在等的东西完全不是一回事**：查单等的是用户
// 还在收银页上按的那一次支付，晚一分钟就意味着用户多盯着转圈一分钟；退款查询等的是渠道把
// 钱退回去，那件事在银联商务那边可以是几分钟，也可以是几天（托管退款要等商户结算）。一分钟
// 问一次只是白白出网。
//
// 五分钟仍然是个不长的周期：一笔正常几秒就退成的退款，最坏五分钟之内售后单就会推进——那个
// 延迟用户看不见（他手上是「退款处理中」），而它把出网次数压到了查单那一路的五分之一。
const DefaultRefundQueryInterval = 5 * time.Minute

// DefaultRefundQueryBatch 是单次扫描最多问渠道几笔。
//
// 与查单同一取值、同一理由：每一笔都要出网，一批 20 笔最坏就是这一轮拖上十几秒。这个数在
// 退款这条路上比查单宽松得多——退款的积压天然比支付小几个量级（只有审过、且渠道没当场给
// 结论的那些才会进这个队列）。
const DefaultRefundQueryBatch = 20

// DefaultRefundQueryStaleAfter 是「一笔退款发起之后多久没动静，才值得去问渠道一句」。
//
// 它比查单那一路（5 分钟）短，而且短得不多是**故意的**：一笔退了 5 分钟还没结论的退款，
// 已经不太可能在下一秒自己跳成 succeeded 了（那种情况在发起那一次应答里就给了 SUCCESS）。
// 而一笔托管退款要问上几天，所以这个值再调大只会推迟它被问到的第一次，不会有别的好处。
//
// 它的下限由**退款查询的周期**决定：这个值短于周期时，每一笔刚被标成 processing 的退款在
// 下一轮就会被问到一次，那是我们想要的第一问。取 5 分钟与周期相同，于是每一笔最多等一个
// 周期就会被问到。
const DefaultRefundQueryStaleAfter = 5 * time.Minute

// RefundQueryWorker 定期向渠道问一遍那些发起之后一直没有结论的退款单。
//
// 它是这条链上**唯一**能把 processing 推向终态的东西：规范里没有退款回调，渠道不会主动来
// 告诉我们结果（见 service.ReconcileProcessingRefunds 里那一段）。没有它，一笔应答是
// PROCESSING 的退款会永远停在 processing、售后单永远停在 refunding。
//
// 形状与 ReconcileWorker 逐字同源：先扫一次再按周期扫，一轮不排空，一批出网几次。
type RefundQueryWorker struct {
	payments   *service.PaymentService
	interval   time.Duration
	batch      int
	staleAfter time.Duration
}

// NewRefundQueryWorker 构造退款查询任务。三个参数非正时各自用默认值。
func NewRefundQueryWorker(payments *service.PaymentService, interval time.Duration, batch int, staleAfter time.Duration) *RefundQueryWorker {
	if interval <= 0 {
		interval = DefaultRefundQueryInterval
	}
	if batch <= 0 {
		batch = DefaultRefundQueryBatch
	}
	if staleAfter <= 0 {
		staleAfter = DefaultRefundQueryStaleAfter
	}
	return &RefundQueryWorker{payments: payments, interval: interval, batch: batch, staleAfter: staleAfter}
}

// Run 先扫一次再按周期扫。
//
// 先扫一次的理由在这里比查单那条路更硬：退款是**用户已经被告知「在处理中」**的事，服务重启
// 之后让那些单再等一个完整周期（五分钟）才开始被问，是白等。
func (w *RefundQueryWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 返回 ctx.Err()：这是「被要求停」，不是「跑完了」（同 ExpiryWorker / ReconcileWorker）。
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep 问一轮。
//
// 与关单那个任务不同，这一轮**不排空**（同 ReconcileWorker）：一批 20 笔要出网 20 次，把积压
// 一次做光会让这个 goroutine 长时间不回 select，停机时要等它。剩下的留给下一轮。
func (w *RefundQueryWorker) sweep(ctx context.Context) {
	stats, err := w.payments.ReconcileProcessingRefunds(ctx, time.Now().Add(-w.staleAfter), w.batch)
	if err != nil {
		// 读不出候选（库抖动）只是一轮没问到，下一次再来。
		slog.Error("refund query sweep", "error", err)
		return
	}

	if stats.Succeeded+stats.Failed > 0 {
		// 这一轮真的把某几笔退款问出了结论——这是这条任务存在的全部理由，值得一行日志。
		slog.Info("reconciled processing refunds against the provider",
			"scanned", stats.Scanned, "succeeded", stats.Succeeded, "failed", stats.Failed)
	}
	if stats.Skipped > 0 {
		// 有笔数压根没问成：支付方式解不出来、渠道没适配器、凭据没配、或者拿到结论之后落库
		// 失败。原因逐笔记在 service 里，这一行只保证「有东西没问到」不会因为状态没变化而
		// 完全看不见。
		slog.Warn("refund query sweep scanned refunds it could not query",
			"scanned", stats.Scanned, "skipped", stats.Skipped,
			"inconclusive", stats.Inconclusive)
	}
}
