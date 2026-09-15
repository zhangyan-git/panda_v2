package model

import "time"

// PaymentFunding 对应 payment_fundings 表：一个支付单下的逐笔出资。
//
// 微信/银联是渠道出资，咖啡豆是账户余额出资。混合出资必须
// 逐笔留行：退款要按来源分别冲正，对账要按渠道核对，只在支付单上存一个支付方式就丢了依据。
//
// 这张表与 order 库的 order_payment_lines 是**同一事实的两个视角**（资金侧 / 订单侧），
// 不是冗余：支付侧是「钱从哪儿来的」，订单侧是「这一单谁出的钱」，靠 payment.succeeded
// 事件对齐。两边都不许直接改对方的那份——发现不一致正是对账要报出来的差异。
type PaymentFunding struct {
	ID        string `db:"id"`
	PaymentID string `db:"payment_id"`
	LineNo    int    `db:"line_no"`
	LineType  string `db:"line_type"`
	Amount    int64  `db:"amount"`
	// reserved / succeeded / failed / released / reversed，见下面的常量。
	Status                string `db:"status"`
	ProviderTransactionID string `db:"provider_transaction_id"`
	FailureCode           string `db:"failure_code"`
	// account-service 的账变 ID（coffee_bean_entries.id）：账户出资扣的是余额，退款要按
	// 这笔账变冲正。**只有 line_type='coffee_bean' 的行有值**，渠道出资的行是 NULL——
	// 它本来就不是余额。
	//
	// 是个指针而不是 string：NULL 与「空 uuid」是两回事，用空串代替会让「这条出资是不是
	// 账户出资」变成一个要靠 line_type 再判一次的推断，而列本身的 NULL 就已经回答了。
	AccountEntryID *string    `db:"account_entry_id"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
	SucceededAt    *time.Time `db:"succeeded_at"`
	ReversedAt     *time.Time `db:"reversed_at"`
}

// 出资行状态，与 payment_fundings.status 的 CHECK 逐字一致，也与 order 库
// order_payment_lines.status 同词表。
const (
	// FundingReserved：已预占（账户余额先冻结待扣）。渠道出资建单后也是它——钱还没到。
	FundingReserved = "reserved"
	// FundingSucceeded：已成功。
	FundingSucceeded = "succeeded"
	FundingFailed    = "failed"
	// FundingReleased：已释放（预占解开、不再扣）。
	FundingReleased = "released"
	// FundingReversed：已冲正。
	FundingReversed = "reversed"
)
