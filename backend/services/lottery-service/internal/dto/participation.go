package dto

import "time"

// ParticipateRequest 是小程序「参与抽奖」的请求体。
//
// 两个字段都可空，但语义完全不同：
//   - SourceOrderID 给了，就表示「用这张订单送的福卡参与」，幂等键由此派生，
//     一笔订单只能参与一次（UNIQUE (idempotency_key) 挡）。
//   - 不给，就是「从抽奖中心直接参与」，这时必须带 Idempotency-Key 请求头。
//
// **没有 amount / cost 字段**：一次参与扣几张是原型的规则（一次一张），由服务端定死。
// 让客户端传这个数，等于让一个被篡改的小程序决定扣几张卡。
type ParticipateRequest struct {
	SourceOrderID string `json:"sourceOrderId"`
	// 参与当时这台设备的快照（从设备二维码进来时带上）。它只进中奖记录的
	// 「来源点位」，不参与任何判定——「这台设备有没有在跑的活动」由活动自己的
	// machine_id 决定，不由请求体决定。
	SourceMachineID string `json:"sourceMachineId"`
	// 参与当时的门店快照。同上，只用于展示。
	SourceLocationID string `json:"sourceLocationId"`
}

// ParticipationResponse 是一条参与记录。
type ParticipationResponse struct {
	ID           string `json:"id"`
	RoundID      string `json:"roundId"`
	RoundNo      string `json:"roundNo"`
	CampaignID   string `json:"campaignId"`
	CampaignName string `json:"campaignName"`
	UserID       string `json:"userId"`
	// 这一笔是从哪张订单来的（「直接参与」为空）。
	SourceOrderID string `json:"sourceOrderId"`
	SourceOrderNo string `json:"sourceOrderNo"`
	// 见 dto.ParticipateRequest 的 SourceMachineID。
	SourceMachineID  string `json:"sourceMachineId"`
	SourceLocationID string `json:"sourceLocationId"`
	Cost             int32  `json:"cost"`
	// pending / confirmed / failed / reversed。
	Status      string `json:"status"`
	FailureCode string `json:"failureCode"`
	// 账户域的账变 ID：对账时从这一笔参与反查那次扣卡。
	FortuneEntryID string     `json:"fortuneEntryId"`
	CreatedAt      time.Time  `json:"createdAt"`
	ConfirmedAt    *time.Time `json:"confirmedAt"`
	// 这一笔有没有中奖。中奖记录按 participation_id 反查，一期内一条参与只能中一次。
	WinID      string `json:"winId"`
	WinClaimNo string `json:"winClaimNo"`
	PrizeName  string `json:"prizeName"`
}

// ParticipateResponse 是参与的结果。
//
// Replayed 为 true 表示这个幂等键之前就参与过了（同一张订单点了两下，或者上一次的响应
// 丢了客户端重试），这一份是**回放**：卡只扣了一次，期次的计数也只加了一次。
// 「参与处理中」那条路（扣卡结果未知）不会走到这里，它是 202。
type ParticipateResponse struct {
	Participation ParticipationResponse `json:"participation"`
	// 这一期还差几次参与到门槛；已达标时是 0。抽奖中心的进度条用它。
	//
	// **数的是次数**：同一个人可以在同一期参与多次，每次各记一笔。
	Remaining int32 `json:"remaining"`
	Replayed  bool  `json:"replayed"`
}

// RoundProgress 是抽奖中心的期次进度（原型顶部那块「本期已参与/目标」）。
type RoundProgress struct {
	RoundID string `json:"roundId"`
	RoundNo string `json:"roundNo"`
	// open / closed / drawn / cancelled。
	Status string `json:"status"`
	// 已达标的参与次数与门槛。没满时 Status 仍是 open——**它会一直开着**，没有截止时间，
	// 前端不要拿时间自己解释（期次只有收满、人工开奖、作废三条出路）。
	//
	// 这里原先还有一个 endsAt，2026-09-15 随活动窗口一起删了。
	ParticipantCount  int32 `json:"participantCount"`
	ParticipantTarget int32 `json:"participantTarget"`
	// 我正在参与的期次里已经记了几笔（同一期可以参与多次）。
	MyParticipationCount int32 `json:"myParticipationCount"`
	// 我剩下的福卡张数，从 account-service 实时读。**不是本库的数**：本库根本没有余额列。
	MyFortuneCardBalance int64 `json:"myFortuneCardBalance"`
	// 余额读不到时为 true（账户域不可达）。前端据此显示「余额暂时读不到」而不是把
	// 一个 0 显示成「你没有卡了」——那两句话的意思完全不同。
	BalanceUnavailable bool `json:"balanceUnavailable"`
}

// CampaignCenterResponse 是抽奖中心页要的全部东西。
type CampaignCenterResponse struct {
	Campaign CampaignResponse `json:"campaign"`
	Round    *RoundProgress   `json:"round"`
}
