// Package worker 是订单服务的后台补偿任务。
//
// 它们都是 runtime.Runner：随服务启停，只依赖传进来的 ctx。超时关单必须是补偿任务而
// 不是定时精确到点——订单到期的那一刻没有人在等它，晚几十秒关掉不会有人受损，而
// 「到点必达」要的是一套调度基础设施，不是这个阶段该引入的东西。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// DefaultSweepInterval 是关单扫描的默认周期。
//
// 一分钟是「用户感知」与「扫描开销」的折中：订单到期时间精确到秒，界面上的倒计时到 0
// 之后最多再等一分钟订单才会变成已关闭；而每分钟一次、每次最多一批的扫描，在一张只按
// 待支付状态建过部分索引的表上是几毫秒的事。
const DefaultSweepInterval = time.Minute

// DefaultSweepBatch 是单次扫描最多关掉的订单数。
//
// 有上限是为了让「积压了十万单」时每次只做一点、把数据库留给正常请求，而不是让补偿
// 任务自己去撞一次长时间的行锁持有。积压靠连续扫描消化（见 Run 里的排空循环）。
const DefaultSweepBatch = 200

// maxSweepsPerTick 是一次 tick 里最多连续扫几批。
//
// 必须有这个上限：没有它，一个永远关不完的积压（比如某张表的 expires_at 有大量历史值）
// 会让这个 goroutine 一直不回到 select，ctx 取消也就迟迟看不到——停机时最需要它停的东西
// 反而停得最慢。剩下的积压留给下一分钟。
const maxSweepsPerTick = 10

// ExpiryWorker 把到点未支付的订单关掉。
type ExpiryWorker struct {
	orders   *service.OrderService
	interval time.Duration
	batch    int
}

// NewExpiryWorker 构造关单任务。interval 或 batch 非正时用默认值。
func NewExpiryWorker(orders *service.OrderService, interval time.Duration, batch int) *ExpiryWorker {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	if batch <= 0 {
		batch = DefaultSweepBatch
	}
	return &ExpiryWorker{orders: orders, interval: interval, batch: batch}
}

// Run 先扫一次再按周期扫。先扫一次的理由是重启：服务停的这段时间里到期、退避、或
// 补投的事件都堆着，等一个完整的周期才处理会让用户在最可能察觉到的时候多等一分钟。
func (w *ExpiryWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 返回 ctx.Err()：这是「被要求停」，不是「跑完了」。runtime 靠这个区分
			// 正常退出与出错退出，别把它吞成 nil。
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *ExpiryWorker) sweep(ctx context.Context) {
	for round := 0; round < maxSweepsPerTick; round++ {
		// traceID 传空串：这是一次没有来路的后台扫描，编一个 id 只会让链路里多一条
		// 对不上的线索。状态流水里的 actor_type 记的是 system，那才是它的身份。
		closed, err := w.orders.ExpireOverdue(ctx, w.batch, "")
		if err != nil {
			// 记下来继续听，不让循环退出：退出会让这个副本从此不再关单，而关单没人做
			// 的后果是订单永远停在待支付、把库存和券一直占着。
			slog.Error("order expiry sweep", "error", err)
			return
		}
		if closed < w.batch {
			// 没扫满一批说明已经关完了，不必再空跑一轮。
			return
		}
		if ctx.Err() != nil {
			// 每一批之间看一眼 ctx：积压深的时候（比如刚跑过一次全量导入）别让这一轮
			// 拖住停机。
			return
		}
	}
	// 到这里的含义是「连续扫满了 maxSweepsPerTick 批还没关完」：积压比预期深。
	// 留一条日志，否则这种情况从外部看只是「订单关得比平常慢」，没人知道该查什么。
	slog.Warn("order expiry sweep hit its per-tick limit; backlog may be building up",
		"batch", w.batch, "rounds", maxSweepsPerTick)
}
