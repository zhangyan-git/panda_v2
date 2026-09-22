// Package dto 是会员服务的对外数据形状：HTTP 的请求 / 响应，以及发到 MQ 上的事件体。
//
// 分成一个包而不是散在 controller 里，理由与其余几个服务的 dto 相同：这些结构上的 json tag
// **不是我们自己说了算的**——admin-web 的会员页与小程序会员中心的字段名是逐字照抄它们的，
// 改一个 tag 要三处同批改（tag、前端字段、前端标签表）。放在一处，是为了让那份契约有一个
// 可以被指着的东西。
package dto

import "time"

// MaxPageSize 是列表接口允许的最大 pageSize，与 platform/api.MaxPageSize 取值一致。
//
// admin-web 的分页组件按 FULL_PAGE_PARAMS 一次要 200 条（下拉、跨页全选用它），接口的取值比
// 它小的话，前端会拿到一页被悄悄截断的数据——看起来像「就是这么多」。coffee-machine-service
// 吃过这个亏（那里原来是 100），所以这个常量在每一个新服务里都从头写一遍。
const MaxPageSize = 200

// DefaultPageSize 是没给 pageSize 时的每页条数。
const DefaultPageSize = 20

// PlanResponse 是一个会员套餐的对外形状。
//
// 它是**套餐定义**，不是某个人的会员：这里的 priceCents / period 是「现在买要多少钱、买多久」。
// 已经买过的人的条款在 memberships 的快照列上，改这个结构不会动到他们。
type PlanResponse struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	// Description 与 Benefits 只用于展示。Benefits 是那句「每月 20 次咖啡享会员价」的列表，
	// 存储上是 JSONB 数组。
	Description string   `json:"description"`
	Benefits    []string `json:"benefits"`
	PriceCents  int64    `json:"priceCents"`
	// Period 是每期时长的单位（month / year），PeriodCount 是个数。两个字段一起读才对
	// ——「month/1」是连续包月，「year/1」是年度会员。前端要显示「1 个月」也是拼这两个，
	// 不是一个预先拼好的中文串：拼串会把「几期」这件事挡在后端，而它对账时是要用的。
	Period      string `json:"period"`
	PeriodCount int32  `json:"periodCount"`
	// AutoRenew 为真表示这个套餐要签微信委托代扣、到期自动续费。
	AutoRenew bool `json:"autoRenew"`
	// WechatPlanID 是微信商户平台的签约模板 ID。后台表单里要填，所以它必须回给前端；
	// 但它**不透传给任何 C 端接口**（见 miniapp 那份套餐形状）。
	WechatPlanID string `json:"wechatPlanId"`
	// MemberPriceMode 决定会员价怎么来：auto=会员本人自动享（年度会员），
	// coupon=靠会员价体验券（连续包月）。后台那一列就是照它显示的。
	MemberPriceMode string `json:"memberPriceMode"`
	// 下面两列只在 coupon 模式有值（库上的 CHECK 钉着）。后台表单据此显示「发哪张券、每期几张」。
	MemberPriceCouponTemplateID string `json:"memberPriceCouponTemplateId"`
	MemberPriceCouponsPerPeriod int32  `json:"memberPriceCouponsPerPeriod"`
	SortOrder                   int32  `json:"sortOrder"`
	// draft / active / disabled。只有 active 能被购买。
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PlanRequest 是新建 / 修改套餐的请求体。
//
// 新建与修改共用同一个结构体、同一套校验：两者的差别只在「这条记录之前存在吗」，校验上分两套
// 只会让其中一套先腐烂（与 lottery-service 的 CampaignRequest 同一条理由）。
//
// **没有 Status**：套餐的上下架走单独的动作（PlanStatusRequest），不是「保存时顺手改一下」。
// 合在一起的话，运营改一个价格描述会把一个正在售的套餐顺手存成草稿——而那一刻可能正有人在
// 下单页上。同一个理由，**没有 Code**：编码不可修改（库上有触发器钉着），它只在新建时给。
type PlanRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Benefits    []string `json:"benefits"`
	PriceCents  int64    `json:"priceCents"`
	Period      string   `json:"period"`
	PeriodCount int32    `json:"periodCount"`
	AutoRenew   bool     `json:"autoRenew"`
	// WechatPlanID 只在 AutoRenew 为真时有意义，其余情况必须留空（见 service 的校验）。
	WechatPlanID string `json:"wechatPlanId"`
	// MemberPriceMode 取 auto / coupon，留空按 auto（与库上的 DEFAULT 一致）。
	MemberPriceMode string `json:"memberPriceMode"`
	// 下面两列与 MemberPriceMode 配对：coupon 必须都给，auto 必须都留空。
	MemberPriceCouponTemplateID string `json:"memberPriceCouponTemplateId"`
	MemberPriceCouponsPerPeriod int32  `json:"memberPriceCouponsPerPeriod"`
	SortOrder                   int32  `json:"sortOrder"`
}

// CreatePlanRequest 是新建套餐的请求体：在 PlanRequest 之上多一个 Code。
//
// 单独一个类型而不是给 PlanRequest 加一个「修改时忽略」的字段：那样的话 Code 会出现在修改
// 接口的文档里，而它改了会被库上的触发器拒绝——一个「写了就被拒」的字段不该出现在契约里。
type CreatePlanRequest struct {
	Code string `json:"code"`
	PlanRequest
}

// PlanStatusRequest 是上下架 / 转草稿的请求体。动作本身只有这一个字段。
//
// 单独一个接口而不是 PUT 套餐时顺带改：见 PlanRequest 的说明。
type PlanStatusRequest struct {
	Status string `json:"status"`
}
