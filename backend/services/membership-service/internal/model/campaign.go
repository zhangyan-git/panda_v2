package model

import "time"

// 店铺码会员活动的状态。与 migrations/membership 里 membership_campaigns.status 的
// CHECK 逐字一致（老系统三个取值照抄）。
//
// draft 是新建活动的初值（老后台也这样建），**要能被扫得再调一次启停接口**。disabled 是下架：
// 已经领过的人不受影响——领取那一刻已经把会员和归属落进 memberships 了，活动下架改不了既成事实。
const (
	CampaignStatusDraft    = "draft"
	CampaignStatusEnabled  = "enabled"
	CampaignStatusDisabled = "disabled"
)

// Campaign 是一张店铺码会员活动：扫码送 gift_days 天 plan 的会员，归属门店记成 store_id。
//
// 它是**配置**：改了它不影响已经领过的人——领取记录（CampaignClaim）上存着那一刻的门店、
// 套餐与天数快照。
type Campaign struct {
	ID string `db:"id"`
	// Name 是给运营看的名字（后台列表、审计里用），不出现在小程序码里。
	Name string `db:"name"`
	// Scene 是小程序码带的 scene 参数，扫码进来靠它找回这场活动。全库唯一（库上一条唯一索引，
	// 撞了就没法判断手里的码是哪一场）。格式由 service 校验，与老系统同一个正则。
	Scene string `db:"scene"`
	// StoreID 是活动门店，也是领到的会员的归属门店。跨库值引用（商户库的 stores），无外键。
	StoreID string `db:"store_id"`
	// PlanID 是送的是哪个套餐的会员。本库外键（REFERENCES membership_plans）。
	PlanID string `db:"plan_id"`
	// GiftDays 是送多少天。与套餐的 period/period_count 无关：这是一次赠送，按天数算。
	GiftDays int32     `db:"gift_days"`
	StartAt  time.Time `db:"start_at"`
	EndAt    time.Time `db:"end_at"`
	Status   string    `db:"status"`
	// CouponTemplateID / CouponCount 是这场活动每次领取送的券（模板是券库的值引用，无外键）。
	//
	// 用 *T 而不是「空串 / 0 表示没有」：NULL 在这里是一个**说得清的状态**——「这场活动不送券」
	// ——而 0 张券是一个配不出来的值（库上 CHECK 要求 > 0）。可空的外键式引用一律这样写，与
	// 同库的 membership_plans.member_price_coupon_template_id 逐字同一写法。
	//
	// 两列**同生共死**（库上那条 CHECK）：只有模板没有张数等于「该发券」静默变成「什么都不发」。
	CouponTemplateID *string `db:"coupon_template_id"`
	CouponCount      *int32  `db:"coupon_count"`
	// QRCodeURL 今天恒为空：生成小程序码要走微信的 wxacode.getUnlimited，而 appid/secret 与
	// access_token 缓存都在 user-service，本服务没有任何微信配置。
	QRCodeURL         string     `db:"qr_code_url"`
	QRCodeGeneratedAt *time.Time `db:"qr_code_generated_at"`
	// CreatedBy / UpdatedBy 是身份库的 admin_users.id（跨库值引用，无外键）。
	CreatedBy *string   `db:"created_by"`
	UpdatedBy *string   `db:"updated_by"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// IsEnabled 表示这场活动现在是开着的。
//
// 只有开关、不含时间窗口：两者是**分开**的两条判据，因为页面上要显示的东西不同
// （「已停用」与「已过期」对运营是两件事）。
func (c *Campaign) IsEnabled() bool { return c.Status == CampaignStatusEnabled }

// IsClaimable 表示这一刻扫码能不能领到。
//
// 三条一起看：开着、且落在 [start_at, end_at) 里。时间窗口是**闭开**区间——与全仓其它有效期
// 一致（end_at 那一刻就已经不能领了）。判据写成方法而不是散在 service 里：领取校验与后台
// 详情页的「当前可领吗」是同一句话，两处各写一遍迟早有一处漏掉 end_at 的比较。
func (c *Campaign) IsClaimable(at time.Time) bool {
	return c.IsEnabled() && !at.Before(c.StartAt) && at.Before(c.EndAt)
}

// CampaignClaim 是一条领取记录：某人某时扫了某场活动，领到了什么。
//
// **行的存在就是「发放完成」**：V2 的领取是一次事务内的幂等发放，失败的那次整体回滚、不留行
// （老系统那五个 pending_sign/signed/granting/completed/failed 是签约链路的产物，这里没有签约）。
type CampaignClaim struct {
	ID         string `db:"id"`
	CampaignID string `db:"campaign_id"`
	UserID     string `db:"user_id"`
	// StoreID / PlanID / GiftDays 是发放那一刻的快照：活动后来改了门店或天数，这一次不受影响。
	StoreID  string `db:"store_id"`
	PlanID   string `db:"plan_id"`
	GiftDays int32  `db:"gift_days"`
	// CouponTemplateID / CouponCount 是这次领取**承诺**发的券在发放那一刻的快照：活动后来改了
	// 券的配置，这一次不受影响（与上面那几列同一条规矩）。
	//
	// 它是承诺、不是结果：实际发了几张在券库（user_coupons.campaign_claim_id 指着这条记录）。
	// 本库不重复记一份券的账——券的失败重试归券服务，这里既不知道也不该知道。
	CouponTemplateID *string `db:"coupon_template_id"`
	CouponCount      *int32  `db:"coupon_count"`
	// MembershipID 是这次领取派生出的那条会员（本库外键）。一个人一条会员，所以它同时也答
	// 「这个人现在是不是会员」。
	MembershipID string `db:"membership_id"`
	// MembershipExpireAt 是发放那一刻算出来的到期时刻。会员行上的 expire_at 会随后台调整、
	// 续费而变，这里记的是「这次领取当时送到哪天」。
	MembershipExpireAt time.Time `db:"membership_expire_at"`
	CreatedAt          time.Time `db:"created_at"`
}
