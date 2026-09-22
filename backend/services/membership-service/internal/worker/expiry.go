// Package worker 是会员服务的后台补偿任务。
//
// 它们都是 runtime.Runner：随服务启停，只依赖传进来的 ctx。两个任务都做成补偿扫描而不是
// 定时精确到点——到期的那一刻没有人在等它：把 status 从 active 改成 expired 晚几十秒，
// 不会让任何人少享一秒权益（判定读的是 expire_at，不是 status，见 model.Membership.IsUsable）；
// 扣款晚几分钟，用户的会员也还在。而「到点必达」要的是一套调度基础设施，不是这个阶段该引入
// 的东西。
//
// 两个任务的分工：expiry.go 管**会员到期**（改本库状态），renewal.go 管**代扣到期**（出网调
// 渠道，一个字节都不改本库）。后者多一层要防的东西，见 renewal.go 里那段「它只发起」。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
)

// DefaultSweepInterval 是到期扫描的默认周期。
//
// 十分钟是「用户感知」与「扫描开销」的折中，比订单域的关单扫描（一分钟）长一个量级：
// 订单的到期时间在界面上是个倒计时，用户盯着它归零；而没有任何界面会因为 status 还是
// active 而显示错——会员中心与下单定价读的都是 expire_at。status 落后一步的后果是
// 运营在后台筛「已过期」时晚几分钟看到这个人，那不是急事。
const DefaultSweepInterval = 10 * time.Minute

// DefaultSweepBatch 是单次扫描最多处理的会员数。
//
// 有上限是为了让「积压了十万条」时每次只做一点、把数据库留给正常请求，而不是让补偿任务
// 自己去撞一次长时间的行锁持有。积压靠连续扫描消化（见 Run 里的排空循环）。
const DefaultSweepBatch = 200

// maxSweepsPerTick 是一次 tick 里最多连续扫几批。
//
// 必须有这个上限：没有它，一个永远扫不完的积压（比如某次批量导入灌进了一批早就过期的行）
// 会让这个 goroutine 一直不回到 select，ctx 取消也就迟迟看不到——停机时最需要它停的东西
// 反而停得最慢。剩下的积压留给下一轮。
const maxSweepsPerTick = 10

// ExpiryWorker 把到点未续费的会员改成已过期。
type ExpiryWorker struct {
	memberships *service.MembershipService
	interval    time.Duration
	batch       int
}

// NewExpiryWorker 构造到期扫描任务。interval 或 batch 非正时用默认值。
func NewExpiryWorker(memberships *service.MembershipService, interval time.Duration, batch int) *ExpiryWorker {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	if batch <= 0 {
		batch = DefaultSweepBatch
	}
	return &ExpiryWorker{memberships: memberships, interval: interval, batch: batch}
}

// Run 先扫一次再按周期扫。先扫一次的理由是重启：服务停的这段时间里到期的会员堆着，等一个
// 完整的周期（十分钟）才处理，会让运营在刚发布完的那几分钟里看到一批「明明过期了还写着
// 生效中」的人。
func (w *ExpiryWorker) Run(ctx context.Context) error {
	w.sweep(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 返回 ctx.Err()：这是「被要求停」，不是「跑完了」。runtime 靠这个区分正常退出与
			// 出错退出，别把它吞成 nil。
			return ctx.Err()
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *ExpiryWorker) sweep(ctx context.Context) {
	for round := 0; round < maxSweepsPerTick; round++ {
		// traceID 传空串：这是一次没有来路的后台扫描，编一个 id 只会让链路里多一条对不上的
		// 线索。变更流水里的 operator_type 记的是 worker，那才是它的身份。
		expired, err := w.memberships.ExpireDue(ctx, w.batch, "")
		if err != nil {
			// 记下来继续听，不让循环退出：退出会让这个副本从此不再扫到期，而没人扫的后果是
			// 会员状态永远停在 active——后台按状态筛「已过期」会一直筛不到人。
			//
			// 注意它**不影响用户能否享会员价**：判定读的是 expire_at（见 model.IsUsable），
			// 所以这一段滞后不会多送出去一分钟权益。这正是这个 worker 可以容忍失败的前提。
			slog.Error("membership expiry sweep", "error", err)
			return
		}
		if expired < w.batch {
			// 没扫满一批说明已经处理完了，不必再空跑一轮。
			return
		}
		if ctx.Err() != nil {
			// 每一批之间看一眼 ctx：积压深的时候别让这一轮拖住停机。
			return
		}
	}
	// 到这里的含义是「连续扫满了 maxSweepsPerTick 批还没扫完」：积压比预期深。留一条日志，
	// 否则这种情况从外部看只是「会员过期得比平常慢」，没人知道该查什么。
	slog.Warn("membership expiry sweep hit its per-tick limit; backlog may be building up",
		"batch", w.batch, "rounds", maxSweepsPerTick)
}
