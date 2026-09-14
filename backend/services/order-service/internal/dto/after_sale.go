package dto

import "time"

// ApplyAfterSaleRequest 是一次退款申请。
//
// 请求体里**没有金额**：退多少钱由服务端算（整单退=剩余可退，按行退=该行应付额）。
// 让调用方报价等于让它决定退多少钱，而它手上的价格是下单时的快照，不是账上的事实。
type ApplyAfterSaleRequest struct {
	// Scope 取值见 model.AfterSaleScope*：all=整单，drink/addon/membership=按行。
	Scope string `json:"scope"`
	// OrderLineID 按行退时必填，整单退时必须为空。DB 的 CHECK 也是这条等价关系
	// （order_after_sales_scope_matches_line），服务先挡一次是为了给出人话。
	OrderLineID *string `json:"orderLineId"`
	Reason      string  `json:"reason"`
	// Images 是凭证图片的 URL 列表，不是文件：上传链路（对象存储、签名、尺寸校验）
	// 不在本服务里，这里只收已经能访问到的 URL。
	Images []string `json:"images"`
}

// CancelAfterSaleRequest 是用户撤销自己的退款申请。
//
// 整个请求体都可以不带（前端只有一个「撤销申请」按钮）：缺省时服务端补一句「用户撤销退
// 款申请」写进状态流水，让那一行读得懂，而不是留一条 reason 为空、事后查不出原因的记录。
type CancelAfterSaleRequest struct {
	Reason string `json:"reason"`
}

// ReviewAfterSaleRequest 是后台的审核决定（通过还是驳回写在路径上）。
type ReviewAfterSaleRequest struct {
	// Remark 驳回必填：驳回是「不退」这个结论，不写理由的话用户拿到的只是一句
	// 「申请未通过」，客服第二天也说不清当时为什么拒。
	Remark string `json:"remark"`
	// FortuneCardUnusedConfirmed 是审核人对福卡规则的显式确认：已确认本单赠送的福卡
	// **没有参与过抽奖**。
	//
	// 为什么是人工勾一个布尔而不是服务端自己判：order 库只有「承诺发几张福卡」
	// （orders.fortune_cards_expected），福卡的发放与消耗在 account-service、抽奖与
	// 抽奖资格在 lottery-service，两个服务都还没建。既然判不了，就不能给「静默放行」
	// 的口子——凡承诺过福卡的订单，没带这个确认一律拒绝。
	//
	// 规则：订单送的福卡若已参与抽奖，整笔不可退（客服查到已抽奖时走驳回，理由写进
	// Remark）。lottery-service 落地后这里换成一次实时查询，字段语义不变。
	FortuneCardUnusedConfirmed bool `json:"fortuneCardUnusedConfirmed"`
}

// AfterSaleView 是售后单对外的形状——**唯一**的那个：申请书（POST 订单下的 after-sales）、
// 用户撤销、后台审核、以及订单详情里挂在 afterSales 上的列表，四处都是它。
//
// 一张售后单有两种长相（申请时回一份薄的、别处回一份全的）只会让前端多写一套分支，
// 而两套迟早对不上：申请那条路曾经就是这样，福卡承诺与行摘要在它那里是缺的。
//
// 后三个字段是只读派生：审核福卡规则要用「这单承诺过几张福卡」，客服要对账要看
// 「退的是哪一行」。它们不属于售后单（那张表上一个字都没存），所以在响应里明确分开。
type AfterSaleView struct {
	ID           string     `json:"id"`
	AfterSaleNo  string     `json:"afterSaleNo"`
	OrderID      string     `json:"orderId"`
	OrderNo      string     `json:"orderNo"`
	UserID       string     `json:"userId"`
	Type         string     `json:"type"`
	Scope        string     `json:"scope"`
	OrderLineID  *string    `json:"orderLineId"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	Images       any        `json:"images"`
	RefundAmount int64      `json:"refundAmount"`
	RefundNo     string     `json:"refundNo"`
	FailureCode  string     `json:"failureCode"`
	ReviewedBy   *string    `json:"reviewedBy"`
	ReviewedAt   *time.Time `json:"reviewedAt"`
	ReviewRemark string     `json:"reviewRemark"`
	RefundedAt   *time.Time `json:"refundedAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`

	// —— 只读派生（不属于售后单本身）——
	// 订单承诺赠送的福卡张数与构成快照：审核福卡规则要看的东西。
	FortuneCardsExpected int `json:"fortuneCardsExpected"`
	FortuneCardSnapshot  any `json:"fortuneCardSnapshot"`
	// 按行退时被退的那一行；整单退为 null。
	OrderLine *AfterSaleLineRef `json:"orderLine"`
}

// AfterSaleLineRef 是「退的是哪一行」的摘要。
//
// 不含取杯码，也不含券与活动快照：审核要看的是「哪一杯、几件、多少钱」，其余的按
// order_line_id 去订单详情看。
type AfterSaleLineRef struct {
	ID            string `json:"id"`
	LineNo        int    `json:"lineNo"`
	LineType      string `json:"lineType"`
	ItemName      string `json:"itemName"`
	Quantity      int    `json:"quantity"`
	PayableAmount int64  `json:"payableAmount"`
}
