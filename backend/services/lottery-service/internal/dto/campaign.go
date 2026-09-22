package dto

import "time"

// PrizeRequest 是那个唯一的奖品，创建 / 修改活动时随活动一起提交。
//
// 一个活动一个奖品，所以它是一个对象而不是数组。ID 在修改时**必须原样带回来**：奖品行被
// 中奖记录引用着（lottery_wins.prize_id 是 ON DELETE RESTRICT），不带 id 会让服务端把旧行
// 删掉重插，而那次删除会被外键拒绝。带上 id 就是原地 UPDATE，中奖记录里那两列名字快照
// （original_ / current_prize_name）本来就等着这一刻。
//
// 这里原先还有 sortOrder / prizeKind / couponTemplateId / quantity 四个字段，
// 2026-09-15 随 migrations/lottery/005 一起删了。
type PrizeRequest struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	CoverImage        string `json:"coverImage"`
	PosterImage       string `json:"posterImage"`
	ClaimInstructions string `json:"claimInstructions"`
}

// CampaignRequest 是新建 / 修改活动的请求体。
//
// ActivationID 在新建时必填；修改时被忽略（活动不能换门店，那等于新建一个）。
type CampaignRequest struct {
	ActivationID string `json:"activationId"`
	// MachineID 为空 = 门店级活动。
	MachineID *string `json:"machineId"`
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	// 见 MaxPageSize 上面的说明：新期次的默认门槛，开期时冻结到期次上。**数的是参与次数**。
	//
	// 这里原先还有 startAt / endAt 一对活动窗口，2026-09-15 整块删了：活动没有截止时间，
	// 只会被人为结束。
	ParticipantTarget int32  `json:"participantTarget"`
	Description       string `json:"description"`
	Status            string `json:"status"`
	// 这个活动的奖品，必填。
	Prize PrizeRequest `json:"prize"`
}

// CampaignResponse 是一个活动（含奖池）。
type CampaignResponse struct {
	ID           string `json:"id"`
	ActivationID string `json:"activationId"`
	LocationID   string `json:"locationId"`
	LocationName string `json:"locationName"`
	// 为空 = 门店级活动。前端的「适用设备」列按它有没有值渲染。
	MachineID *string `json:"machineId"`
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	// 见 dto.CampaignRequest.ParticipantTarget。
	ParticipantTarget int32  `json:"participantTarget"`
	Description       string `json:"description"`
	IsDefault         bool   `json:"isDefault"`
	Status            string `json:"status"`
	// 奖品。列表接口不带它（一次 20 个活动、每个带一张图，列表响应会白胖一圈），详情接口带。
	Prize *CampaignPrizeResponse `json:"prize"`
	// 这里原先还有一个 prizeTotalQuantity = SUM(prizes.quantity)，即「下一期的 winner_count」。
	// 名额恒为 1 之后它变成了第二份事实，而期次上那个冻结的 winnerCount 才是真的（同一个
	// 理由删掉了 draw 的 campaignEnded）。2026-09-15 删掉。
	//
	// 在跑的那一期（open / closed），没有则为空。
	LiveRoundID   string `json:"liveRoundId"`
	LiveRoundNo   string `json:"liveRoundNo"`
	LiveRoundSize int32  `json:"liveRoundSize"`
	LiveRoundDone int32  `json:"liveRoundDone"`
	// 期次数，列表页显示「已开 12 期」。
	RoundCount int       `json:"roundCount"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// CampaignPrizeResponse 是那个唯一的奖品。
//
// 不回 quantity：名额恒为 1，看它的地方（期次上的 winnerCount）已经有一份冻结过的真值。
type CampaignPrizeResponse struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	CoverImage        string `json:"coverImage"`
	PosterImage       string `json:"posterImage"`
	ClaimInstructions string `json:"claimInstructions"`
}

// RoundResponse 是一期。
type RoundResponse struct {
	ID         string `json:"id"`
	CampaignID string `json:"campaignId"`
	// 活动的短名与名字，跟着期次一起回：后台的期次列表是按活动分组看的，
	// 少了它们每一行都要回头查活动。
	CampaignCode string `json:"campaignCode"`
	CampaignName string `json:"campaignName"`
	Seq          int32  `json:"seq"`
	RoundNo      string `json:"roundNo"`
	Status       string `json:"status"`
	// 门槛与已达标的参与次数。列表页的进度条按这两个数画。
	//
	// 这里原先还有 startsAt / endsAt 一对期次窗口，2026-09-15 随活动窗口一起删了。期次没有
	// 起止时间：它只有三条出路——收满门槛转 closed 并开奖、人工开奖、作废。
	ParticipantTarget int32      `json:"participantTarget"`
	ParticipantCount  int32      `json:"participantCount"`
	WinnerCount       int32      `json:"winnerCount"`
	DrawnAt           *time.Time `json:"drawnAt"`
	CancelledAt       *time.Time `json:"cancelledAt"`
	CancelReason      string     `json:"cancelReason"`
	// 开奖记录 ID / 方式 / 触发，未开奖为空。列表页要能直接点进那一次开奖。
	DrawID      string `json:"drawId"`
	DrawMode    string `json:"drawMode"`
	DrawTrigger string `json:"drawTrigger"`
	// 实际中奖人数（不是 winner_count 那个名额数）。开奖后才有值。
	ActualWinnerCount int32     `json:"actualWinnerCount"`
	CreatedAt         time.Time `json:"createdAt"`
}

// DrawRequest 是人工开奖的请求体。
//
// ExpectedRoundStatus / ExpectedParticipantCount **必填**：管理员拿着一个页面点了开奖，
// 而那个页面可能是三十秒前加载的，这期间期次可能已经达标关闭、已经被自动开奖、甚至
// 已经被作废。对不上就回 409，而不是替一个已经变了的局面决定谁中奖。
//
// 与 account-service 人工调整余额那条路（identity/021）是同一条思路：全系统唯一能凭空
// 决定资产归属的动作，必须先证明自己看的是当前状态。
type DrawRequest struct {
	Reason                   string `json:"reason"`
	ExpectedRoundStatus      string `json:"expectedRoundStatus"`
	ExpectedParticipantCount *int32 `json:"expectedParticipantCount"`
}

// DrawResponse 是一次开奖的结果。
type DrawResponse struct {
	DrawID  string `json:"drawId"`
	RoundID string `json:"roundId"`
	RoundNo string `json:"roundNo"`
	Mode    string `json:"mode"`
	Trigger string `json:"trigger"`
	// Seed 回给调用方：人工开奖之后管理员要能把这次开奖记录下来备查，
	// 而种子是复核的全部依据。
	Seed             string    `json:"seed"`
	Algorithm        string    `json:"algorithm"`
	ParticipantCount int32     `json:"participantCount"`
	WinnerCount      int32     `json:"winnerCount"`
	CreatedAt        time.Time `json:"createdAt"`
	// 本次开出的中奖记录（可能被参与数截断，所以它与 WinnerCount 一样长）。
	Winners []WinResponse `json:"winners"`
	// 同一次事务里开出来的下一期。为空表示这一期开完之后没有下一期了——活动不在 enabled
	// （被暂停、被结束、还是草稿），或者零人参与那一条路。
	//
	// 这里原先还有一个 campaignEnded：它表达的「开完之后没有下一期」与 nextRoundId 为空
	// 是同一件事，是一份会走偏的第二事实，而且窗口删掉之后它的名字（活动被时间结束了）
	// 直接是错的。2026-09-15 删掉，看 nextRoundId 就够。
	NextRoundID string `json:"nextRoundId"`
	NextRoundNo string `json:"nextRoundNo"`
}

// CancelRoundRequest 是期次作废的请求体。
type CancelRoundRequest struct {
	Reason string `json:"reason"`
}
