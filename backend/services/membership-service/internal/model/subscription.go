package model

import "time"

// 订阅状态。与 migrations/membership 里 membership_subscriptions.status 的 CHECK 逐字一致。
//
// 生命周期：pending_sign →（签约成功）active。active ⇄ suspended（连续扣款失败到阈值停扣；
// 停扣之后某一期又扣成了就回到 active）。active / suspended →（有一方说不续了）cancelled，
// 或（会员到期不再续）expired。
//
// 注意 cancelled 与 expired 的区别不是「谁发起的」而是「还能不能复活」：cancelled 是
// 有一方明确说不续了，expired 是正常走完。
//
// **suspended 是「停扣」，不是「过期」，也不是解约**：协议还挂在渠道上，本服务只是不再发起新的
// 扣款（续费扫描只捞 active，见 repository.ListDueSubscriptions）。它与 cancelled 的差别正在
// 这里——suspended 保留了那份授权，所以它仍然占着 live 那个坑（见 IsLive），也仍然是一条能被
// 运营看见并处置的订阅。
const (
	SubscriptionStatusPendingSign = "pending_sign"
	SubscriptionStatusActive      = "active"
	SubscriptionStatusSuspended   = "suspended"
	SubscriptionStatusCancelled   = "cancelled"
	SubscriptionStatusExpired     = "expired"
)

// MaxConsecutiveChargeFailures 是连续失败到几次就停扣（订阅转 suspended）。
//
// 与 payment-service 的 model.ChargeMaxAttempts 是同一个数、也不是同一条判据：那一个是「这一期
// 还试不试」，这一个是「这个人还扣不扣」。今天两边都是 3，但它们分开的理由是它们会分开——渠道
// 那边的封顶是成本（一次出网一次手续费），这里的封顶是用户关系（连着三期扣不到钱，多半是卡注销
// 了或人不用了，再扣下去只是每天给客服添一条工单）。
//
// **停扣不是解约**：协议还挂在微信上，授权是用户给的，本服务不替用户撤回（见 ChangeSuspend 的
// 说明与 migrations/membership 里「变更记录」那一节）。
const MaxConsecutiveChargeFailures = 3

// Subscription 是一条连续包月的签约与扣款期次记录。
//
// 协议本体在 payment-service（payment_agreements），这里只记「这一期什么时候该扣、扣了没、
// 失败几次、什么时候解约」。方案 7.3 的注释反过来也成立：**扣多少、什么时候扣由本服务
// 决定**并传给 payment，所以 PriceCents、NextChargeAt 这两列必须在这里，不能去 payment 现查。
//
// 一个会员同时只能有一条活着的订阅（pending_sign/active/suspended 三种状态算活着，
// 见 membership_subscriptions_live_unique）；一份代扣协议也只能对应一条
// （membership_subscriptions_agreement_unique）。两条不变式都在库上钉着，因为它们的
// 反面都是「扣两次钱」。
type Subscription struct {
	ID           string `db:"id"`
	MembershipID string `db:"membership_id"`
	UserID       string `db:"user_id"`
	PlanID       string `db:"plan_id"`
	Status       string `db:"status"`
	// AgreementID 是 payment-service 的 payment_agreements.id，仅作值引用：
	// 本库不解释它的状态，也不去校验它对不对。
	AgreementID *string `db:"agreement_id"`
	// ContractCode 是商户协议号，签约时由 payment 返回并写回。对账时要拿它去渠道查，
	// 所以本库留一份副本——不是「副本可以不同步」，是这份副本从来只读不写。
	ContractCode string `db:"contract_code"`
	// PriceCents 是签约时的每期扣款金额快照。代扣金额是签约时约定死的：套餐后来调价，
	// 已签约用户仍按这个数扣。与 memberships 上那组快照是同一条理由。
	PriceCents int64 `db:"price_cents"`
	// WechatPlanID 是签约时的签约模板 ID 快照，取值同套餐 wechat_plan_id。签约后套餐换了
	// 模板，已签的协议仍然挂在旧模板上，得拿旧的那个去查协议、去扣款。
	WechatPlanID string `db:"wechat_plan_id"`
	// Period / PeriodCount 是每期叠加的时长（连续包月 = month/1），与套餐同义。
	Period      string `db:"period"`
	PeriodCount int32  `db:"period_count"`
	// NextChargeAt 是下一次应扣款时间。续费扫描按它取任务，所以 active 必须有值
	// （CHECK (status <> 'active' OR next_charge_at IS NOT NULL) 钉着）：一条 active
	// 却没有下次扣款时间的订阅永远不会被扫到，也不会有人发现。
	NextChargeAt *time.Time `db:"next_charge_at"`
	LastChargeAt *time.Time `db:"last_charge_at"`
	ChargeCount  int32      `db:"charge_count"`
	// FailedCount 是累计失败次数（统计用），ConsecutiveFailedCount 是连续失败次数
	// （触发暂停用）。一次成功两者都清零。分开是因为「历史失败过 5 次但已经好了」
	// 和「连续失败 5 次还在坏着」要问的是不同的问题。
	FailedCount            int32      `db:"failed_count"`
	ConsecutiveFailedCount int32      `db:"consecutive_failed_count"`
	SuspendedAt            *time.Time `db:"suspended_at"`
	CancelAt               *time.Time `db:"cancel_at"`
	CancelReason           string     `db:"cancel_reason"`
	// CancelledBy 是解约发起人：用户自己在小程序点的，或者后台运营/风控。system 触发的
	// 解约（比如协议被渠道作废）留空。
	CancelledBy *string `db:"cancelled_by"`

	// 下面三个 id 记的是「这一次签约是从哪儿来的」，也就是后台页面上那两列
	// （签约场景、首月支付）的原料。三列都可空，空串表示「不是从这条路来的」。
	//
	// **今天没有任何代码写它们**：写入方是小程序端签约，而小程序端还没接（见
	// migrations/membership 与 repository/subscription.go 的包说明）。页面不会因此显示
	// 错东西——三个都为空正好推出「会员中心支付并签约」，那是这一档的默认答案。
	OrderID         string    `db:"order_id"`
	CoffeeOrderID   string    `db:"coffee_order_id"`
	CampaignClaimID string    `db:"campaign_claim_id"`
	CreatedAt       time.Time `db:"created_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}

// IsLive 表示这条订阅还活着——占着 membership_subscriptions_live_unique 那个坑。
//
// pending_sign 也算活着：否则用户连点两次「开通连续包月」就会签出两份协议、扣两次钱。
// 这个方法存在的意义就是让人别在别处把 live 写成 `== active`。
func (s *Subscription) IsLive() bool {
	switch s.Status {
	case SubscriptionStatusPendingSign, SubscriptionStatusActive, SubscriptionStatusSuspended:
		return true
	default:
		return false
	}
}

// IsDue 表示这一期已经到点该扣了。
//
// 只有 active 且 NextChargeAt 已过的才该扣。三种「到点了也不扣」各有各的理由：pending_sign 还没
// 签约成功；cancelled / expired 已经结束了；**suspended 是被停掉的**——连续失败到阈值之后本服务
// 主动不再发起（见 MaxConsecutiveChargeFailures），所以「到点了」对它不构成理由。
//
// 一期的重试**不靠 suspended**：这一期没扣成时 NextChargeAt 不动、状态还是 active，下一轮扫描
// 算出的还是同一个期次（见 ChargePeriod），支付侧按 (协议, 期次) 幂等地接着试。now 作参数的理由
// 同 Membership.IsUsable。
func (s *Subscription) IsDue(now time.Time) bool {
	return s.Status == SubscriptionStatusActive &&
		s.NextChargeAt != nil && !s.NextChargeAt.After(now)
}

// ChargePeriod 把「下一次该扣的时间」算成那一期的期次（东八区下的 yyyyMMdd，如 20261001）。
//
// 它是**代扣那一期的幂等键**：payment-service 上 `UNIQUE (agreement_id, biz_period)`，所以
// 一份协议的同一个期次只可能扣一次。
//
// # 为什么必须从 NextChargeAt 派生
//
// 因为**这一期扣成之前 NextChargeAt 不推进**（推进发生在扣款成功那条事件里，见
// service/event.go 对 charge_succeeded 的处置）。一期失败重试三次，三次算出来的是同一个
// 期次、落在支付库同一行上（那边记的是 attempt_count 加一）。换成一个每天都在变的输入
// （比如「这一轮扫描是什么时候跑的」），重试就成了新的一期——那不是重试，是重复扣款。
//
// # 为什么按东八区取日期而不是 UTC
//
// NextChargeAt 是 timestamptz，pgx 读出来带 UTC。北京时间 10 月 1 日 0 点整在 UTC 下是
// 9 月 30 日 16 点，按 UTC 取日期会把一期会员的期次写成上个月的那一天。期次本身只是个
// 标识，但它同时是客服与财务拿去和微信账单对的那个号——差一天就没人对得上。
//
// 用 FixedZone 而不是 LoadLocation("Asia/Shanghai")：中国从 1991 年起没有夏令时，固定
// +08:00 就是准的；而 LoadLocation 依赖运行环境里有 tzdata，缺了它**不报错、悄悄退回 UTC**
// ——正好落回上面那个错法，而且只在容器里错、在本机不错。
func ChargePeriod(nextChargeAt time.Time) string {
	return nextChargeAt.In(chargeZone).Format("20060102")
}

// chargeZone 是代扣期次用的时区，见 ChargePeriod。
var chargeZone = time.FixedZone("CST", 8*3600)
