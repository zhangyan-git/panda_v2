package dto

import "time"

// ActivateRequest 是「开通门店抽奖」的请求体。
//
// LocationName 由后台选择器带上来（它已经在门店下拉里拿到了名字）。不在这里回头去问
// 商户服务：开通是一个动作，重试它不该因为一次跨服务读失败而失败；而且商户服务改了店名
// 之后，这条记录仍然应当显示开通那一刻的名字。
type ActivateRequest struct {
	LocationID   string `json:"locationId"`
	LocationName string `json:"locationName"`
	Remark       string `json:"remark"`
	// 这两个都是可选的，**两个都不给也成立**：默认活动由内置模板建（名取
	// service.DefaultCampaignName、门槛取 service.DefaultCampaignTarget，见 service.Activate）。
	// 运营可以在开通的同一个动作里把它们改成自己要的。
	//
	// 分开成两个可空字段而不是一个嵌套对象：后台那个表单就是「开通 + 一个默认活动」
	// 一屏，嵌套一层只会让前端多拼一次结构。
	CampaignName      string `json:"campaignName"`
	ParticipantTarget *int32 `json:"participantTarget"`
	// 默认活动的窗口。都不给时取「现在起 90 天」——开通一个门店抽奖而它当场就结束
	// 显然是错的，所以必须有一个兜底的窗口，而不是要求运营每次都填。
	StartAt *time.Time `json:"startAt"`
	EndAt   *time.Time `json:"endAt"`
}

// ActivationResponse 是一条开通记录。
type ActivationResponse struct {
	ID           string `json:"id"`
	LocationID   string `json:"locationId"`
	LocationName string `json:"locationName"`
	Status       string `json:"status"`
	Remark       string `json:"remark"`
	// 默认活动的 ID 与名字。列表页要能直接点进默认活动，也要能回答「这家店开通的是
	// 哪个活动」。它来自 lottery_campaigns.is_default，不是存在开通行上的列
	// （见 migrations/lottery/001 为什么不多存一列）。
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
