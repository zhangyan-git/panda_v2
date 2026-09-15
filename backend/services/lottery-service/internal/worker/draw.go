// Package worker 是抽奖服务的两个后台任务：开奖与参与修复。
//
// 两个都是**补偿任务**而不是「精确定时」的任务：它们随服务启停、只依赖传进来的 ctx，按周期
// 扫一批该做的事。抽奖这边比支付那边更宽松一点——期次到点之后没有人卡在一个转不完的圈上等
// 它（用户在看抽奖中心，界面显示的是「已结束，开奖中」），所以晚十几秒开奖是可以接受的，
// 而「到点必达」要的是一套调度基础设施，不是这个阶段该引入的东西。
//
// 两个 worker 都不选主，多副本同时跑是**设计如此**：真正的判定在数据库的行锁里重做一遍
// （见 repository.DrawRound / Confirm），扫描只是「谁该被看一眼」的清单。多一个副本只是多
// 几次白跑，不会多开一次奖、也不会多扣一张卡。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/service"
)

// DefaultDrawInterval 是开奖扫描的默认周期。
//
// 十五秒而不是像支付关单那样一分钟：这一条的等待是**用户看得见的**。期次达标的那一瞬间
// 界面就从「还差 0 人」变成「开奖中」，用户大概率正盯着那一页等结果；一分钟才开一次奖会
// 让人觉得「是不是卡住了」。而每十五秒一次、每次一批的扫描，在一张只按状态与 ends_at 建过
// 索引的表（lottery_rounds_sweep_idx）上代价可以忽略。
const DefaultDrawInterval = 15 * time.Second

// DefaultDrawBatch 是单次扫描最多处理几期。
//
// 上限存在的理由是让「积压了」这种情况每次只做一点，把数据库留给正常请求。正常情况下
// 这个数是 0——该开奖的期次在系统里是「刚达标的那一期」，不会是几十条。
const DefaultDrawBatch = 50

// maxDrawsPerTick 是一次 tick 里最多连续扫几批。
//
// 与支付那个 worker 同样的理由：没有上限的话，一个永远开不完的积压会让这个 goroutine
// 一直不回到 select，ctx 取消也就迟迟看不到——停机时最需要它停的东西反而停得最慢。
const maxDrawsPerTick = 5

// DrawWorker 把到点或已达标的期次开掉。
type DrawWorker struct {
	lottery  *service.LotteryService
	interval time.Duration
	batch    int
}

// NewDrawWorker 构造开奖任务。interval 或 batch 非正时用默认值。
func NewDrawWorker(lottery *service.LotteryService, interval time.Duration, batch int) *DrawWorker {
	if interval <= 0 {
		interval = DefaultDrawInterval
	}
	if batch <= 0 {
		batch = DefaultDrawBatch
	}
	return &DrawWorker{lottery: lottery, interval: interval, batch: batch}
}

// Run 先扫一次再按周期扫。
//
// 先扫一次的理由是重启：服务停的这段时间里到点或达标的期次都堆着，等一个完整周期才处理
// 会让它们多挂十五秒；而在这十五秒里，那一期的用户看到的是「正在进行」——界面在骗人，
// 那一期其实已经不再收人了（达标的那一期在确认时就被置成 closed 了）。
func (w *DrawWorker) Run(ctx context.Context) error {
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

func (w *DrawWorker) sweep(ctx context.Context) {
	for round := 0; round < maxDrawsPerTick; round++ {
		ids, err := w.lottery.RoundsAwaitingDraw(ctx, w.batch)
		if err != nil {
			// 记下来继续听，不让循环退出：退出会让这个副本从此不再开奖，而开奖没人做的
			// 后果是期次永远停在 closed——参与的用户以为自己在等一个结果，其实没有人在算。
			slog.ErrorContext(ctx, "lottery draw sweep could not scan for rounds", "error", err)
			return
		}

		// 一轮里把这一批逐个开掉。**开奖之间不设间隔**：每一期各自独立，锁也是按期次行的。
		for _, id := range ids {
			w.drawOne(ctx, id)
			if ctx.Err() != nil {
				// 每一期之间看一眼 ctx：积压深的时候别让这一轮拖住停机。
				return
			}
		}

		// 没扫满一批说明当前没有该开的了，不必再空跑一轮。
		if len(ids) < w.batch {
			return
		}
	}
	slog.WarnContext(ctx, "lottery draw sweep hit its per-tick limit; backlog may be building up",
		"batch", w.batch, "rounds", maxDrawsPerTick)
}

// drawOne 开一期，并把结果记成日志。
//
// 四种「没做成」要分开记，因为它们对运维的含义完全不同：
//
//   - (nil, nil)：本期被别人抢先开了、被人作废了、或者已经不在开奖窗口里。多副本下这是
//     **正常噪声**，一条都不能告警——否则每多一个副本，告警就多一份。
//   - Draw 为 nil 的 outcome：零人参与，这一期直接作废并开了下一期。这不是失败，是
//     「没人来」。它也**不该**被当成一次告警，但值得留一条 info：一个长期零人参与的活动
//     是运营要看见的事。
//   - 开奖成功：info，带上种子与人数。种子落在这里，事后要复盘这次开奖时它是线索。
//   - 出错：error。这是真的需要人看的，尤其是带 ErrRoundChanged 的那一类——它意味着
//     期次在扫描与开奖之间变了样子，而那种事只应该由并发开奖引起（上面第一种）。
func (w *DrawWorker) drawOne(ctx context.Context, roundID string) {
	outcome, err := w.lottery.DrawAutomatically(ctx, roundID)
	if err != nil {
		slog.ErrorContext(ctx, "drawing a lottery round failed", "roundId", roundID, "error", err)
		return
	}
	if outcome == nil {
		// 被抢先了 / 已经不在窗口里。见上面第一段，这里连日志都不记：多副本下它会刷屏。
		return
	}
	if outcome.Draw == nil {
		slog.InfoContext(ctx, "a lottery round had no participants and was cancelled",
			"roundId", roundID, "nextRoundId", nextRoundID(outcome.NextRound))
		return
	}
	slog.InfoContext(ctx, "lottery round drawn automatically",
		"roundId", roundID,
		"roundNo", roundNo(outcome),
		"drawId", outcome.Draw.ID,
		"trigger", outcome.Draw.Trigger,
		"participants", outcome.Draw.ParticipantCount,
		"winners", outcome.Draw.WinnerCount,
		"seed", outcome.Draw.Seed,
		"campaignEnded", outcome.CampaignEnded)
}

// nextRoundID / roundNo 是给日志用的两个小取值器。
//
// 写成函数而不是在日志行里内联一个个 nil 判断：开奖结果里这两个指针**正常就是空的**
// （活动窗口结束、或者零人参与那一条路），日志那一行不该被三个问号塞满。
func nextRoundID(round *model.Round) string {
	if round == nil {
		return ""
	}
	return round.ID
}

func roundNo(outcome *repository.DrawOutcome) string {
	if outcome.Round == nil {
		return ""
	}
	return outcome.Round.RoundNo
}
