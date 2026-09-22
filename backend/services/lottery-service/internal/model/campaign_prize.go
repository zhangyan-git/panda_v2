package model

import "time"

// CampaignPrize 对应 lottery_campaign_prizes 表：奖池里的那一行。
//
// **一个活动恰好一行**（表上有 UNIQUE(campaign_id)）。原型里活动的 prize 就是一个字符串，
// 这里曾经按通用模型开了 1..N 行的口子（带 sort_order 的分配顺序、prize_kind 的类型、
// 每档自己的名额），运营侧实际从来只填一个——2026-09-15 收敛掉，多余的列随 005 一起删了。
//
// Quantity 是**每期**的中奖名额，当前恒为 1（表单里没有这个字段）：开期时它被冻结成期次的
// winner_count，之后改这里不影响已经开出的期次。留着一列而不是写死 1，是为了将来「一期发
// 多份」时有个封存位可改。
type CampaignPrize struct {
	ID         string `db:"id"`
	CampaignID string `db:"campaign_id"`
	// 展示名（「10 元咖啡兑换券」）。中奖记录会把开奖那一刻的值快照成
	// original_prize_name / current_prize_name，之后改这里不会回头改已开出的奖。
	Name string `db:"name"`
	// 封面图，必填，用在活动卡片上；海报图可留空，用在活动详情顶部的横幅。
	// 两张图的比例**待定**，所以这里只记用途。
	CoverImage        string    `db:"cover_image"`
	PosterImage       string    `db:"poster_image"`
	ClaimInstructions string    `db:"claim_instructions"`
	Quantity          int32     `db:"quantity"`
	CreatedAt         time.Time `db:"created_at"`
	UpdatedAt         time.Time `db:"updated_at"`
}
