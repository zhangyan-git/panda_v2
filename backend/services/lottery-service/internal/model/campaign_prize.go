package model

import "time"

// CampaignPrize 对应 lottery_campaign_prizes 表：奖池里的一行。
//
// Quantity 是**每期**的中奖名额：开期时把 SUM(quantity) 冻结成期次的 winner_count，
// 开奖时按 SortOrder 依次分配名额。原型一个活动只有一个 prize 字符串，那落在这一张表的
// 一行上——不为单奖品另设一条路。
type CampaignPrize struct {
	ID         string `db:"id"`
	CampaignID string `db:"campaign_id"`
	// 分配名额的顺序：sort_order 小的先拿满自己的 quantity 份，再往下走。
	SortOrder int32 `db:"sort_order"`
	// coupon / coffee / physical / custom，见下面的常量。
	PrizeKind string `db:"prize_kind"`
	// 展示名（「10 元咖啡兑换券」）。中奖记录会把开奖那一刻的值快照成
	// original_prize_name / current_prize_name，之后改这里不会回头改已开出的奖。
	Name              string    `db:"name"`
	CouponTemplateID  string    `db:"coupon_template_id"`
	ImageURL          string    `db:"image_url"`
	ClaimInstructions string    `db:"claim_instructions"`
	Quantity          int32     `db:"quantity"`
	CreatedAt         time.Time `db:"created_at"`
	UpdatedAt         time.Time `db:"updated_at"`
}

// 奖品类型，与 lottery_campaign_prizes.prize_kind 的 CHECK 逐字一致。
//
// 本轮**四种都不兑付**：中奖记录停在 pending，领取/核销/换奖整块延后（用户拍板）。
// 词表先按最终形态写进 CHECK，免得下一轮为了加一个取值再来一次迁移。
const (
	PrizeKindCoupon   = "coupon"
	PrizeKindCoffee   = "coffee"
	PrizeKindPhysical = "physical"
	PrizeKindCustom   = "custom"
)

// ValidPrizeKind 让服务层在写库之前就能拒掉拼错的取值。
//
// 与「放过它、由 CHECK 报 23514」的区别只在报错的样子：一个是 400 带一句人话，
// 一个是 500。取值集合必须与上面那四个常量一起改。
func ValidPrizeKind(kind string) bool {
	switch kind {
	case PrizeKindCoupon, PrizeKindCoffee, PrizeKindPhysical, PrizeKindCustom:
		return true
	default:
		return false
	}
}
