package dto

import "time"

// 店铺码会员活动的形状。字段名与小程序码、后台表单逐字对应（frontend 那两份把它们当
// dataIndex 用），改一处要同批改三处，否则 TypeScript 不报错、只是整列空白。

// CampaignQuery 是后台活动列表的筛选条件。
type CampaignQuery struct {
	// draft / enabled / disabled，空表示不筛。
	Status string
	// 模糊搜（ILIKE）：按活动名或 scene 搜。运营手里那半截可能是「五一活动」，也可能是码上
	// 印着的那串 smc_xxx。
	Keyword  string
	Page     int
	PageSize int
}

// CampaignClaimQuery 是一场活动的领取记录列表的筛选条件。
//
// 只有分页：领取记录永远是**先选定一场活动**再看（活动在路径上），不需要再按状态或用户筛——
// 一个人一场活动只能有一条记录，筛用户等于直接看那一条。
type CampaignClaimQuery struct {
	Page     int
	PageSize int
}

// CampaignResponse 是一场活动的后台形状。
type CampaignResponse struct {
	ID string `json:"id"`
	// Name 是给运营看的名字，不出现在码里。
	Name string `json:"name"`
	// Scene 是小程序码带的参数，扫码进来靠它找回活动。全库唯一。
	Scene string `json:"scene"`
	// StoreID 是活动门店，也是领到的会员的归属门店。StoreName 是**后端现解出来的**
	// （会员库只存门店 id，名字是商户域的事实），解不出来时是空串——商户域抖一下不该让整页
	// 打不开。显示时按「空串 ⇒ 显示 id 或 —」处理。
	StoreID   string `json:"storeId"`
	StoreName string `json:"storeName"`
	// PlanID / PlanName 是送的是哪个套餐的会员。名字同样来自后端（本库 join）。
	PlanID   string `json:"planId"`
	PlanName string `json:"planName"`
	// GiftDays 是送多少天。与套餐的时长无关：这是一次赠送，按天算。
	GiftDays int32     `json:"giftDays"`
	StartAt  time.Time `json:"startAt"`
	EndAt    time.Time `json:"endAt"`
	// draft / enabled / disabled。**只有 enabled 且落在窗口内的活动能被扫到。**
	Status string `json:"status"`
	// CouponTemplateID / CouponCount 是每领一次送的券（模板在券库，本服务只存 id）。
	// **空串与 0 = 这场活动只送会员天数**，库里那两列是 NULL，形状转换时归一化成零值。
	//
	// 它是跨库值引用：本服务查不到那张模板存不存在，配错了要等发券那一刻才现形（券服务会
	// 记一条日志并跳过，不会回头改这里）——见 migrations/membership 那段说明。
	CouponTemplateID string `json:"couponTemplateId"`
	CouponCount      int32  `json:"couponCount"`
	// QRCodeURL 今天恒为空：生成小程序码要走微信的 wxacode.getUnlimited，而 appid/secret
	// 与 access_token 缓存都在 user-service，本服务没有任何微信配置。这一列描述的是活动的事实，
	// 没有码的时候空着，语义清楚。
	QRCodeURL         string     `json:"qrCodeUrl"`
	QRCodeGeneratedAt *time.Time `json:"qrCodeGeneratedAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// CampaignRequest 是新建 / 修改活动的请求体。
//
// 新建与修改共用同一个结构体、同一套校验：两者的差别只在「这条记录之前存在吗」。与
// PlanRequest 同一条理由。
//
// **没有 Status**：新建一律 draft，启停走单独的动作（CampaignStatusRequest）。合在一起的话，
// 运营改一句活动名会把正开着的活动顺手存回 draft——而那一刻可能正有人拿着码在扫。
type CampaignRequest struct {
	Name string `json:"name"`
	// Scene 是码上那串参数。**格式与唯一性都校验**：它进了码之后就改不动了（改 scene 会让已经
	// 印出去的码静默失效），所以格式不对要被挡在第一次保存时。
	Scene    string `json:"scene"`
	StoreID  string `json:"storeId"`
	PlanID   string `json:"planId"`
	GiftDays int32  `json:"giftDays"`
	// StartAt / EndAt 是活动的有效期窗口（闭开区间：EndAt 那一刻已经领不了）。
	StartAt time.Time `json:"startAt"`
	EndAt   time.Time `json:"endAt"`
	// CouponTemplateID / CouponCount 是每次领取送的券，**要么都填、要么都不填**：只填一半等于
	// 「该发券」静默变成「什么都不发」。空串 + 0 = 不送券。
	CouponTemplateID string `json:"couponTemplateId"`
	CouponCount      int32  `json:"couponCount"`
}

// CampaignStatusRequest 是启停 / 转草稿的请求体。动作本身只有这一个字段。
type CampaignStatusRequest struct {
	Status string `json:"status"`
}

// CampaignClaimResponse 是一条领取记录的后台形状。
type CampaignClaimResponse struct {
	ID         string `json:"id"`
	CampaignID string `json:"campaignId"`
	UserID     string `json:"userId"`
	// StoreID / GiftDays 是**发放那一刻的快照**：活动后来改了门店或天数，这一次不受影响。
	StoreID  string `json:"storeId"`
	GiftDays int32  `json:"giftDays"`
	// CouponTemplateID / CouponCount 同样是快照，且是**承诺**而不是结果：实际发了几张在券库
	// （user_coupons.campaign_claim_id 指着这条记录）。后台要看「发出去没有」得去券库查，
	// 本页答不了那个问题——这里只答「这一领取承诺了什么」。
	CouponTemplateID string `json:"couponTemplateId"`
	CouponCount      int32  `json:"couponCount"`
	// MembershipID 是这次领取派生出的那条会员，MembershipExpireAt 是那一刻算出来的到期时刻
	// （会员行上的到期时间会随后台调整、续费而变，这里记的是当时送到哪天）。
	MembershipID       string    `json:"membershipId"`
	MembershipExpireAt time.Time `json:"membershipExpireAt"`
	CreatedAt          time.Time `json:"createdAt"`
}

// CampaignClaimRequest 是小程序端的领取请求。
//
// **只有 scene**：用户是谁从登录态来（不接客户端给的 user_id），活动门店、套餐、天数全部由
// scene 解出来的那一行决定。客户端能影响的只有「扫的是哪张码」。
type CampaignClaimRequest struct {
	Scene string `json:"scene"`
}

// CampaignClaimResult 是小程序端领取成功后的回执。
//
// 它**不是**领取记录的完整形状：用户要看的是「我领到了什么」——送到哪天、送的哪一款会员。
// 内部 id（campaign_id / store_id / membership_id）对他没有用处。
type CampaignClaimResult struct {
	// MembershipExpireAt 是这次赠送之后的到期时刻。
	MembershipExpireAt time.Time `json:"membershipExpireAt"`
	PlanName           string    `json:"planName"`
	GiftDays           int32     `json:"giftDays"`
	// CouponCount 是这次领取一并给出的券张数，0 = 这场活动只送会员天数。用户在小程序上要看到
	// 「还送了 N 张券」，而这件事**此刻还没有发生**：券由券服务在收到事件之后发，可能晚几秒。
	// 所以它是「领到了什么」的一部分，不是「券已经在账上了」的保证。
	CouponCount int32 `json:"couponCount"`
	// AlreadyClaimed 为真表示这一次是重复扫码：结果与第一次完全相同，什么都没再发。
	//
	// 页面据此说「你已经领过了」，而不是再弹一次「领取成功」——两个都对，但只有一个是真的。
	AlreadyClaimed bool `json:"alreadyClaimed"`
}
