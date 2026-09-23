package model

import (
	"encoding/json"
	"time"
)

// Win 对应 lottery_wins 表：一条中奖记录。
//
// 它和别的资产表不一样的地方是：中奖记录是**可变的状态行**（领取、换奖、核销都会改它），
// 所以它没有只增触发器；不可篡改的那一半由 WinEvent 承担。这与福卡（不可变流水 + 余额）
// 的形状相反，理由也不同——福卡是资产，中奖记录是流程。
//
// **本轮只有 pending 可达**：领取、核销、换奖整块延后（用户拍板）。其余取值是最终状态机
// 的一部分，先写进 CHECK，免得下一轮为了加一个词表再来一次迁移。同理，ClaimNo /
// Testimonial* / Redeem* 这几列本轮**没有写入方**，每条都写明了将来是谁写它。
type Win struct {
	ID         string `db:"id"`
	DrawID     string `db:"draw_id"`
	RoundID    string `db:"round_id"`
	CampaignID string `db:"campaign_id"`
	// 中奖的那一条参与记录。一次开奖里同一条参与只能中一次
	// （lottery_wins_participation_unique）。
	ParticipationID string `db:"participation_id"`
	UserID          string `db:"user_id"`
	RoundNo         string `db:"round_no"`
	CampaignName    string `db:"campaign_name"`
	PrizeID         string `db:"prize_id"`
	// 没有 PrizeKind：库里没有 prize_kind 这一列，快照也就无从谈起。
	//
	// 原奖品与现奖品分两列：换奖改的是 Current，Original 永远留着原样。
	OriginalPrizeName string `db:"original_prize_name"`
	CurrentPrizeName  string `db:"current_prize_name"`
	// 领取 / 核销的凭证号，给人念的（LW20260915-000123）。由列默认值从
	// lottery_claim_no_seq 生成，**不由 Go 侧拼**——序号要连续，只能有一个发号人。
	//
	// 本轮**没有写入方**：核销延后了。它将来的用途是门店端扫码/输号核销。
	ClaimNo string `db:"claim_no"`
	// pending / claimed / redeemed / expired / revoked / superseded，见下面的常量。
	Status string `db:"status"`
	// 获奖感言与图片（原型领取时必填 1–5 张）。两列本轮**都没有写入方**：
	// 图片上传随小程序一起延后，文字随领取一起延后。
	Testimonial       string          `db:"testimonial"`
	TestimonialImages json.RawMessage `db:"testimonial_images"`
	// 来源快照，从参与记录抄过来，让中奖详情不必回头 join 参与表。
	SourceOrderID    *string `db:"source_order_id"`
	SourceOrderNo    string  `db:"source_order_no"`
	SourceMachineID  *string `db:"source_machine_id"`
	SourceLocationID *string `db:"source_location_id"`
	// NULL = 不过期，与福卡一致（福卡到今天也没有过期这个概念）。本轮没有写入方：
	// 过期扫描是领取/核销那一轮的事。
	ExpiresAt *time.Time `db:"expires_at"`
	// 本轮**没有写入方**：领取延后了。
	ClaimedAt *time.Time `db:"claimed_at"`
	// 核销：谁、在哪家店、什么时候。三列本轮**都没有写入方**——核销整块延后，
	// 商户端一个字节都不动。
	RedeemedAt         *time.Time `db:"redeemed_at"`
	RedeemedBy         *string    `db:"redeemed_by"`
	RedeemLocationID   *string    `db:"redeem_location_id"`
	RedeemLocationName string     `db:"redeem_location_name"`
	CreatedAt          time.Time  `db:"created_at"`
	UpdatedAt          time.Time  `db:"updated_at"`
}

// 中奖状态，与 lottery_wins.status 的 CHECK 逐字一致。
//
// 只有 WinPending 本轮可达，其余五个是最终状态机的一部分。**不要因为「没人写」就把它们
// 删掉**：删掉之后下一轮加领取时要动的是 CHECK 约束，那是一次全表校验的迁移。
const (
	// WinPending 是中奖后的初始状态：等用户来领。
	WinPending = "pending"
	// WinClaimed 是用户已经领了（填了获奖感言）。本轮不可达。
	WinClaimed = "claimed"
	// WinRedeemed 是门店已经核销。本轮不可达。
	WinRedeemed = "redeemed"
	// WinExpired 是过了 expires_at 没领。本轮不可达（expires_at 也还没有写入方）。
	WinExpired = "expired"
	// WinRevoked 是被人工撤销（作弊、重复发奖）。本轮不可达。
	WinRevoked = "revoked"
	// WinSuperseded 是被重抽顶替。本轮不可达——重抽本身就不做。
	WinSuperseded = "superseded"
)

// WinEvent 对应 lottery_win_events 表：中奖记录改过什么，由这张只增的表回答。
//
// 本轮只会写 EventWinCreated（开奖时），其余取值留给领取 / 换奖 / 核销 / 过期。
type WinEvent struct {
	ID    string `db:"id"`
	WinID string `db:"win_id"`
	// 见下面那组常量，与 lottery_win_events.event_type 的 CHECK 逐字一致。
	EventType  string `db:"event_type"`
	FromStatus string `db:"from_status"`
	ToStatus   string `db:"to_status"`
	// user / merchant / admin / system，与 CHECK 逐字一致。
	ActorType string  `db:"actor_type"`
	ActorID   *string `db:"actor_id"`
	// 操作人的显示名快照。后台管理员改名之后，历史记录仍应显示当时那个名字。
	ActorName string          `db:"actor_name"`
	Reason    string          `db:"reason"`
	Metadata  json.RawMessage `db:"metadata"`
	CreatedAt time.Time       `db:"created_at"`
}

// 中奖流水的类型，与 lottery_win_events.event_type 的 CHECK 逐字一致。
const (
	EventWinCreated            = "created"
	EventWinClaimed            = "claimed"
	EventWinTestimonialUpdated = "testimonial_updated"
	EventWinRedeemed           = "redeemed"
	EventWinSwapped            = "swapped"
	EventWinSuperseded         = "superseded"
	EventWinRevoked            = "revoked"
	EventWinExpired            = "expired"
)

// 操作者类型，与 lottery_win_events.actor_type 的 CHECK 逐字一致。
const (
	ActorUser     = "user"
	ActorMerchant = "merchant"
	ActorAdmin    = "admin"
	ActorSystem   = "system"
)
