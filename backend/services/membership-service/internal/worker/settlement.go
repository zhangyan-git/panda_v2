package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
)

// DefaultSettlementInterval 是待办重试的默认周期。
//
// 它比另外两个扫描**勤一个量级**（那两个是十分钟），因为它等的是**另一个域恢复**：订单域重启、
// 发布、抖一下，通常都是分钟级的事，而这几分钟里每一笔积压都是一笔已经收了钱、账上却查不到的
// 扣款。一分钟一轮让积压在这一分钟里就开始消化。
//
// 跑得勤的代价可以忽略：待办表正常是空的，这一轮就是那条索引上的一次查找（与另外两个扫描不同，
// 它们每一轮都要扫全表的一个切片）。
const DefaultSettlementInterval = time.Minute

// SettlementWorker 把「钱收了、账没落成」的待办重试到落成为止。
//
// 它服务的是**扣款成功那条链的第一步**（在订单域建一张续费单）：第一步失败时事件消费会把那一期
// 落成一条待办并 ack（见 service.parkChargeSettlement），这个 worker 就是那个「稍后再试」。
//
// **它不会放弃**：试满几次就删行等于把这笔钱从账上抹掉。所以这个 worker 的失败长这样——一段
// ERROR 日志按退避的节奏一直响，直到订单域恢复（自动落成）或者有人把根因修好。这与到期扫描那条
// 「记下来继续听」是同一条规矩，只是这里的后果更重：那一条漏了只是状态晚几分钟翻，这一条漏了是
// 一条对不上账的钱。
type SettlementWorker struct {
	memberships *service.MembershipService
	interval    time.Duration
	batch       int
}

// NewSettlementWorker 构造待办重试任务。interval 或 batch 非正时用默认值。
func NewSettlementWorker(memberships *service.MembershipService, interval time.Duration, batch int) *SettlementWorker {
	if interval <= 0 {
		interval = DefaultSettlementInterval
	}
	if batch <= 0 {
		batch = repository.DefaultSettlementBatch
	}
	return &SettlementWorker{memberships: memberships, interval: interval, batch: batch}
}

// Run 先扫一次再按周期扫。先扫一次的理由与另外两个任务逐字相同（见 expiry.go）：重启前那一段
// 积压着，等一个完整周期才处理，等于让订单域的一次重启在服务重启之后再拖一分钟。
func (w *SettlementWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 返回 ctx.Err()：这是「被要求停」，不是「跑完了」。
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *SettlementWorker) sweep(ctx context.Context) {
	for round := 0; round < maxSweepsPerTick; round++ {
		// traceID 传空串：这是一次没有来路的后台重试，编一个 id 只会让链路里多一条对不上的线索。
		// 每一条待办自己的 trace_id 是**事件到达时那个**（存在行上），那才是能把这一笔追回原事件
		// 的线索，由 service 带进流水。
		settled, err := w.memberships.SettlePendingCharges(ctx, w.batch)
		if err != nil {
			// 记下来继续听，不让循环退出：退出会让这个副本从此不再重试，而没人重试的后果是一批
			// 「收了钱没落账」的行静静躺在表里——没有别的任何地方会提它们。
			slog.Error("membership settlement sweep", "error", err)
			return
		}
		if settled < w.batch {
			// 没落满一批说明这一批已经处理完了（剩下的要么没有，要么是失败的——失败的那些已经被
			// 推后了，不会再被这一轮捞到）。
			return
		}
		if ctx.Err() != nil {
			// 每一批之间看一眼 ctx：积压深的时候别让这一轮拖住停机。
			return
		}
	}
	// 到这里的含义是「连续落满了 maxSweepsPerTick 批还没落完」：订单域刚恢复时的积压比预期深。
	// 留一条日志，否则这种情况从外部看只是「补得有点慢」，没人知道该查什么。
	slog.Warn("membership settlement sweep hit its per-tick limit; backlog may be building up",
		"batch", w.batch, "rounds", maxSweepsPerTick)
}
