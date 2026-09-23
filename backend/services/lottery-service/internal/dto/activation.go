package dto

import "time"

// ActivateRequest 是「开通门店抽奖」的请求体。
//
// 请求体里**只有门店 id，没有门店名**：名字是商户域的事实，本库不留（见
// migrations/lottery）。服务端在开通前拿这个 id 问一次商户域「这家店存在吗」，问不出
// 来就不受理；显示用的名字则在每次读的时候现解（service.resolveStoreNames）。
//
// 原先这里有一个 locationName，由后台选择器带上来。去掉它换来的正是那条存在性校验：
// 受理一个由客户端写过来的名字，等于把「这家店真的存在」的判断权交给了客户端。
type ActivateRequest struct {
	LocationID string `json:"locationId"`
	Remark     string `json:"remark"`
	// 这两个都是可选的，**两个都不给也成立**：默认活动由内置模板建（名取
	// service.DefaultCampaignName、门槛取 service.DefaultCampaignTarget，见 service.Activate）。
	// 运营可以在开通的同一个动作里把它们改成自己要的。
	//
	// 分开成两个可空字段而不是一个嵌套对象：后台那个表单就是「开通 + 一个默认活动」
	// 一屏，嵌套一层只会让前端多拼一次结构。
	CampaignName string `json:"campaignName"`
	// ParticipantTarget 是第一期（以及之后每一期）的开奖门槛，**数的是参与次数**。
	//
	// 原先这里还有一对 startAt / endAt 组成默认活动的窗口，2026-09-15 随活动窗口一起删了：
	// 一期收满门槛就开奖，没满就一直等着，期次与活动都没有截止时间。
	ParticipantTarget *int32 `json:"participantTarget"`
}

// ActivationResponse 是一条开通记录。
type ActivationResponse struct {
	ID         string `json:"id"`
	LocationID string `json:"locationId"`
	// 门店名是**读这一刻**向商户域解出来的，不是开通时的快照；解不出来时是空串（商户域
	// 不可达，或那个 id 商户域已经不认识了）。门店的身份是上面那一列 id。
	LocationName string `json:"locationName"`
	Status       string `json:"status"`
	Remark       string `json:"remark"`
	// 默认活动的 ID 与名字。列表页要能直接点进默认活动，也要能回答「这家店开通的是
	// 哪个活动」。它来自 lottery_campaigns.is_default，不是存在开通行上的列
	// （见 migrations/lottery 为什么不多存一列）。
	DefaultCampaignID   string `json:"defaultCampaignId"`
	DefaultCampaignName string `json:"defaultCampaignName"`
	// 活动数（含默认）。列表页显示「3 个活动」。
	CampaignCount int       `json:"campaignCount"`
	ActivatedBy   string    `json:"activatedBy"`
	ActivatedAt   time.Time `json:"activatedAt"`
	// 停用时间，未停用为 null。
	DeactivatedAt *time.Time `json:"deactivatedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	// 这个门店在跑的期次 ID 与单号，没有则为空。
	//
	// 未开奖的那一期是运营最关心的东西（「现在第几期、还差几个人」），列表页直接显示它，
	// 省掉「点进活动再点进期次」那两步。
	LiveRoundID   string `json:"liveRoundId"`
	LiveRoundNo   string `json:"liveRoundNo"`
	LiveRoundSize int32  `json:"liveRoundSize"`
	LiveRoundDone int32  `json:"liveRoundDone"`
}

// UpdateActivationRequest 是开通记录上可改的那两样。
//
// 门店 ID 不能改：改门店等于换一家店开通，那是停用 + 新开通两件事，不是一次 PUT。
type UpdateActivationRequest struct {
	Status string `json:"status"`
	Remark string `json:"remark"`
}
