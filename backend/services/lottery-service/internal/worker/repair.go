package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/service"
)

// DefaultRepairInterval 是参与修复的默认周期。
//
// 与开奖那条不同，这一条**没有人在等**：卡住的参与是「用户的卡可能已经扣了，但参与没进
// 期次」，用户此刻看到的是「参与处理中」。慢一分钟没有让任何人看到一个错误的界面，而
// 它调的是 account-service——跑得太密只会让账户域在它已经不可用的时候再被多打几轮。
//
// 它取 RepairAfter（60 秒）本身：扫描的条件是「pending 且老于一分钟」，每 60 秒扫一次
// 意味着一条卡住的记录最多在两分钟内被重跑。这个下界不能凭感觉调小——它必须**大于等于**
// RepairAfter，否则每一轮都会扫到刚刚才落下的记录，把一段毫秒级的三段事务当成「卡住了」。
const DefaultRepairInterval = service.DefaultRepairAfter

// maxRepairRoundsPerTick 是一次 tick 里最多连续扫几批。
//
// 同开奖 worker：上限是为了让积压不会把停机拖住。正常情况下这个循环第一轮就返回
// ——待修复的记录应该是零。
const maxRepairRoundsPerTick = 5

// RepairWorker 重跑卡在 pending 的参与记录。
//
// # 它修的是什么
//
// 参与是「落本地记录 → 扣卡 → 确认」三段，跨服务调用夹在中间。进程在第二段中途死掉、
// 或者扣卡调用超时，都会留下一条不动的 pending，而**用户的卡可能已经被扣了**。没有这个
// worker，那张卡就悬在那里：余额少一张，抽奖中心里却查不到任何一笔参与。
//
// # 为什么它安全
//
// 重跑走的是 service.settle，与第一次尝试逐字相同的那段代码；扣减请求里的 request_id
// 就是参与记录的 id，账户域拿它派生幂等键。所以无论重跑多少次，卡最多扣一张、计数最多加
// 一次。
type RepairWorker struct {
	lottery  *service.LotteryService
	interval time.Duration
	batch    int
}

// NewRepairWorker 构造修复任务。interval 或 batch 非正时用默认值。
func NewRepairWorker(lottery *service.LotteryService, interval time.Duration, batch int) *RepairWorker {
	if interval <= 0 {
		interval = DefaultRepairInterval
	}
	if batch <= 0 {
		batch = service.DefaultSweepBatch
	}
	return &RepairWorker{lottery: lottery, interval: interval, batch: batch}
}

// Run 先扫一次再按周期扫。
func (w *RepairWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *RepairWorker) sweep(ctx context.Context) {
	for round := 0; round < maxRepairRoundsPerTick; round++ {
		result, err := w.lottery.SweepParticipations(ctx, w.batch)
		if err != nil {
			// 读不出待修复的清单（数据库抖动）。记下来继续听：退出会让这个副本从此不再
			// 修复，而卡住的记录没有别人会来收。
			slog.ErrorContext(ctx, "lottery participation repair sweep failed", "error", err)
			return
		}
		if result.Scanned == 0 {
			return
		}
		if result.StillPending > 0 {
			// 有记录这一轮还是没得出结论，说明账户域仍然不可达。**这是唯一需要人看见的
			// 一种「没做成」**，所以它按 warn 记，并且带上还剩几条。
			slog.WarnContext(ctx, "some pending lottery participations are still unresolved",
				"scanned", result.Scanned, "settled", result.Settled, "stillPending", result.StillPending)
		} else {
			slog.InfoContext(ctx, "lottery participation repair sweep finished",
				"scanned", result.Scanned, "settled", result.Settled)
		}

		// 没扫满一批说明积压已经清完了。
		if result.Scanned < w.batch {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
	slog.WarnContext(ctx, "lottery participation repair hit its per-tick limit; backlog is building up",
		"batch", w.batch, "rounds", maxRepairRoundsPerTick)
}
