package model

import "time"

// OrderPaymentLine 对应 order_payment_lines 表，一次支付的出资分摊。
//
// 一个订单可能同时由微信、咖啡豆、福卡、家园消费金出资：退款要按来源分别冲正、
// 分账要按来源拆分，所以逐笔留行，而不是在主表存一个支付方式了事。
//
// 券不在这里：券是权益抵扣、不是资金出资，它抵掉的钱记在它作用的那一行
// （OrderLine.CouponDiscountAmount），核销事实在 coupon-service。
type OrderPaymentLine struct {
	ID      string `db:"id"`
	OrderID string `db:"order_id"`
	LineNo  int    `db:"line_no"`
	// wechat / unionpay / coffee_bean / fortune_card / wallet / other。
	LineType string `db:"line_type"`
	Amount   int64  `db:"amount"`
	// reserved / succeeded / failed / released / reversed。
	Status string `db:"status"`
	// payment-service 的支付单号；渠道流水号与失败码一并留档。
	PaymentNo             string `db:"payment_no"`
	ProviderTransactionID string `db:"provider_transaction_id"`
	FailureCode           string `db:"failure_code"`
	// account-service 的账变 ID：咖啡豆/福卡出资扣的是账户余额，退款要按这笔账变冲正。
	AccountEntryID *string    `db:"account_entry_id"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
	SucceededAt    *time.Time `db:"succeeded_at"`
	ReversedAt     *time.Time `db:"reversed_at"`
}

// 出资状态取值，与 order_payment_lines.status 的 CHECK 逐字一致。
const (
	PaymentLineReserved  = "reserved"
	PaymentLineSucceeded = "succeeded"
	PaymentLineFailed    = "failed"
	PaymentLineReleased  = "released"
	PaymentLineReversed  = "reversed"
)

// 出资方取值。既有渠道支付，也有账户余额（咖啡豆/福卡）出资。
const (
	FundingWechat      = "wechat"
	FundingUnionPay    = "unionpay"
	FundingCoffeeBean  = "coffee_bean"
	FundingFortuneCard = "fortune_card"
	FundingWallet      = "wallet"
	FundingOther       = "other"
)
