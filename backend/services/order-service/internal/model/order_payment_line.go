package model

import "time"

// OrderPaymentLine 对应 order_payment_lines 表，一次支付的出资分摊。
//
// 一个订单可能同时由微信、咖啡豆、家园消费金出资：退款要按来源分别冲正、
// 分账要按来源拆分，所以逐笔留行，而不是在主表存一个支付方式了事。
//
// 福卡**不在出资方之列**：它是下单赠送的抽奖凭证，余额归 account-service、消耗途径只有
// 抽奖（这两个服务都还没建）。规划里没有把它列成出资方，是本文件与那套旧词表的 CHECK 凭空
// 写上了它；那套词表今天已经退场，line_type 上只剩「非空」这一条 CHECK。
//
// 券不在这里：券是权益抵扣、不是资金出资，它抵掉的钱记在它作用的那一行
// （OrderLine.CouponDiscountAmount），核销事实在 coupon-service。
type OrderPaymentLine struct {
	ID      string `db:"id"`
	OrderID string `db:"order_id"`
	LineNo  int    `db:"line_no"`
	// 支付方式的 code：ums_h5_alipay / coffee_bean 等，与 orders.payment_method 同一个值。
	LineType string `db:"line_type"`
	Amount   int64  `db:"amount"`
	// reserved / succeeded / failed / released / reversed。
	Status string `db:"status"`
	// payment-service 的支付单号；渠道流水号与失败码一并留档。
	PaymentNo             string `db:"payment_no"`
	ProviderTransactionID string `db:"provider_transaction_id"`
	FailureCode           string `db:"failure_code"`
	// account-service 的账变 ID：咖啡豆出资扣的是账户余额，退款要按这笔账变冲正。
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

// line_type 那套「出资渠道」词表（wechat / unionpay / coffee_bean / wallet / other）与
// 它的五个常量**已经退场**（见 migrations/order）：order_payment_lines.line_type 今天存的是
// **支付方式的 code**（catalog 里的 ums_h5_alipay / coffee_bean 等），与
// orders.payment_method 同一个值。
//
// 从前它是「这笔钱从哪个通道出」的归纳，与「用户点了哪个支付方式」分两处记，代价是加一种
// 支付方式要改两个库的 DDL——支付宝在词表里没有档，只能落 `other`，后台于是把一笔支付宝单
// 显示成「其他」。CHECK 也已经从那份词表换成 `line_type <> ''`。
//
// 唯一的历史遗留：那个词表时期写下的行（以及 order 库里由回填订正过的那些）。新代码不该
// 再引用 wechat / unionpay / wallet / other 这几个值。
