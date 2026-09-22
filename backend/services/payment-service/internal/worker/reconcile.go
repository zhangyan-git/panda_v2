package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// DefaultReconcileInterval 是主动查单的默认周期。
//
// 一分钟照抄老系统（panda_serve:miniapp/routes.go:357 的那个 ticker）：用户还在收银页上等着
// 的时候，钱已经到了渠道而我们这边什么都没发生的窗口，最长就是这一分钟。
const DefaultReconcileInterval = time.Minute

// DefaultReconcileBatch 是单次扫描最多问渠道几笔。
//
// 20 也是老系统的取值（routes.go:362）。上限的意义与关单扫描不同：这里的每一笔都要**出网**，
// 一次要花几百毫秒到几秒，所以一批 20 笔最坏就是这一轮拖上十几秒——足够把积压一点点消化掉，
// 又不至于让一个 goroutine 长期占着。
const DefaultReconcileBatch = 20

// DefaultReconcileStaleAfter 是「一笔发起之后多久没结论，才值得去问渠道一句」。
//
// 5 分钟同样是老系统的取值（routes.go:361）。这个值必须短于支付单的存活时长：它是
// service.DefaultPaymentTTL（15 分钟），于是这一笔在超时关单把它收走之前还能被问到大约十次
// ——十次里面问到一次就够了，而关单之后才发现的「钱收了、单关了」是要人来收拾的。
const DefaultReconcileStaleAfter = 5 * time.Minute

// ReconcileWorker 定期向渠道问一遍那些发起之后一直没结论的支付单。
//
// 它补的是回调不到达的那一段，理由见 service.ReconcilePendingPayments。与关单任务合起来看：
// 关单管「我们不再等了」，这个管「钱到底到没到」——两件事分开，是因为关掉一张单从来不能说明
// 那笔钱没被收走。
type ReconcileWorker struct {
	payments   *service.PaymentService
	interval   time.Duration
	batch      int
	staleAfter time.Duration
}

// NewReconcileWorker 构造查单任务。三个参数非正时各自用默认值。
func NewReconcileWorker(payments *service.PaymentService, interval time.Duration, batch int, staleAfter time.Duration) *ReconcileWorker {
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	if batch <= 0 {
		batch = DefaultReconcileBatch
	}
	if staleAfter <= 0 {
		staleAfter = DefaultReconcileStaleAfter
	}
	return &ReconcileWorker{payments: payments, interval: interval, batch: batch, staleAfter: staleAfter}
}

// Run 先扫一次再按周期扫。先扫一次的理由同关单：服务停的那段时间里攒下的 pending 支付单，
// 等一个完整周期才去问会让它们多挂一分钟——而这一分钟里钱可能早就到了渠道。
func (w *ReconcileWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 返回 ctx.Err()：这是「被要求停」，不是「跑完了」（同 ExpiryWorker）。
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

// sweep 问一轮。
//
// 与关单那个任务不同，这一轮**不排空**：一批 20 笔要出网 20 次，把积压一次做光会让这个
// goroutine 长时间不回 select，停机时要等它。剩下的留给下一分钟——一笔支付单在超时关单收走
// 它之前有大约十轮机会。
func (w *ReconcileWorker) sweep(ctx context.Context) {
	stats, err := w.payments.ReconcilePendingPayments(ctx, time.Now().Add(-w.staleAfter), w.batch)
	if err != nil {
		// 读不出候选（库抖动）只是一轮没问到，下一分钟再来。
		slog.Error("payment reconcile sweep", "error", err)
		return
	}

	if stats.Succeeded+stats.Failed > 0 {
		// 这一轮真的把某几笔的状态问出来了——这是这条任务存在的全部理由，值得一行日志。
		slog.Info("reconciled pending payments against the provider",
			"scanned", stats.Scanned, "succeeded", stats.Succeeded, "failed", stats.Failed)
	}
	if stats.Skipped > 0 {
		// 有笔数压根没问成：支付方式解不出来、渠道没适配器、这一族没有查单接口、或者拿到
		// 结论之后落库失败。原因逐笔记在 service 里，这一行只保证「有东西没问到」不会因为
		// 没有状态变化而完全看不见。
		slog.Warn("payment reconcile sweep scanned payments it could not query",
			"scanned", stats.Scanned, "skipped", stats.Skipped, "inconclusive", stats.Inconclusive)
	}
}
