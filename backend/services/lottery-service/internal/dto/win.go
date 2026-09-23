package dto

import "time"

// WinResponse 是一条中奖记录。
//
// 字段名与 admin-web/src/services/lottery.ts 里的类型、以及 ProTable 的 dataIndex
// **逐字一致**——改一个要同批改三处（见本包的文件头）。
type WinResponse struct {
	ID              string `json:"id"`
	DrawID          string `json:"drawId"`
	RoundID         string `json:"roundId"`
	RoundNo         string `json:"roundNo"`
	CampaignID      string `json:"campaignId"`
	CampaignName    string `json:"campaignName"`
	ParticipationID string `json:"participationId"`
	UserID          string `json:"userId"`
	PrizeID         string `json:"prizeId"`
	// 没有 prizeKind：库里没有 prize_kind 这一列，快照也就无从谈起。
	//
	// 原奖品与现奖品分两列，与表结构一致。换奖改的是 Current，Original 永远留着。
	OriginalPrizeName string `json:"originalPrizeName"`
	CurrentPrizeName  string `json:"currentPrizeName"`
	// 领取 / 核销的凭证号（LW20260915-000123）。本轮**没有写入方之外的用途**：
	// 核销延后了，这列先展示给用户看，他将来到门店要报的就是它。
	ClaimNo string `json:"claimNo"`
	// pending / claimed / redeemed / expired / revoked / superseded。
	// 本轮只有 pending 会出现。
	Status string `json:"status"`
	// 获奖感言与图片。两列本轮都没有写入方（见 model.Win 的说明），恒为空的形态。
	Testimonial       string   `json:"testimonial"`
	TestimonialImages []string `json:"testimonialImages"`
	// 来源快照。中奖详情不必回头 join 参与表。
	SourceOrderID    string `json:"sourceOrderId"`
	SourceOrderNo    string `json:"sourceOrderNo"`
	SourceMachineID  string `json:"sourceMachineId"`
	SourceLocationID string `json:"sourceLocationId"`
	// 领取截止时间，null = 不过期。本轮恒为 null。
	ExpiresAt *time.Time `json:"expiresAt"`
	ClaimedAt *time.Time `json:"claimedAt"`
	// 核销信息，本轮恒为空。
	RedeemedAt         *time.Time `json:"redeemedAt"`
	RedeemedBy         string     `json:"redeemedBy"`
	RedeemLocationID   string     `json:"redeemLocationId"`
	RedeemLocationName string     `json:"redeemLocationName"`
	CreatedAt          time.Time  `json:"createdAt"`
}

// WinEventResponse 是中奖记录的一条流水。
//
// 本轮只会出现 created。放在这里有第二层用途：后台的中奖详情用它展示「这条记录是谁、
// 什么时候、因为什么产生的」，而那张只增表就是用来回答这个问题的。
type WinEventResponse struct {
	ID         string `json:"id"`
	EventType  string `json:"eventType"`
	FromStatus string `json:"fromStatus"`
	ToStatus   string `json:"toStatus"`
	ActorType  string `json:"actorType"`
	ActorID    string `json:"actorId"`
	ActorName  string `json:"actorName"`
	Reason     string `json:"reason"`
	// Metadata 原样透出（JSON 对象）。它按事件类型有不同的形状，所以不在这里定成结构体
	// ——那是给前端按 eventType 分支去读的东西。
	Metadata  map[string]any `json:"metadata"`
	CreatedAt time.Time      `json:"createdAt"`
}

// WinDetailResponse 是中奖详情：记录本身 + 它的流水。
type WinDetailResponse struct {
	Win    WinResponse        `json:"win"`
	Events []WinEventResponse `json:"events"`
}

// MyWinSummary 是「我的中奖」顶部的三个数。
type MyWinSummary struct {
	Total int `json:"total"`
	// 待领取数。本轮它等于 Total——领取延后了，所有中奖都停在 pending。
	Pending int `json:"pending"`
}
