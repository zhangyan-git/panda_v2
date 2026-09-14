package dto

// 支付结果事件的类型。方案 7.3：Payment 在本地事务里更新支付单、写流水、写 outbox，
// 然后经 RabbitMQ 发布，Order 幂等地更新订单状态。创建支付那条链路（7.2）不能走 MQ，
// 因为客户端要立刻拿到支付参数——但那条链路不归 order-service，本服务只消费结果。
const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentFailed    = "payment.failed"
)

// PaymentEventPayload 是 payment-service 发出的支付结果事件体。
//
// 字段是**事件体**，不是 HTTP 请求体，所以它没有 json 标签之外的保护：解码用的是
// DisallowUnknownFields（见 service.HandlePaymentEvent），多发一个字段会在反序列化
// 阶段就报错，而不是被静默忽略——契约漂移要在联调时炸，不要在线上悄悄少扣一笔钱。
type PaymentEventPayload struct {
	// OrderNo 是订单号（orders.order_no）。用单号而不是订单 ID：支付服务手里只有下单时
	// 拿到的那张单，让两边各自维护对方的内部 ID 只会多一份对不上的可能。
	OrderNo   string `json:"orderNo"`
	PaymentNo string `json:"paymentNo"`
	// 事件体里的金额，单位分。服务端必须拿它和订单的 payable_amount 对一遍：对不上就不落单，
	// 而不是信它——一个错的事件把订单标成已支付，比一次失败难查得多。
	Amount int64 `json:"amount"`
	// 主渠道，写进 orders.payment_method 用于列表展示与对账归类。
	PaymentMethod string `json:"paymentMethod"`
	// 逐笔出资分摊。为空时按「单笔、渠道 = PaymentMethod、金额 = Amount」落一行，
	// 覆盖纯微信支付这个最常见的情况；混合出资（咖啡豆 + 福卡 + 微信）必须逐笔给全，
	// 否则退款时按来源冲正就没有依据。
	Fundings              []PaymentFunding `json:"fundings"`
	ProviderTransactionID string           `json:"providerTransactionId"`
	// 支付时间（Unix 秒）。0 表示事件没带，服务端用 NOW()。
	PaidAtUnix     int64  `json:"paidAtUnix"`
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// PaymentFunding 是一笔出资：渠道支付，或者账户余额（咖啡豆/福卡）。
type PaymentFunding struct {
	LineType  string `json:"lineType"`
	Amount    int64  `json:"amount"`
	PaymentNo string `json:"paymentNo"`
	// account-service 的账变 ID，只有账户余额出资有。
	AccountEntryID *string `json:"accountEntryId"`
}
