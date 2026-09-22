package model

import "time"

// PaymentTransaction 对应 payment_transactions 表：不可变的资金流水（方案 5.9）。
//
// 操作表（payments / payment_refunds）记的是当前状态，会一直被改写；这一张只追加，记的是
// 「什么时候真的进出过一笔钱」，对账（5.9、14.3）以它为基准。
//
// 冲正不删不改原记录，新增一条反向记录（方案 8.2）——Kind='reversal'、Direction 与原记录
// 相反，于是「这条流水现在还剩多少」永远是整表求和，而不是某一行被改成了什么。
type PaymentTransaction struct {
	ID string `db:"id"`
	// 老库（ObjectID 的 24 位 hex），存量的流水迁进来时有值。
	LegacyID *string `db:"legacy_id"`
	// payment=一笔出资成功入账，refund=一笔出资被退回，reversal=冲正。
	Kind string `db:"kind"`
	// 三个定位键都是空串而不是 NULL：NULL 在唯一索引里互不相等，空串才能真的挡住重复记账。
	// UNIQUE (kind, payment_no, refund_no, funding_line_no) 就是靠它们工作的。
	PaymentNo string `db:"payment_no"`
	RefundNo  string `db:"refund_no"`
	// 出资序号，对应 payment_fundings.line_no；没有具体行时为 0。
	FundingLineNo int    `db:"funding_line_no"`
	LineType      string `db:"line_type"`
	Direction     string `db:"direction"`
	Amount        int64  `db:"amount"`
	// 走哪条渠道，值是渠道名（同 payments.provider），账户出资的那条流水是空串。
	//
	// 这里**从前也没有外键**（见 001），理由今天反而变成了它的常态：流水是对账基准，
	// 而渠道已经不在库里了——它写在这里是一个**事实的快照**，不是一个引用。
	Provider              string  `db:"provider"`
	ProviderTransactionID string  `db:"provider_transaction_id"`
	AccountEntryID        *string `db:"account_entry_id"`
	// 渠道给的成交时间，不是我们记账的时间。
	OccurredAt time.Time `db:"occurred_at"`
	CreatedAt  time.Time `db:"created_at"`
}

// 流水类型，与 payment_transactions.kind 的 CHECK 逐字一致。
const (
	TransactionPayment  = "payment"
	TransactionRefund   = "refund"
	TransactionReversal = "reversal"
)

// 资金方向，与 payment_transactions.direction 的 CHECK 逐字一致。
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)
