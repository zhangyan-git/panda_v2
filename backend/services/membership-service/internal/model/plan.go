// Package model 是会员库各张表的 Go 形状。
//
// 每张表一个文件，字段与列一一对应，`db` tag 就是列名。**没有转换逻辑、没有派生字段**：
// 一旦这里出现「如果 X 就填 Y」，那条规则就同时活在两个地方（这里和 SQL 里），而它们
// 迟早会有一次不一样。
//
// 唯一一处例外是下面那几个枚举常量：它们是**取值表**，不是派生规则——库上的 CHECK 用的
// 也是这几个字面量，两边必须逐字一致，所以它们只在这里写一遍。
package model

import "time"

// 套餐状态。与 migrations/membership 里 membership_plans.status 的 CHECK 逐字一致。
//
// 只有 active 的套餐能被购买。draft 是还没配完，disabled 是下架——**两者都不影响已经
// 买出去的会员**，因为 memberships 上存了成交快照（见 model.Membership 那一段）。
const (
	PlanStatusDraft    = "draft"
	PlanStatusActive   = "active"
	PlanStatusDisabled = "disabled"
)

// 会员价的两条来路。与套餐和会员两张表 CHECK 里的取值逐字一致。
//
// 这一列是**下单时 order-service 走哪个分支的依据**，不能靠「是不是订阅制」去推：年度
// 会员与连续包月都会出现在同一个套餐列表里，而它们判定会员价的方式完全不同。
const (
	// MemberPriceModeAuto —— 会员本人自动享会员价（年度会员）。下单时看 memberships 在不在
	// 有效期内就够了。
	MemberPriceModeAuto = "auto"
	// MemberPriceModeCoupon —— 会员本人**不**自动享会员价，靠会员价体验券（连续包月）。
	// 下单时看的是用户有没有一张可用的券，那件事归 coupon-service。
	MemberPriceModeCoupon = "coupon"
)

// 每期时长的单位。与两张表 CHECK 里的取值逐字一致。
//
// 存「月 / 年 + 个数」而不是天数：续期是**日历加法**（1 月 31 日开通的包月，下一期是
// 2 月 28 日），按天加会随期数漂。老系统同一条口径（panda_serve subscription_service.go
// 的 AddDate）。
const (
	PeriodMonth = "month"
	PeriodYear  = "year"
)

// Plan 是会员套餐：卖了什么。
//
// 三个「会员价从哪来」的列与 memberships 上那三个同名快照列是一对：这里是**当前配置**，
// 那里是**成交当时的配置**。套餐改名、改价、改权益、下架都不能改变已经卖出去的会员，
// 所以后者不是冗余。
type Plan struct {
	ID   string `db:"id"`
	Code string `db:"code"`
	Name string `db:"name"`
	// Description 与 Benefits 都是**展示用**的：Benefits 是 JSON 数组的文案列表
	// （原型：「每月 20 次咖啡享会员价」），真正的权益判定看 Period／PeriodCount／
	// AutoRenew／MemberPriceMode，不解析这段文案。
	Description string `db:"description"`
	Benefits    []byte `db:"benefits"`
	// PriceCents 单位是分，与仓库约定一致。
	PriceCents int64 `db:"price_cents"`
	// Period / PeriodCount 是每期时长（连续包月 = month/1，年度会员 = year/1）。
	Period      string `db:"period"`
	PeriodCount int32  `db:"period_count"`
	// AutoRenew 为真表示这个套餐要签微信委托代扣、到期自动续费。
	AutoRenew bool `db:"auto_renew"`
	// WechatPlanID 是微信商户平台的签约模板 ID（后台表单里那串 214488），签约时原样透传给
	// payment-service。AutoRenew 的套餐必有值（库上的 CHECK 钉着），年度会员为空。
	WechatPlanID string `db:"wechat_plan_id"`
	// MemberPriceMode 决定会员价怎么来，见上面那组常量。
	MemberPriceMode string `db:"member_price_mode"`
	// MemberPriceCouponTemplateID 是 coupon 模式发券用的券模板 ID，**仅作跨库值引用**
	// （coupon-service 的 coupon_templates）。券张数、面额、适用范围、有效期全在模板上，
	// 本库不复制。
	MemberPriceCouponTemplateID *string `db:"member_price_coupon_template_id"`
	// MemberPriceCouponsPerPeriod 是 coupon 模式每期发放几张。老系统写死 20
	// （panda_serve subscription_service.go:1242），这里做成套餐可配：改张数不该发一版服务。
	MemberPriceCouponsPerPeriod *int32    `db:"member_price_coupons_per_period"`
	SortOrder                   int32     `db:"sort_order"`
	Status                      string    `db:"status"`
	LegacyID                    *string   `db:"legacy_id"`
	CreatedBy                   *string   `db:"created_by"`
	CreatedAt                   time.Time `db:"created_at"`
	UpdatedAt                   time.Time `db:"updated_at"`
}

// IsActive 表示这个套餐当前可售。
//
// 抽成方法而不是在各处写 `plan.Status == model.PlanStatusActive`：可售的判据只有一条，
// 而它出现在列表过滤、下单校验、开通校验三处——写三遍的话，哪天「停售但还可以续费」
// 这类规则进来，会有一处漏改。
func (p *Plan) IsActive() bool { return p.Status == PlanStatusActive }
