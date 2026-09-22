package dto

// 协议变更事件的类型。与支付/退款结果走同一套 outbox 与同一个交易所，只是路由键不同。
//
// 这两个字符串同样是**路由契约**（EventType 就是 routing key）：membership-service 的消费者
// 没绑上它们时，发布方报成功、消息静默丢弃（见 [[publish-treats-unroutable-as-success]]）。
// 绑定的落点在 `deploy/compose/dev/docker-compose.yml` 里 membership-service 那一段——它要
// 同时含 `payment.agreement.signed` 与 `payment.agreement.terminated`，而**只写 `payment.*`
// 是不够的**：topic 交换机里 `*` 只匹配恰好一段，这两个键是三段（与退款那两个键同一个坑）。
//
// 只有这两个：**没有 agreement.pending**。pending 不是一个结论（用户还没在微信里点确认），
// 而这条事件的全部用处是「让订阅跟着协议走」——一份还没签成的协议不该让任何订阅动起来。
const (
	// EventAgreementSigned：协议生效了，以后可以扣款。
	EventAgreementSigned = "payment.agreement.signed"
	// EventAgreementTerminated：协议结束了（用户解约、渠道同步发现已解约）。
	//
	// 它是**终态**：一份已经解约的协议不会再签回来——用户要再用，走的是一次新的签约。
	// 所以收到它的一方可以把本地那条订阅直接收掉，不必等一个反向的事件。
	EventAgreementTerminated = "payment.agreement.terminated"

	// EventAgreementChargeSucceeded / EventAgreementChargeFailed：**某一期**扣款的结果。
	//
	// 与上面两条不是一类东西，别把四个并成一个词表：上面两条说的是**协议**的状态变化
	// （签了、解了），下面两条说的是**一次尝试**的结果，而协议在这两条之后仍然是 active——
	// 一次扣款失败不会让用户失去授权，它只说明这一期没扣到钱。
	//
	// 成对取名（charge_succeeded / charge_failed）而不是 succeeded / failed：路由键是
	// topic 交换机上的字符串，`payment.agreement.charge_failed` 与将来可能出现的
	// `payment.refund.failed` 要一眼分得开。
	//
	// 消费方（membership-service）**只认这两条来推进续费**，不看 ChargeAgreement 那个 RPC
	// 的返回值——受理不等于扣到钱（见 model.ChargeStatusCharging）。一条权威，避免「RPC 回包
	// + 事件」把同一期算成两期。
	EventAgreementChargeSucceeded = "payment.agreement.charge_succeeded"
	EventAgreementChargeFailed    = "payment.agreement.charge_failed"
)

// AgreementEventPayload 是 payment-service 发出的协议变更事件体。
//
// # 与别的负载不同，这份还没有镜像
//
// 支付与退款那两份负载在 order-service 里各有一份**逐字一致**的镜像结构（跨 module 不能
// 互相 import，只能靠一份镜像测试守住）。这份的消费方（membership-service 的订阅事件消费者）
// 还没写，所以今天**没有镜像可对**——写它的一方要负责在消费方落地时把字段对上，改这里之前
// 先搜一遍有没有人在解它。
//
// # 为什么每个字段都在
//
// 消费方要做的判断只有一条：「哪条订阅，变成什么状态」，所以载荷里的每个字段都对着那个判断
// 的一部分，没有一个是「留给以后」的：
//
//	agreementId  membership_subscriptions.agreement_id 就是它 —— 事件靠它命中那一行
//	status      目标状态（active / terminated），消费方据此短路幂等
//	userId      对一遍订阅的 user_id（我们这条协议确实是这个人的）
//	agreementNo 人读的那个号：排查时手里拿到的往往是我们自己的协议号或渠道协议号
//	contractNo  渠道侧的协议号，解约之后它就是唯一还能拿去问微信的东西
type AgreementEventPayload struct {
	// AgreementID 是 payment_agreements.id（UUID 文本）。**消费方按它匹配订阅**：它是
	// membership_subscriptions.agreement_id 的那一列，也是这条事件唯一能精确命中的键。
	AgreementID string `json:"agreementId"`
	// AgreementNo 是我们自己的协议号（payment_agreements.agreement_no），同时就是交给渠道的
	// contract_code——一个协议号，两个身份（见 provider.AgreementSignRequest.ContractCode）。
	AgreementNo string `json:"agreementNo"`
	// ContractNo 是渠道侧的协议号（微信的 contract_id）。
	//
	// 解约事件里它尤其重要：微信的解约通知**不带 contract_code**，所以从这一刻起，能拿去
	// 问渠道「这份协议怎么了」的只剩它。
	ContractNo string `json:"contractNo"`
	// UserID 是签约的用户（payment_agreements.user_id）。消费方拿它与订阅上的 user_id 对
	// 一遍：对不上说明这条事件说的不是那一行，宁可不动也不要改错人的会员。
	UserID string `json:"userId"`
	// Status 是**变更之后**的本库状态，取 model.AgreementStatusActive 或
	// model.AgreementStatusTerminated。
	//
	// 给的是状态而不是「这次是 ADD 还是 DELETE」：事件名已经说了那次变更，消费方要的却是
	// 「现在是什么」，让它自己从事件名推一遍状态等于把同一个判断写两份。
	Status string `json:"status"`
}
