// Package worker 是支付服务的后台补偿任务。
//
// 它随服务启停，只依赖传进来的 ctx。超时关单是补偿任务而不是定时精确到点：支付单到期的
// 那一刻没有人在等它，晚几十秒关掉不会有人受损，而「到点必达」要的是一套调度基础设施，
// 不是这个阶段该引入的东西。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
)

// DefaultSweepInterval 是关单扫描的默认周期。
//
// 一分钟是「用户感知」与「扫描开销」的折中：支付单的 expires_at 精确到秒，用户在那个
// 收银页面上等着的时候最多再等一分钟它才会变成已过期；而每分钟一次、每次最多一批的扫描，
// 在一张只按待支付状态建过索引的表（payments_pending_expiry_idx）上是几毫秒的事。
const DefaultSweepInterval = time.Minute

// DefaultSweepBatch 是单次扫描最多关掉的支付单数。
//
// 有上限是为了让「积压了十万单」时每次只做一点、把数据库留给正常请求，而不是让补偿任务
// 自己去撞一次长时间的行锁持有。积压靠连续扫描消化（见 sweep 里的排空循环）。
const DefaultSweepBatch = 200

// maxSweepsPerTick 是一次 tick 里最多连续扫几批。
//
// 必须有这个上限：没有它，一个永远关不完的积压（比如某张表的 expires_at 有大量历史值）
// 会让这个 goroutine 一直不回到 select，ctx 取消也就迟迟看不到——停机时最需要它停的东西
// 反而停得最慢。剩下的积压留给下一分钟。
const maxSweepsPerTick = 10

// ExpiryWorker 把到点未支付的支付单关掉。
type ExpiryWorker struct {
	payments *service.PaymentService
	interval time.Duration
	batch    int
}

// NewExpiryWorker 构造关单任务。interval 或 batch 非正时用默认值。
func NewExpiryWorker(payments *service.PaymentService, interval time.Duration, batch int) *ExpiryWorker {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	if batch <= 0 {
		batch = DefaultSweepBatch
	}
	return &ExpiryWorker{payments: payments, interval: interval, batch: batch}
}

// Run 先扫一次再按周期扫。先扫一次的理由是重启：服务停的这段时间里到期的支付单都堆着，
// 等一个完整的周期才处理会让它们多挂一分钟——而每多挂一分钟，用户就多一次「在一个已经
// 该作废的收银页上付款」的机会。
func (w *ExpiryWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 返回 ctx.Err()：这是「被要求停」，不是「跑完了」。runtime 靠这个区分正常退出
			// 与出错退出，别把它吞成 nil。
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *ExpiryWorker) sweep(ctx context.Context) {
	// 先把「豆已经扣了、单还没结」的结算掉，再关到点的。顺序不能反：关单那条查询排除了
	// 这些行，所以两批互不相交，但如果先关、下一轮才结算，中间那段时间里用户看到的是一张
	// 「已过期」而钱已经扣了的单（见 service.SettleOverdueAccountPayments）。
	//
	// 只跑一轮，不排空：这一批处理完之前它们也不会过期得更厉害，而积压靠下一分钟继续。
	// 出错只记日志、不 return——关单是这一轮的主任务，不该被它挡住。
	if settled, err := w.payments.SettleOverdueAccountPayments(ctx, w.batch); err != nil {
		slog.Error("settle overdue account-funded payments", "error", err)
	} else if settled > 0 {
		slog.Info("settled overdue account-funded payments", "count", settled)
	}

	for round := 0; round < maxSweepsPerTick; round++ {
		closed, err := w.payments.ExpireOverduePayments(ctx, w.batch)
		if err != nil {
			// 记下来继续听，不让循环退出：退出会让这个副本从此不再关单，而关单没人做的
			// 后果是支付单永远停在待支付，渠道那边那张预支付单还有效——用户能在一个我们
			// 以为已经作废的单上把钱付了。
			slog.Error("payment expiry sweep", "error", err)
			return
		}
		if closed < w.batch {
			// 没扫满一批说明已经关完了，不必再空跑一轮。
			return
		}
		if ctx.Err() != nil {
			// 每一批之间看一眼 ctx：积压深的时候别让这一轮拖住停机。
			return
		}
	}
	// 到这里的含义是「连续扫满了 maxSweepsPerTick 批还没关完」：积压比预期深。留一条日志，
	// 否则这种情况从外部看只是「支付单关得比平常慢」，没人知道该查什么。
	slog.Warn("payment expiry sweep hit its per-tick limit; backlog may be building up",
		"batch", w.batch, "rounds", maxSweepsPerTick)
}
