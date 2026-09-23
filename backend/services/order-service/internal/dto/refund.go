package dto

// 退款结果事件的类型。与支付那两条（见 payment.go）同一套 outbox 机制、同一个主题，
// 只是 event_type 不同——payment-service 在自己的事务里写完退款单再发，order-service
// 幂等地把售后单推进到位。
//
// **没有 refund.processing**：一笔退款停在渠道那边（应答是 PROCESSING / UNKNOWN）对订单侧
// 没有可执行的含义——售后单已经在 refunding 上了，而那张单只有等渠道给出结论才会动。多发
// 一个中间事件只会让下游多写一条「什么都别做」的分支。
const (
	EventPaymentRefundSucceeded = "payment.refund.succeeded"
	EventPaymentRefundFailed    = "payment.refund.failed"
)

// RefundEventPayload 是 payment-service 发出的退款结果事件体。
//
// 与 PaymentEventPayload 同一条约定：解码用 DisallowUnknownFields（见
// service.HandleRefundEvent），多发一个字段会在反序列化阶段就报错，而不是被静默忽略——
// 而这里被忽略的那次漂移可能是支付侧改了退款单号的字段名，我们还按旧名字在推进售后单。
//
// 它比支付那条事件多一个 `afterSaleNo` 并**以它为主键**：售后单号是这条链的幂等键
// （payment_refunds.after_sale_no 整表唯一），order-service 拿它找那张单，而不是拿 refund_no
// ——退款单号是先建单后回填到 order_after_sales.refund_no 的，用它反查会在「钱退成了、
// 但回填那一步没跑」时找不到任何东西。
type RefundEventPayload struct {
	// AfterSaleNo 是 order-service 的售后单号，值引用。**落点**：这条事件推进的就是它。
	AfterSaleNo string `json:"afterSaleNo"`
	// RefundNo 是 payment-service 的退款单号。它会被拿去与库里的 refund_no 对一次：
	// 对不上说明这条事件说的是另一张退款单，那要人来查，不能当作 replay 忽略掉。
	RefundNo string `json:"refundNo"`
	// OrderNo / PaymentNo 是排查用的值引用，落库时不读它们——售后单那一行已经带着订单号，
	// 而支付单号是支付域的事实。
	OrderNo   string `json:"orderNo"`
	PaymentNo string `json:"paymentNo"`
	// Amount 是这一笔退款的总额，单位为分。**失败事件里也是同一个数**，不是 0：它描述的是
	// 「本来要退多少」，而售后单上那一栏早在申请时就写死了。
	Amount int64 `json:"amount"`
	// Fundings 是逐笔出资的冲正结果。今天 order 库不落它（退款单本身在支付域），带在体里
	// 是为了让「钱从哪几笔出资里退回去的」在一次联调或事后追查中看得见。
	Fundings []RefundFunding `json:"fundings"`
	// SucceededAtUnix 是钱退回去的时刻（Unix 秒）。0 表示事件没带，服务端用 NOW()。
	SucceededAtUnix int64 `json:"succeededAtUnix"`
	// FailureCode / FailureMessage 只在失败那条事件里有值，写进
	// order_after_sales.failure_code，后台列表与客服据此解释「为什么没退成」。
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// RefundFunding 是一笔出资的冲正。
type RefundFunding struct {
	// LineType 是这行退的是哪种支付方式：catalog 的 code（ums_h5_alipay / coffee_bean 等），
	// 与 payment_fundings.line_type 一致——出资渠道那套词表已经退场（见 migrations/payment）。
	LineType string `json:"lineType"`
	Amount   int64  `json:"amount"`
	// AccountEntryID 只有账户出资（咖啡豆）那条路可能有，而且**今天恒为空**——豆的冲正
	// 由 account-service 消费 order.after_sale.refunded（退款成功那条事件）自己完成，
	// 本服务不发起、也拿不到那个 ID（见 payment-service model.RefundFunding 的注释）。
	// 指针而不是 string：支付侧发的是 null，收成空串会把「没有这个值」与「有一个空 ID」
	// 混成一件事。
	AccountEntryID *string `json:"accountEntryId"`
}
