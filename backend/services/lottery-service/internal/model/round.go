package model

import "time"

// Round 对应 lottery_rounds 表：活动下面滚动开的一期一期。
//
// 原型里 LAKE-202608-12 已经到第 12 期，所以**不是一期一活动**：开奖后在同一个事务里
// 开下一期，一直到活动窗口结束或活动停用。活动带 start_at / end_at，期次在窗口内滚动，
// 每期的 ends_at 都取 campaign.end_at（「一期的截止就是活动的截止」）。**没有
// max_rounds**——原型没有这个概念。
type Round struct {
	ID         string `db:"id"`
	CampaignID string `db:"campaign_id"`
	// 活动内的序号，从 1 开始。round_no = '{campaign.code}-{seq:04d}'。
	Seq     int32  `db:"seq"`
	RoundNo string `db:"round_no"`
	// open / closed / drawn / cancelled，见下面的常量。
	Status string `db:"status"`
	// 开期时从活动冻结下来。之后改活动的 participant_target 不影响这一期。
	ParticipantTarget int32 `db:"participant_target"`
	// 已达标的参与数。**存下来而不是 COUNT(*)**：它既是抽奖中心每次渲染都要读的数，
	// 又是开奖 worker 的扫描判据——列比较能走索引，聚合不能。代价是可能与参与记录漂移，
	// 所以有一把行锁 + 一条集成测试盯着「= COUNT(*) WHERE status='confirmed'」，
	// 与账户域盯着「SUM(amount) = balance」是同一套办法。
	ParticipantCount int32 `db:"participant_count"`
	// 开期时 = 奖池 SUM(quantity)。实际抽出的人数还可能被参与数封顶
	// （min(winner_count, participant_count)，见 Draw）。
	WinnerCount int32     `db:"winner_count"`
	StartsAt    time.Time `db:"starts_at"`
	EndsAt      time.Time `db:"ends_at"`
	// 与 status 由 CHECK 绑定：drawn ⇔ 非空。开奖时刻不是一个可以忘的字段。
	DrawnAt *time.Time `db:"drawn_at"`
	// 作废那一组同理，与 status='cancelled' 绑定。
	CancelledAt  *time.Time `db:"cancelled_at"`
	CancelReason string     `db:"cancel_reason"`
	CancelledBy  *string    `db:"cancelled_by"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`
}

// 期次状态，与 lottery_rounds.status 的 CHECK 逐字一致。
const (
	// RoundOpen 是收人中：可以参与。
	RoundOpen = "open"
	// RoundClosed 是已达标、停止收人，等开奖。**不再接受参与**，但还没开奖。
	RoundClosed = "closed"
	// RoundDrawn 是终态：已经开过奖。一期只能开一次（lottery_draws_round_unique）。
	RoundDrawn = "drawn"
	// RoundCancelled 是终态：作废，没有开奖记录。本轮只允许 participant_count = 0
	// 的期次作废——有参与者的作废需要 N 次跨服务冲正，属下一轮。
	RoundCancelled = "cancelled"
)

// AcceptsParticipation 是参与入口在锁内用的那一条判定。
//
// 只看 status 不看时间：到点（NOW() >= ends_at）但状态还是 open 的期次，在开奖 worker
// 扫到它之前**仍然收人**是有意的——用户的卡已经在借记路上了，为了一个还没发生的开奖
// 抢先把人挡在门外，只会让「期次看起来还能点、点了报错」。
func (r *Round) AcceptsParticipation(now time.Time) bool {
	return r.Status == RoundOpen && now.Before(r.EndsAt)
}

// AwaitingDraw 回答「这一期现在该不该开奖」，以及是哪种触发。
//
// 两条路谁先到算谁：达标（status 已经是 closed，参与确认那一步置的）或到点
// （still open 但 NOW() >= ends_at）。返回值第二个是 trigger，与 lottery_draws.trigger
// 的 CHECK 逐字一致。
//
// **到点必开，不流局**：流局要把 N 张卡沿着 N 次跨服务冲正还回去，而那条路没有截止
// 时间；「参与后不可撤回」也是原型的明文规则。零人参与是唯一的例外，那一条在 worker
// 里直接 cancelled，不写开奖记录。
func (r *Round) AwaitingDraw(now time.Time) (bool, string) {
	switch {
	case r.Status == RoundClosed:
		return true, TriggerThreshold
	case r.Status == RoundOpen && !now.Before(r.EndsAt):
		return true, TriggerDeadline
	default:
		return false, ""
	}
}

// 开奖触发方式，与 lottery_draws.trigger 的 CHECK 逐字一致。
const (
	// TriggerThreshold 是人数达标。
	TriggerThreshold = "threshold"
	// TriggerDeadline 是到点（ends_at 已过）。
	TriggerDeadline = "deadline"
	// TriggerManual 是人工开奖。它与 mode='manual' 由 CHECK 绑成同一件事。
	TriggerManual = "manual"
)

// 开奖方式，与 lottery_draws.mode 的 CHECK 逐字一致。
const (
	DrawModeAuto   = "auto"
	DrawModeManual = "manual"
)
