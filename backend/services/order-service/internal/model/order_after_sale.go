package model

import (
	"encoding/json"
	"time"
)

// OrderAfterSale 对应 order_after_sales 表，售后申请。
//
// 只装「申请与处理结果」：真正的退款单、渠道调用、退款流水在 payment-service，
// 本库只留一个 refund_no 值引用——同一个退款在两边各存一份状态，迟早会不一致。
type OrderAfterSale struct {
	ID          string  `db:"id"`
	LegacyID    *string `db:"legacy_id"`
	AfterSaleNo string  `db:"after_sale_no"`
	OrderID     string  `db:"order_id"`
	// 冗余订单号：客服/对账都是拿着单号来找，不为了显示一个号再回表 join 一次。
	OrderNo string `db:"order_no"`
	UserID  string `db:"user_id"`
	// 类型暂时只有 refund。加新类型（换杯、补做）时扩 CHECK，不新开表。
	Type string `db:"type"`
	// 退款范围：all=整单，drink=只退饮品行，addon=只退加购行，membership=只退会员套餐。
	Scope string `db:"scope"`
	// 退的是哪一行。scope 只说到「哪一类」，说不出一单里两个不同加购活动的杯套分别是哪个，
	// 而退款金额、券的退回、赠卡追回都得落在具体那一行上。all 时为空——两头由
	// order_after_sales_scope_matches_line 约束成等价关系。
	OrderLineID *string `db:"order_line_id"`
	// pending / approved / rejected / refunding / refunded / failed / cancelled。
	Status string `db:"status"`
	Reason string `db:"reason"`
	// 用户上传的凭证图片 URL 列表。
	Images       json.RawMessage `db:"images"`
	RefundAmount int64           `db:"refund_amount"`
	RefundNo     string          `db:"refund_no"`
	FailureCode  string          `db:"failure_code"`
	// FailureMessage 是渠道回的原文（「原交易不存在」这种）。failure_code 给机器认、
	// 这一列给人读，后台那一行要解释「为什么没退成」靠的就是它。成功时两者都是空串。
	FailureMessage string     `db:"failure_message"`
	ReviewedBy     *string    `db:"reviewed_by"`
	ReviewedAt     *time.Time `db:"reviewed_at"`
	ReviewRemark   string     `db:"review_remark"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
	RefundedAt     *time.Time `db:"refunded_at"`
}

// 退款范围取值，与 order_after_sales.scope 的 CHECK 逐字一致。
const (
	AfterSaleScopeAll        = "all"
	AfterSaleScopeDrink      = "drink"
	AfterSaleScopeAddon      = "addon"
	AfterSaleScopeMembership = "membership"
)

// 售后状态取值，与 order_after_sales.status 的 CHECK 逐字一致。
const (
	AfterSaleStatusPending   = "pending"
	AfterSaleStatusApproved  = "approved"
	AfterSaleStatusRejected  = "rejected"
	AfterSaleStatusRefunding = "refunding"
	AfterSaleStatusRefunded  = "refunded"
	AfterSaleStatusFailed    = "failed"
	AfterSaleStatusCancelled = "cancelled"
)

// AfterSaleOpenStatuses 是「还没结束」的三个状态。
//
// 它同时是两件事的判据，所以必须只有一处：占用互斥位（同一行/整单不能有第二张在跑），
// 以及占用退款金额（整单退要减掉它们，否则同一笔钱能退两次）。伪造成「终态」的被驳回、
// 已撤销、退款失败都不在其中——那三种情况下钱没出去，用户本来就该能再申请一次。
var AfterSaleOpenStatuses = []string{
	AfterSaleStatusPending,
	AfterSaleStatusApproved,
	AfterSaleStatusRefunding,
}

// 审核动作。它不是状态：动作描述的是审核人做了什么，落到 status 上才是状态（approve 落
// approved，reject 落 rejected）。分成两个常量而不是一个 bool，是因为它要进审计载荷和
// 领域事件，那里需要一个人能读懂的字面量。
const (
	AfterSaleActionApprove = "approve"
	AfterSaleActionReject  = "reject"
)
