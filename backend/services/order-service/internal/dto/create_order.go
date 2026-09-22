package dto

import (
	"encoding/json"
	"time"
)

// CreateOrderRequest 是下单请求。
//
// **价格不由调用方说了算**，这一点是这份请求的形状的全部来由。
//
// 方案 5.8 要求下单时校验「设备状态、商品供应及选品上下文」，价格由服务端从商品目录现查。
// 两条线上都做到了：
//
//   - 会员行只收 membershipPlanId。价格、时长、名称与套餐快照由服务端向 membership-service
//     现取（service.applyMembershipPlan）。
//   - 饮品行只收 itemId。名称、图片、原价与会员价由服务端向 coffee-machine-service 现查
//     （service.applyDrinkPricing），会员资格向 membership-service 现问。
//
// 两处共用的那条规矩是：**请求里那几个价格字段对这两种行一概不参与计算**。不是「校验它对
// 不对」，而是它压根不进算钱那一步——这样客户端算错了也不会把用户带到一个与我们实际收的钱
// 不一样的价钱上。
//
// 还剩下两处调用方定价，都是有意留的：
//
//   - **加购行**。加购品今天没有商品目录，服务端没有第二个来源可查，所以它的价格只能是
//     调用方给的。等它有了目录，照上面两条的形状再来一次。
//   - **couponDiscountAmount**。「这张券抵多少钱」的权威在 coupon-service，该由券自己算；
//     那是券域的一刀，不在这里。**这是这份请求里仅存的、能影响应付额的调用方输入。**
type CreateOrderRequest struct {
	// miniapp=小程序直接下单，screen_qr=咖啡机屏幕选品后扫码下单。
	Source string `json:"source"`
	// 点位与咖啡机。有饮品行时 deviceId 必填：设备是「这台机器属于哪个点位」的唯一来源，
	// storeId 传了就必须与设备上的点位一致，不一致直接拒（见 service.createOrder）。
	StoreID    *string `json:"storeId"`
	StoreName  string  `json:"storeName"`
	DeviceID   *string `json:"deviceId"`
	SceneToken string  `json:"sceneToken"`
	// 下单时的**会员资格**（按会员价卖饮品时那份「他当时是不是会员、什么等级」的快照），
	// 原样存下来用于还原当时的会员价。结构归 membership-service。
	//
	// 与「这一单是不是在买会员」无关：买会员的人还没有会员（这一单正是去给他开会员的），
	// 所以它不是必填，也不与会员行配对。真要用它定价时，它该由服务端从会员域取，而不是收
	// 客户端填的——这条口径与会员行完全一致（见 CreateOrderLine.MembershipPlanID）。
	MembershipID       *string         `json:"membershipId"`
	MembershipSnapshot json.RawMessage `json:"membershipSnapshot"`
	// 这一单承诺发多少张福卡（基础赠送 + 各加购活动加赠）。规则本身（方案 3.1）仍是待定项，
	// 没有任何服务拥有它，所以这里不重算，只如实存下调用方算好的承诺数。
	FortuneCardsExpected int               `json:"fortuneCardsExpected"`
	FortuneCardSnapshot  json.RawMessage   `json:"fortuneCardSnapshot"`
	Lines                []CreateOrderLine `json:"lines"`
	Remark               string            `json:"remark"`
}

// CreateOrderLine 是下单请求里的一行商品。
//
// 没有 discountAmount / payableAmount：这两个值由服务端从 PriceDiscountAmount 与
// CouponDiscountAmount 算出来（discount = price + coupon，payable = 目录单价 × 数量 −
// 优惠），收进来就等于给了同一笔钱两个来源。
//
// 也没有 pickupCode / deviceOrderNo / fulfillmentTaskNo：取杯号由服务端在支付成功时生成
// （客户端挑号等于让用户自己决定屏幕上显示什么）；厂商单号与履约任务号属于履约侧，
// 不从小程序收。
type CreateOrderLine struct {
	// drink / addon / membership。
	LineType string `json:"lineType"`
	// ItemID 是这一行的商品值引用：饮品行是 drinks.id（设备上的一杯），会员行是
	// membership_plans.id。
	//
	// 饮品行**只收这一个值**：名称、图片、原价与会员价都由服务端拿它去现查（见
	// service.applyDrinkPricing）。加购行今天没有目录可查，所以它的 itemId 只是存档。
	ItemID *string `json:"itemId"`
	// ItemCode / ItemName / ItemImage 是商品快照，**只对加购行有效**：饮品行与会员行
	// 传了会被忽略，那两行的这三格由服务端从各自的目录填。
	ItemCode  string `json:"itemCode"`
	ItemName  string `json:"itemName"`
	ItemImage string `json:"itemImage"`
	Quantity  int    `json:"quantity"`
	// 目录价与成交单价，单位分。**只对加购行有效**：饮品行传了会被忽略（服务端按 itemId
	// 现查目录价与会员价），会员行传了也会被忽略（价格来自套餐）。
	OriginalUnitPrice int64 `json:"originalUnitPrice"`
	UnitPrice         int64 `json:"unitPrice"`
	// PriceDiscountAmount 是「标价与成交价之间的差」，只对加购行有效：饮品行那一格由服务端
	// 算成「目录价 − 成交价」（会员价优惠就记在这里），会员行恒为 0。
	PriceDiscountAmount int64 `json:"priceDiscountAmount"`
	// 券只优惠饮品：加购行与会员行带 couponId 会被拒（DB 的
	// order_lines_coupon_only_on_drink 也挡这一条）。
	CouponID *string `json:"couponId"`
	// CouponDiscountAmount 是这张券抵掉的钱（分）。**饮品行上它是调用方给的**——「这张券抵
	// 多少」的权威在 coupon-service，该由券自己算，那是券域的一刀。
	//
	// 所以它是这份请求里仅存的、能影响应付额的调用方输入。放它在这里不代表它是合理的，
	// 只代表它还没轮到。
	CouponDiscountAmount int64 `json:"couponDiscountAmount"`
	// 饮品规格与固定选品快照，原样存档不参与查询。加购行与会员行留空。
	Specs             json.RawMessage `json:"specs"`
	SelectionSnapshot json.RawMessage `json:"selectionSnapshot"`
	// 加购活动：加购行必须有 campaignId，饮品行与会员行不能有。
	CampaignID       *string         `json:"campaignId"`
	CampaignSnapshot json.RawMessage `json:"campaignSnapshot"`
	// MembershipPlanID 是会员行买的那个套餐（membership_plans.id），只有会员行有。
	//
	// **只收这一个值**：价格、时长、会员价路径都由服务端拿它去会员域现取（见
	// service.createOrder），请求里给什么都不作数。曾经这里收的是整份套餐快照——那等于
	// 让客户端定价：一条 originalUnitPrice=1、快照写着年卡的请求能花一分钱开一年会员。
	MembershipPlanID *string `json:"membershipPlanId"`
	Remark           string  `json:"remark"`
}

// MembershipPlanSnapshot 是会员行上那份套餐快照的形状。
//
// 它有**两个用处，一份内容**：写进 order_lines.membership_plan_snapshot，以及付款成功之后
// 作为 order.paid 的 membership 段发给会员域。会员域据此开通会员、发会员价券——所以这里的
// 每一个 json tag 都与 membership-service 的 dto.OrderMembershipSnapshot 逐字对应
// （那边开着 DisallowUnknownFields：多发一个字段整条消息进死信）。
//
// 它是**服务端**从会员域取回来的一份拷贝（见 service.createOrder），不是客户端给的：
// 快照同时决定用户付多少钱、买到多长、会员价怎么来，这三件事只有会员域说得清。而它一旦
// 落进订单行就不再变——套餐改价、改时长、下架都不影响已经卖出去的那一单。
//
// 字段一个都不能少，所以没有一个 omitempty：coupon 模式下会员域要读模板 ID 与张数，
// 缺一个它就没法发券。
type MembershipPlanSnapshot struct {
	// PlanID 是套餐的值引用（订单行不建外键，会员库里才有）。PlanCode / PlanName 是下单
	// 那一刻的副本，套餐改名不影响历史订单。
	PlanID   string `json:"planId"`
	PlanCode string `json:"planCode"`
	PlanName string `json:"planName"`
	// PriceCents 是这一单实付的套餐价（分）。它同时是会员行上 original_unit_price 的来源
	// ——两个值出自同一个地方，不会对不上。
	PriceCents int64 `json:"priceCents"`
	// Period / PeriodCount 是这一单买到的时长（month/1 或 year/1）。续期按日历加。
	Period      string `json:"period"`
	PeriodCount int32  `json:"periodCount"`
	// AutoRenew 是套餐层面的「这个产品要签代扣」，不是用户开关（那个在会员域自己身上）。
	AutoRenew bool `json:"autoRenew"`
	// MemberPriceMode 与下面两列是会员价的来路快照：auto 自动享，coupon 靠发券，
	// 发几张、用哪个模板看后两列（只有 coupon 模式有值）。
	MemberPriceMode             string `json:"memberPriceMode"`
	MemberPriceCouponTemplateID string `json:"memberPriceCouponTemplateId"`
	MemberPriceCouponsPerPeriod int32  `json:"memberPriceCouponsPerPeriod"`
}

// CreateOrderResponse 是下单成功的响应体，同时也是写进幂等表的响应快照：
// 同一个 Idempotency-Key 重放时原样回放它，不重新执行一遍副作用。
type CreateOrderResponse struct {
	OrderID string `json:"orderId"`
	OrderNo string `json:"orderNo"`
	// 主状态机的初始状态（pending_payment）与履约汇总的初始状态（有饮品行是 pending，
	// 纯会员单是 none）。
	Status            string `json:"status"`
	FulfillmentStatus string `json:"fulfillmentStatus"`
	// 金额，单位分。payableAmount = originalAmount − discountAmount，由服务端算出。
	OriginalAmount int64 `json:"originalAmount"`
	DiscountAmount int64 `json:"discountAmount"`
	PayableAmount  int64 `json:"payableAmount"`
	// 待支付超时时间，客户端据此显示倒计时；到期由关单扫描置为 expired。
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}
