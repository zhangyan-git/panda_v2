package dto

import "time"

// OrderQuery 是订单列表的筛选条件，后台端与小程序端共用。
//
// 小程序端由服务端强制填上 UserID（来自令牌），后台端才允许按用户筛——这不是一个
// 可选项：漏填就等于把别人订单的读能力交给调用方，所以 UserID 的赋值只在
// service.listOrders 里做，controller 传进来的值得不到直接就生效。
type OrderQuery struct {
	UserID string
	// OrderNo 是精确匹配（客服/对账都是拿着完整单号来找）；Keyword 才是模糊。
	OrderNo string
	Status  string
	Source  string
	// 只筛「有某类行」或「没有某类行」的订单（零值 = 不筛）。订单类型不存冗余列，它是派生
	// 出来的（见迁移 001），所以每一个都是一个 EXISTS 子查询而不是一个等值条件。
	// 后台的「咖啡订单 / 幸运杯套订单 / 会员订单」三个列表各钉其中一项。
	HasDrink      *bool
	HasAddon      *bool
	HasMembership *bool
	StoreID       string
	DeviceID      string
	// 创建时间的闭开区间 [CreatedFrom, CreatedTo)。
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	Page        int
	PageSize    int
}

// OrderSummary 是订单列表里的一行。列表不带行明细与出资分摊：那是详情的事，
// 列表里带上去会让「一页 20 单」变成一次上百行的 join。
type OrderSummary struct {
	ID                string     `json:"id"`
	OrderNo           string     `json:"orderNo"`
	UserID            string     `json:"userId"`
	Source            string     `json:"source"`
	Status            string     `json:"status"`
	FulfillmentStatus string     `json:"fulfillmentStatus"`
	StoreID           *string    `json:"storeId"`
	StoreName         string     `json:"storeName"`
	DeviceID          *string    `json:"deviceId"`
	DeviceNo          string     `json:"deviceNo"`
	OriginalAmount    int64      `json:"originalAmount"`
	DiscountAmount    int64      `json:"discountAmount"`
	PayableAmount     int64      `json:"payableAmount"`
	PaidAmount        int64      `json:"paidAmount"`
	RefundedAmount    int64      `json:"refundedAmount"`
	PaymentMethod     string     `json:"paymentMethod"`
	PaymentNo         string     `json:"paymentNo"`
	PaidAt            *time.Time `json:"paidAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
	CancelledAt       *time.Time `json:"cancelledAt"`
	// 取消原因。后台取消会写「后台取消」，用户取消写「用户取消」，超时关单写「超时未支付」。
	CancellationReason string     `json:"cancellationReason"`
	ExpiresAt          *time.Time `json:"expiresAt"`
	Remark             string     `json:"remark"`
	// 只读的派生字段：这一单有没有饮品行 / 加购行 / 会员行。后台按这三样把订单分成
	// 「咖啡订单 / 幸运杯套订单 / 会员订单」三个列表，而一张合并单会同时出现在几个列表里，
	// 所以列表里必须能看出「这单还含什么」——这三个布尔值就是给那一列用的。
	HasDrinkLine         bool `json:"hasDrinkLine"`
	HasAddonLine         bool `json:"hasAddonLine"`
	HasMembershipLine    bool `json:"hasMembershipLine"`
	FortuneCardsExpected int  `json:"fortuneCardsExpected"`
	// 取杯号：这一单饮品行的那个短号（同一单的饮品行共用一个），没付成功或没有饮品行时为 nil。
	// 列表里带上它，是因为客服最常被问的就是「我的号是多少」——空着手让人再点进详情不好用。
	PickupCode *string   `json:"pickupCode"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// OrderDetail 是订单详情：主表 + 行明细 + 出资分摊 + 状态流水。
type OrderDetail struct {
	OrderSummary
	SceneToken          string                 `json:"sceneToken"`
	MembershipID        *string                `json:"membershipId"`
	MembershipSnapshot  any                    `json:"membershipSnapshot"`
	FortuneCardSnapshot any                    `json:"fortuneCardSnapshot"`
	Lines               []OrderLineView        `json:"lines"`
	PaymentLines        []OrderPaymentLineView `json:"paymentLines"`
	Transitions         []OrderTransitionView  `json:"transitions"`
	// 这一单的售后申请，新的在前。用户在订单详情里看自己的退款进度，客服也从这里进来，
	// 所以不再单开一个用户侧列表接口。
	AfterSales []AfterSaleView `json:"afterSales"`
}

// OrderLineView 是订单行对外的形状。
//
// PickupCode 是取杯号（屏幕上叫取杯码，同一个值），用户侧与后台都看得到：它本来就是取杯口
// 屏幕上大字显示的短号，不是凭据。曾经这里以「凭据」为由对后台端置 nil，那建立在一次凭空
// 的列拆分上（order/004 已合并回一列）。
type OrderLineView struct {
	ID                     string    `json:"id"`
	LineNo                 int       `json:"lineNo"`
	LineType               string    `json:"lineType"`
	ItemID                 *string   `json:"itemId"`
	ItemCode               string    `json:"itemCode"`
	ItemName               string    `json:"itemName"`
	ItemImage              string    `json:"itemImage"`
	Quantity               int       `json:"quantity"`
	OriginalUnitPrice      int64     `json:"originalUnitPrice"`
	UnitPrice              int64     `json:"unitPrice"`
	PriceDiscountAmount    int64     `json:"priceDiscountAmount"`
	DiscountAmount         int64     `json:"discountAmount"`
	PayableAmount          int64     `json:"payableAmount"`
	CouponID               *string   `json:"couponId"`
	CouponDiscountAmount   int64     `json:"couponDiscountAmount"`
	Specs                  any       `json:"specs"`
	SelectionSnapshot      any       `json:"selectionSnapshot"`
	CampaignID             *string   `json:"campaignId"`
	CampaignSnapshot       any       `json:"campaignSnapshot"`
	MembershipPlanSnapshot any       `json:"membershipPlanSnapshot"`
	DeviceID               *string   `json:"deviceId"`
	DeviceOrderNo          string    `json:"deviceOrderNo"`
	FulfillmentTaskNo      string    `json:"fulfillmentTaskNo"`
	PickupCode             *string   `json:"pickupCode"`
	Remark                 string    `json:"remark"`
	CreatedAt              time.Time `json:"createdAt"`
	UpdatedAt              time.Time `json:"updatedAt"`
}

// OrderPaymentLineView 是一次出资分摊对外的形状。
//
// 不带 ProviderTransactionID：渠道流水号是对账用的凭据，后台要看就去 payment-service 查，
// 这里多存一份只会多一个泄漏面。
type OrderPaymentLineView struct {
	ID             string     `json:"id"`
	LineNo         int        `json:"lineNo"`
	LineType       string     `json:"lineType"`
	Amount         int64      `json:"amount"`
	Status         string     `json:"status"`
	PaymentNo      string     `json:"paymentNo"`
	FailureCode    string     `json:"failureCode"`
	AccountEntryID *string    `json:"accountEntryId"`
	SucceededAt    *time.Time `json:"succeededAt"`
	ReversedAt     *time.Time `json:"reversedAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// OrderTransitionView 是一条状态流水。
type OrderTransitionView struct {
	ID            string    `json:"id"`
	AggregateType string    `json:"aggregateType"`
	AggregateID   string    `json:"aggregateId"`
	FromStatus    string    `json:"fromStatus"`
	ToStatus      string    `json:"toStatus"`
	Reason        string    `json:"reason"`
	ActorType     string    `json:"actorType"`
	ActorID       *string   `json:"actorId"`
	CreatedAt     time.Time `json:"createdAt"`
}
