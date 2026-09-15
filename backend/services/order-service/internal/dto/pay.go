package dto

// PayOrderRequest 是小程序发起一次支付的请求体。
//
// 只有一个字段，其余几样都**刻意不在这里**：
//
//   - 付款人身份（微信小程序支付要的 openid）：渠道需要它时那个值必须来自身份服务，
//     不能来自请求体——让客户端自称 openid 等于把「用谁的身份发起支付」交给调用方。
//     user-service 那边现在还没有把 openid 交出来的入口，所以本轮没有任何渠道拿得到它。
//   - 商品描述、渠道附加数据：支付侧按渠道规则拼，订单号它已经拿到了。
//
// 幂等号也不在请求体里：它是「这一次提交」的标识，不是支付的内容，所以走
// Idempotency-Key 请求头（与下单、申请退款同一条约定）。
type PayOrderRequest struct {
	PaymentMethodID string `json:"paymentMethodId"`
}

// PayAction 是一次发起支付的结果，字段与 payment-service 的 gRPC 契约一一对应。
//
// **它同时是客户端的调起指令**：action 说怎么调起（jump_miniapp / native_pay /
// direct_pay / qrcode / h5），payParams 是那种调法要的参数。客户端只认 action、不认渠道
// code——「新增一家同形态的渠道不用改客户端」这句话，靠的就是这两个字段。
//
// status='failed' 是**一个结果不是一个错误**（渠道明确拒绝、参数不全）：HTTP 仍然是 200，
// 客户端看 status 决定是调起支付还是让用户换一种方式。把渠道拒绝翻成 4xx/5xx 会让一次
// 正常的「换张卡付」看起来像服务端出了故障，而客户端会去重试一个注定失败的请求。
//
// 金额不在这里：客户端要付多少是订单的事，它已经在订单详情里拿到了 payableAmount。
type PayAction struct {
	// PaymentNo 是支付服务侧的支付单号。订单上的 payment_no 要等支付结果事件回来才写，
	// 所以客户端要查这一笔支付，用的是这个号。
	PaymentNo string `json:"paymentNo"`
	// Status 取 created / pending / succeeded / failed。pending 才是「可以去调起支付」；
	// succeeded 是**已经收妥**（今天只有账户出资：扣豆成功就是成功，没有第三方要等），
	// 客户端什么都不用做；failed 是发起即失败，可以让用户换一种方式。
	//
	// 三个非 created 的值与 payment-service 的 gRPC 契约（contracts/proto/payment/v1/
	// payment.proto 的 status 字段）逐字对应，改一处要同批改两处。
	Status string `json:"status"`
	// Action 见上，只在 pending 时有意义。
	Action string `json:"action"`
	// PayParams 是渠道返回的支付参数，扁平字符串键值。**只有非敏感的、客户端可见的键**。
	// 没有省略字段：null 与 {} 对客户端是两种读法（前者是 undefined，后者是空对象），
	// 而支付侧已经保证 pending 时它至少是 {}。
	PayParams map[string]string `json:"payParams"`
	// ExpiresAtUnix 是这张支付单的超时（Unix 秒），0 表示没有超时。它与订单的
	// expiresAt 不是同一个值——支付单的期限从发起支付算起，可以晚于订单的。
	ExpiresAtUnix int64 `json:"expiresAtUnix"`
	// FailureCode / FailureMessage 只在 status=failed 时有值。
	FailureCode    string `json:"failureCode,omitempty"`
	FailureMessage string `json:"failureMessage,omitempty"`
}
