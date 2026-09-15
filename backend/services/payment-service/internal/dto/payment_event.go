// Package dto 是支付服务的对外数据形状：目前只有发出去的支付结果事件体。
//
// 它之所以单独成一个包，是因为这份结构**不是我们自己说了算的**：order-service 用
// `DisallowUnknownFields` 解它（order-service/internal/service/payment.go），多发一个
// 字段、改一个 json tag 都会在联调时炸。把它放在一处，是为了让「我们发出去的形状」
// 有一个可以被测试指着的东西（见 payment_event_test.go 里那份手工照抄的镜像）。
package dto

// 支付结果事件的类型。方案 7.3：Payment 在本地事务里更新支付单、写流水、写 outbox，
// 然后经 RabbitMQ 发布，Order 幂等地更新订单状态。
//
// EventType 就是 RabbitMQ 的 routing key（交易所按它路由），所以这两个字符串是**路由
// 契约**，不是普通的常量：改一个字母，事件就会投进一个没人绑定的路由，然后被静默丢掉
// （见 [[publish-treats-unroutable-as-success]]）。
const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentFailed    = "payment.failed"
)

// EventVersion 是事件信封上的版本号，不是负载里的字段。
//
// 它进 messaging.Envelope.EventVersion，与 order-service 那边的取值一致。负载本身
// **不套信封**：payload 是裸的 PaymentEventPayload JSON，多加一层会让对面解不出来。
const EventVersion = "v1"

// PaymentEventPayload 是 payment-service 发出的支付结果事件体。
//
// 字段与 json tag 必须与 order-service/internal/dto/payment.go 的 PaymentEventPayload
// **逐字一致**。两份不能合并：跨 module 不能互相 import（每个服务是独立 module，
// contracts 里那条 gRPC 契约管不到 MQ 上的这份），所以只能靠一份镜像测试守住
// （payment_event_test.go）。
//
// 一个反直觉的地方：`payment.failed` 也带 Amount，而且带的是**支付单的应付额**，
// 不是渠道说的那个数。order-service 的 settleFailed 拿它记「这次尝试想扣多少」，
// 记 0 会让订单侧的失败流水金额靠兜底逻辑猜。
type PaymentEventPayload struct {
	// OrderNo 是订单号（orders.order_no）。用单号而不是订单 ID：支付服务手里只有下单时
	// 拿到的那张单，让两边各自维护对方的内部 ID 只会多一份对不上的可能。
	OrderNo   string `json:"orderNo"`
	PaymentNo string `json:"paymentNo"`
	// Amount 单位为分。order-service 必须拿它和订单的 payable_amount 对一遍：对不上就
	// 不落单，而不是信它——一个错的事件把订单标成已支付，比一次失败难查得多。
	Amount int64 `json:"amount"`
	// PaymentMethod 取 payments.funding_type，落到 orders.payment_method 上用于列表
	// 展示与对账归类。
	PaymentMethod string `json:"paymentMethod"`
	// Fundings 是逐笔出资分摊。为空时 order-service 按「单笔、渠道 = PaymentMethod、
	// 金额 = Amount」补一行；本轮永远是恰好一行（渠道出资），混合出资要等 account-service。
	Fundings []PaymentFunding `json:"fundings"`
	// ProviderTransactionID 是渠道侧交易号，落到订单侧的支付流水上供反查渠道。
	ProviderTransactionID string `json:"providerTransactionId"`
	// PaidAtUnix 是支付时间（Unix 秒）。0 表示事件没带，order-service 用 NOW()。
	// **用渠道给的成交时间**：补投或重放一条旧事件时，NOW() 会把「昨晚付的款」记成
	// 「刚才」，对账就永远对不平。
	PaidAtUnix     int64  `json:"paidAtUnix"`
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// PaymentFunding 是一笔出资：渠道支付，或者账户余额（咖啡豆）。
type PaymentFunding struct {
	LineType  string `json:"lineType"`
	Amount    int64  `json:"amount"`
	PaymentNo string `json:"paymentNo"`
	// AccountEntryID 是 account-service 的账变 ID，只有账户余额出资有。**必须是指针**：
	// 写成 string 会在没有账变时发一个 ""，而对面收到的是一个「给了但为空」的值。
	AccountEntryID *string `json:"accountEntryId"`
}
