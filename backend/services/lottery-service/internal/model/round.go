package model

import "time"

// Round 对应 lottery_rounds 表：活动下面滚动开的一期一期。
//
// 原型里 LAKE-202608-12 已经到第 12 期，所以**不是一期一活动**：开奖后在同一个事务里
// 开下一期，只要活动还是 enabled 就一直滚下去。**没有 max_rounds**——原型没有这个概念。
//
// 期次**没有截止时间**：它只有三条出路——收满 participant_target 转 closed 并开奖、
// 管理员人工开奖、管理员作废。收不满就一直开着，这是有意的，不是兜底。
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
	WinnerCount int32 `db:"winner_count"`
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
// 只看 status。**期次没有截止时间**：一个 open 的期次会一直收人，直到参与数把它顶到
// closed（见 repository.Confirm 里那一句 CASE）、管理员人工开奖、或管理员作废。
func (r *Round) AcceptsParticipation() bool {
	return r.Status == RoundOpen
}

// AwaitingDraw 回答「这一期现在该不该开奖」，以及是哪种触发。
//
// 只有一条路：**收满门槛**——status 已经是 closed，那是参与确认那一步置的。返回值第二个
// 是 trigger，与 lottery_draws.trigger 的 CHECK 逐字一致。
//
// 这里曾经还有第二条路（到点，open 且 ends_at 已过），2026-09-15 拿掉了：运营的心智是
// 「说好收满 10 次就开奖」，一个截止时间只会制造出「10 次没到也开了奖」这种没人预期过的
// 结果。**收不满就一直等着**，出口是管理员人工开奖或作废。
//
// 「参与后不可撤回」仍是原型的明文规则，所以也没有流局。零人参与只在人工开奖那一条路上
// 会遇到，那一条在 worker 里直接 cancelled，不写开奖记录。
func (r *Round) AwaitingDraw() (bool, string) {
	if r.Status == RoundClosed {
		return true, TriggerThreshold
	}
	return false, ""
}

// 开奖触发方式，与 lottery_draws.trigger 的 CHECK 逐字一致。
const (
	// TriggerThreshold 是参与次数达标。
	TriggerThreshold = "threshold"
	// TriggerManual 是人工开奖。它与 mode='manual' 由 CHECK 绑成同一件事。
	TriggerManual = "manual"
)

// 开奖方式，与 lottery_draws.mode 的 CHECK 逐字一致。
const (
	DrawModeAuto   = "auto"
	DrawModeManual = "manual"
)
