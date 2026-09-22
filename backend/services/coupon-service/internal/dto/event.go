package dto

import "time"

// EventVersion 是本服务**发出**的事件版本（发券时写进 message_outbox）。
const EventVersion = "v1"

// 本服务**消费**的事件类型。它们是 membership-service 发出的路由键，逐字照抄那边的常量。
//
// 改一个字就等于退订：消息照样发出来，只是再也不会进本服务的队列，而**这不会有任何报错**
// ——队列绑定不上时 RabbitMQ 不会通知任何人。绑定靠 RABBITMQ_ROUTING_KEY（逗号分隔可订阅
// 多个键），那份配置在 deploy 的 compose 与 .env.example 里，两边要同时改。
const (
	// EventMembershipActivated 是「这个人第一次成为会员」。
	EventMembershipActivated = "membership.activated"
	// EventMembershipRenewed 是「有效期被往后叠了一段」（支付续费、或后台调整有效期）。
	EventMembershipRenewed = "membership.renewed"
	// EventMembershipCampaignClaimed 是「有人扫码领了店铺码活动的会员」。**本服务订阅它**，
	// 因为那次领取同时还承诺了 N 张券（见 MembershipCampaignClaimedEvent）。
	EventMembershipCampaignClaimed = "membership.campaign.claimed"
)

// MembershipChangedEvent 是 membership-service 的 `membership.activated` /
// `membership.renewed` 在本服务这一侧的镜像。
//
// **镜像而不是共享**：两边各写一份结构体与 json tag，消费时用 DisallowUnknownFields（见
// service.HandleEvent）。共享一个包会让 producer 加一个字段就悄悄改掉 consumer 的行为，而
// 跨服务契约的每一次漂移都该在联调时炸出来。这与 membership-service 镜像 order.paid 是同一
// 套写法。
//
// 两条事件共用一个形状（那边就是这么发的）：本服务要读的字段完全一样，分两个结构体只会让
// 这里多一个用不上的分支。
//
// **发券要用的是「成交快照」那三列**（memberPriceMode / memberPriceCouponTemplateId /
// memberPriceCouponsPerPeriod），不是现查套餐：会员买的时候是什么规则，就按什么规则发。
type MembershipChangedEvent struct {
	// MembershipID 与 OrderID 一起构成幂等键——见 service 里的说明。
	MembershipID string `json:"membershipId"`
	UserID       string `json:"userId"`
	PlanCode     string `json:"planCode"`
	PlanName     string `json:"planName"`
	// MemberPriceMode 是成交快照上的那条会员价路径：auto=自动享（不发券），coupon=靠券
	// （要发券）。它决定这条事件要不要处理。
	MemberPriceMode string `json:"memberPriceMode"`
	// 下面两列只在 coupon 模式下有值。库上那条 CHECK 把它们与 mode 钉在一起：mode=coupon
	// 就一定有值，反之一定为空。
	MemberPriceCouponTemplateID string `json:"memberPriceCouponTemplateId,omitempty"`
	MemberPriceCouponsPerPeriod int32  `json:"memberPriceCouponsPerPeriod,omitempty"`
	// Status / ExpireAt 本服务不读，但必须镜像（DisallowUnknownFields 要求覆盖生产者发来的
	// 每一个字段，否则整条消息解不开）。
	Status   string    `json:"status"`
	ExpireAt time.Time `json:"expireAt"`
	// OrderID 在支付驱动的开通与续费上有值，后台人工调整有效期、到期、撤销上没有（那些路径
	// 传空串）。**它是「这次变更是不是钱换来的」的判据**：后台把有效期往后挪一年也发这条
	// 事件，照发的话每改一次就白送一批券。
	OrderID    string    `json:"orderId,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

// MembershipCampaignClaimedEvent 是 membership-service 的
// `membership.campaign.claimed` 在本服务这一侧的镜像。
//
// 一次扫码领会员**同时**送出两样东西：N 天会员（会员域自己发）与 M 张券（本服务发）。
// 两件事各有各的账本，靠这条事件连起来——会员域不替券记账，券域也不碰会员。
//
// 券那一半是可选的（CouponTemplateID 为空 = 这场活动没配券），所以本服务看见空模板时
// 什么都不做、直接 ack。**认一个不认识的东西比认一个空的东西危险得多**：活动本来就没配券，
// 报错只会把队列堵满。
type MembershipCampaignClaimedEvent struct {
	// ClaimID 是会员域那条领取记录（membership_campaign_claims.id），也是本服务发券的幂等键
	// ——一个人一场活动只能领一次，重投时靠它命中上一次的批次。
	ClaimID    string `json:"claimId"`
	CampaignID string `json:"campaignId"`
	UserID     string `json:"userId"`
	// StoreID 是活动门店，也是这次领到的会员的归属门店。它同时决定券的适用范围：领到的券
	// **只能在这家店用**（老系统同一口径，见 service/handler）。
	StoreID string `json:"storeId"`
	// PlanName / GiftDays 本服务不读（券与会员天数无关），必须镜像：DisallowUnknownFields
	// 要求覆盖生产者发来的每一个字段。
	PlanName string `json:"planName"`
	GiftDays int32  `json:"giftDays"`
	// CouponTemplateID / CouponCount 是这次要发的券。**是领取那一刻的快照**：活动后来改了
	// 券的配置，已经领过的那次不受影响。模板为空 = 这场活动只送会员天数、不送券。
	CouponTemplateID string    `json:"couponTemplateId,omitempty"`
	CouponCount      int32     `json:"couponCount,omitempty"`
	OccurredAt       time.Time `json:"occurredAt"`
}
