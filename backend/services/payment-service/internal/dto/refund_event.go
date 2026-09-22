package dto

// 退款结果事件的类型。与支付结果走同一套 outbox 与同一个交易所，只是路由键不同。
//
// 这两个字符串同样是**路由契约**（EventType 就是 routing key）：order-service 的
// RABBITMQ_ROUTING_KEY 里没有它们时，发布方报成功、消息静默丢弃——那个坑本仓库踩过。
//
// 落点在 `deploy/compose/dev/docker-compose.yml` 里 order-service 那一段，绑的是
// `payment.*,payment.refund.*`：**只写 `payment.*` 是不够的**，topic 交换机里 `*` 匹配
// 恰好一段，而这两个键是三段。
//
// 只有 succeeded 与 failed 两个：**没有 refund.processing**。processing 不是结论，
// order-service 拿着它什么也做不了——售后单本来就停在 refunding，那正是对的（钱确实
// 还没退回去）。把「还没定论」也发出去，只会让下游多一个必须忽略的事件类型。
const (
	EventPaymentRefundSucceeded = "payment.refund.succeeded"
	EventPaymentRefundFailed    = "payment.refund.failed"
)

// RefundEventPayload 是 payment-service 发出的退款结果事件体。
//
// 字段与 json tag 必须与 order-service/internal/dto/refund.go 的 RefundEventPayload
// **逐字一致**，理由与 PaymentEventPayload 那段逐字相同：跨 module 不能互相 import，
// 而对面用 DisallowUnknownFields 解它，只能靠一份镜像测试守住。
//
// # 为什么带 AfterSaleNo 而不是只带 RefundNo
//
// order-service 收到事件之后要改的是**那张售后单**，而它记的是 refund_no——两边都有也能
// 认，但 after_sale_no 是这条链真正的业务唯一（一张售后单最多一张退款单）。带上它，
// 事件就能直接命中主键，不用先按 refund_no 反查一遍；而 refund_no 仍然带，是因为
// 人工排查时手里拿到的往往是渠道/支付单号那一边。
type RefundEventPayload struct {
	OrderNo     string `json:"orderNo"`
	PaymentNo   string `json:"paymentNo"`
	RefundNo    string `json:"refundNo"`
	AfterSaleNo string `json:"afterSaleNo"`
	// Amount 单位为分，是这张退款单的**总额**，等于 Fundings 各项之和。
	//
	// 失败事件也带它，而且带的是同一个数（不是 0）：order-service 拿它对着售后单上的
	// refund_amount 复核，记 0 会让它以为这次退款是零元。
	Amount int64 `json:"amount"`
	// Fundings 是逐笔出资的冲正结果。为空时 order-service 按「单笔、金额 = Amount」补
	// 一行，与支付事件那边同一个兜底口径。
	Fundings []RefundFunding `json:"fundings"`
	// SucceededAtUnix 是钱退回去的时刻（Unix 秒），用**渠道给的成交时间**而不是我们记账的
	// 时刻；0 表示事件没带，order-service 用 NOW()。理由与 PaidAtUnix 逐字相同：
	// 补投一条旧事件时 NOW() 会把「上周退的款」记成「刚才」。
	SucceededAtUnix int64  `json:"succeededAtUnix"`
	FailureCode     string `json:"failureCode"`
	FailureMessage  string `json:"failureMessage"`
}

// RefundFunding 是一笔出资的冲正：渠道退回，或者账户余额（咖啡豆）冲正。
//
// 与 PaymentFunding 几乎同形，但**不带 paymentNo**：那个值在事件的顶层，每行复制一遍
// 只会多一个能对不上的地方。
type RefundFunding struct {
	// LineType 是这笔出资的支付方式 code（如 `coffee_bean` / `ums_h5_alipay`），与
	// payment_refund_fundings.line_type 同一套值——出资渠道词表已退场，见 payment/012。
	LineType string `json:"lineType"`
	Amount   int64  `json:"amount"`
	// AccountEntryID 是 account-service 那笔反向账变的 ID。
	//
	// **今天永远是 null**：咖啡豆的冲正由 account-service 自己消费
	// `order.after_sale.refunded` 完成——那是本服务的退款成功事件经订单域转出来的，
	// 冲正发生在本服务之后，本服务拿不到那个 ID——account-service 的
	// ReverseCoffeeBeanEntry **有意不回**它。
	// 留着这一栏是为了让 order-service 的镜像结构与支付事件那份同形，将来账户域能回填时
	// 下游不用改。
	AccountEntryID *string `json:"accountEntryId"`
}
