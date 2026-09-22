package model

import "time"

// Refund 对应 payment_refunds 表：**一笔退款**这件事本身。
//
// 它是支付单之外的一个独立聚合，理由与 payments 那一段同源：一次支付可以有多次退款
// （先退加购行、再退整单），而「这笔钱收没收到」不该因为退过一次就要读两个状态才知道。
// payments.status 在退款期间始终是 succeeded（见 service/state.go 里 paymentTransitions
// 那段注释），退款的进度只在这张表上。
type Refund struct {
	ID string `db:"id"`
	// RefundNo 是本服务生成的退款单号，带 REF 前缀与时间戳（见 service.refundNo）。
	//
	// 它**整表唯一**，并且会被编成渠道侧的商户账单号发出去（带 sourceCode 前缀，
	// 见 provider/ums/orderid.go 末尾那一节）——所以它的长度有上限，不能随意加长。
	RefundNo string `db:"refund_no"`
	// LegacyID 是老库 refund_orders._id（ObjectID 的 24 位 hex），迁移过来的行才有值。
	LegacyID *string `db:"legacy_id"`
	// PaymentID 指向被退的那张支付单。**NOT NULL 是这条链的形状，不是可选的严格**：
	// 取货码 / 刷卡机那类订单根本没有 payments 行，它们进不了这张表，也就进不了退款链。
	// 那种单要在别处定退款语义（见 order-service 的 ApplyAfterSale 那一条拒绝）。
	PaymentID string `db:"payment_id"`
	PaymentNo string `db:"payment_no"`
	OrderNo   string `db:"order_no"`
	// AfterSaleNo 是 order-service 的售后单号，**整表唯一**——一张售后单最多落一张退款单。
	//
	// 它同时是这条链的幂等键：调用方重发同号时拿回同一张退款单。用售后单而不是请求号，
	// 是因为「一次退款只该发生一次」这件事的出处就是售后单，那才是业务上的唯一。
	AfterSaleNo string `db:"after_sale_no"`
	// OrderLineID 是 order_lines.id，值引用；整单退为空。与 order_after_sales 上那条
	// scope/line 的等价 CHECK 同一条口径。
	OrderLineID *string `db:"order_line_id"`
	UserID      string  `db:"user_id"`
	// Amount 单位为分，是这一笔退款的**总额**，等于各行出资冲正之和。
	Amount int64  `db:"amount"`
	Reason string `db:"reason"`
	// pending / processing / succeeded / failed / cancelled，见下面的常量。
	Status string `db:"status"`
	// ProviderRefundID 是渠道侧的退款单号。PROCESSING 那些笔在查到结论之前一直是空串。
	ProviderRefundID string `db:"provider_refund_id"`
	FailureCode      string `db:"failure_code"`
	FailureMessage   string `db:"failure_message"`
	RequestID        string `db:"request_id"`
	// SucceededAt 是钱真正退回去的时刻（渠道给的，不是我们记账的时刻）。
	SucceededAt *time.Time `db:"succeeded_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

// RefundFunding 对应 payment_refund_fundings 表：一张退款单下**逐笔出资**的冲正。
//
// 它存在的理由与 payment_fundings 逐字对称：用户付的时候可能一半豆一半渠道，退的时候要按
// 来源分别冲，只在退款单上记一个总额就丢了对账依据。
//
// 今天它**恒为一行**（出资今天恒为一行，见 service.create 里 FundingLines 那段注释），
// 但形状按多行设计，混合出资落地时这条链不用再改。
type RefundFunding struct {
	ID       string `db:"id"`
	RefundID string `db:"refund_id"`
	// FundingID 指向被冲的那一行 payment_fundings.id。可空：存量迁移过来的退款可能找不到
	// 对应的出资行。
	FundingID *string `db:"funding_id"`
	// LineNo 与 payment_fundings.line_no 对齐，让两条链的行能按序号互相认出来。
	LineNo   int    `db:"line_no"`
	LineType string `db:"line_type"`
	Amount   int64  `db:"amount"`
	// pending / succeeded / failed，见下面的常量。
	Status           string `db:"status"`
	ProviderRefundID string `db:"provider_refund_id"`
	FailureCode      string `db:"failure_code"`
	// AccountEntryID 是账户出资冲正产生的反向账变 ID。
	//
	// **今天永远是 NULL**，而且这不是待办：咖啡豆的冲正由 account-service 自己消费
	// `order.after_sale.refunded` 完成（幂等键 `after_sale:{afterSaleNo}`）——那是本服务的
	// 退款成功事件经订单域转出来的结果，本服务不发那条请求、也就拿不到那个 ID——
	// account-service 的 ReverseCoffeeBeanEntry **有意不回**冲正流水的 ID（见
	// coffee_bean.proto 里那句「入口是 after_sale:{afterSaleNo} 的唯一索引」）。
	// 这一列留给将来由账户域回填或由对账补齐。
	AccountEntryID *string    `db:"account_entry_id"`
	SucceededAt    *time.Time `db:"succeeded_at"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
}

// 退款单状态，与 payment_refunds.status 的 CHECK 逐字一致。
const (
	// RefundPending：已建单，还没向渠道发起。只出现在「建完单、发起之前进程死了」那个窗口里，
	// 重发同一个 after_sale_no 会接着把它推下去。
	RefundPending = "pending"
	// RefundProcessing：已向渠道发起，渠道还没给结论（应答是 PROCESSING / UNKNOWN，或者那次
	// 调用超时了）。**这个状态要有人跟**——退款查询 worker 会扫它（见 worker/refund.go）。
	//
	// 与支付单的 pending 一样，它是一个**会堆积**的状态：规范里没有退款回调，渠道不会主动
	// 来告诉我们结果，只能靠问。
	RefundProcessing = "processing"
	// RefundSucceeded / RefundFailed 是终态。
	RefundSucceeded = "succeeded"
	RefundFailed    = "failed"
	// RefundCancelled：预留。今天没有任何写路径会把它写进去——售后单的撤销走的是
	// order_after_sales 自己的 cancelled，那时候退款单根本还没建。
	RefundCancelled = "cancelled"
)

// IsRefundStatus 判断一个值是不是合法的退款单状态。
//
// 写成 switch 而不是从上面那组常量拼一个切片：与 IsPaymentStatus 同一条理由——漏在一个
// 切片里不会编译不过，漏在一个 switch 里会（重复的 case）。
func IsRefundStatus(status string) bool {
	switch status {
	case RefundPending, RefundProcessing, RefundSucceeded, RefundFailed, RefundCancelled:
		return true
	default:
		return false
	}
}

// 退款出资行状态，与 payment_refund_fundings.status 的 CHECK 逐字一致。
//
// 它比出资行的状态机（reserved / succeeded / failed / released / reversed）短得多，因为
// 退款这一侧没有「预占」那一步：一笔退款要么发出去了、要么还没发，没有「先占住再确认」。
const (
	RefundFundingPending   = "pending"
	RefundFundingSucceeded = "succeeded"
	RefundFundingFailed    = "failed"
)
