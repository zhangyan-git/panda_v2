package dto

import "time"

// PrizeRequest 是奖池里的一行，创建 / 修改活动时随活动一起提交。
//
// 奖池整体替换而不是逐行增删：后台那个表单就是「一张奖品表」，运营加一行删一行之后点保存，
// 提交的是那份完整清单。逐行接口会让前端自己算差集，而差集算错的后果是名额总数对不上，
// 那正是开奖时要用的数。
type PrizeRequest struct {
	// SortOrder 由前端的行序决定（0 起）。服务端不再排一遍——「哪个奖排前面」是运营的
	// 意思，不是我们能猜的。
	SortOrder         int32  `json:"sortOrder"`
	PrizeKind         string `json:"prizeKind"`
	Name              string `json:"name"`
	CouponTemplateID  string `json:"couponTemplateId"`
	ImageURL          string `json:"imageUrl"`
	ClaimInstructions string `json:"claimInstructions"`
	Quantity          int32  `json:"quantity"`
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
	// 见 MaxPageSize 上面的说明：新期次的默认门槛，开期时冻结到期次上。
	ParticipantTarget int32          `json:"participantTarget"`
	Description       string         `json:"description"`
	StartAt           time.Time      `json:"startAt"`
	EndAt             time.Time      `json:"endAt"`
	Status            string         `json:"status"`
	Prizes            []PrizeRequest `json:"prizes"`
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
	ParticipantTarget int32     `json:"participantTarget"`
	Description       string    `json:"description"`
	IsDefault         bool      `json:"isDefault"`
	StartAt           time.Time `json:"startAt"`
	EndAt             time.Time `json:"endAt"`
	Status            string    `json:"status"`
	// 奖池。列表接口不带它（一次 20 个活动、每个带 5 个奖品，列表就成了奖池查询），
	// 详情接口带。
	Prizes []CampaignPrizeResponse `json:"prizes"`
	// 奖池总名额 = SUM(prizes.quantity)，也就是下一期的 winner_count。
	// 存成响应字段而不是让前端自己加：运营看的就是这个数对不对。
	PrizeTotalQuantity int32 `json:"prizeTotalQuantity"`
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

// CampaignPrizeResponse 是奖池里的一行。
type CampaignPrizeResponse struct {
	ID                string `json:"id"`
	SortOrder         int32  `json:"sortOrder"`
	PrizeKind         string `json:"prizeKind"`
	Name              string `json:"name"`
	CouponTemplateID  string `json:"couponTemplateId"`
	ImageURL          string `json:"imageUrl"`
	ClaimInstructions string `json:"claimInstructions"`
	Quantity          int32  `json:"quantity"`
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
	// 门槛与已达标人数。列表页的进度条按这两个数画。
	ParticipantTarget int32      `json:"participantTarget"`
	ParticipantCount  int32      `json:"participantCount"`
	WinnerCount       int32      `json:"winnerCount"`
	StartsAt          time.Time  `json:"startsAt"`
	EndsAt            time.Time  `json:"endsAt"`
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
	// 同一次事务里开出来的下一期。为空表示这一期开完之后没有下一期了，此时
	// CampaignEnded 必然是 true——两句是同一个事实的两面，分开回是为了让后台不必
	// 靠「nextRoundId 是空的」去猜活动是不是结束了。
	NextRoundID string `json:"nextRoundId"`
	NextRoundNo string `json:"nextRoundNo"`
	// 这次开奖是否把活动置成了 ended（窗口已过或活动被人停用）。
	CampaignEnded bool `json:"campaignEnded"`
}

// CancelRoundRequest 是期次作废的请求体。
type CancelRoundRequest struct {
	Reason string `json:"reason"`
}
