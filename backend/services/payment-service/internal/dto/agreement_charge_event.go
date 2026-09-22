package dto

// AgreementChargeEventPayload 是某**一期**扣款结果的事件体，由 EventAgreementChargeSucceeded
// 与 EventAgreementChargeFailed 两条事件共用。
//
// 一条事件只描述一期，所以载荷里没有「第几期」之外的歧义：bizPeriod 就是那一期的标识，而它
// 在扣成之前不推进（见 model.PaymentAgreementCharge.BizPeriod），消费方拿它做幂等与对账时
// 拿到的是稳定的值。
//
// # 与 AgreementEventPayload 的关系
//
// 两份载荷说的不是一件事，所以不复用结构：那一份说的是协议（签了、解了），这一份说的是钱
// （这一期扣到了没有）。消费方（membership-service）两条都收，各自落到订阅的不同列上。
//
// # 为什么带上 amount 与 providerTransactionId
//
// 消费方要用它们写自己的续费流水：amount 是它记「这期收了多少钱」的唯一来源（membership 那
// 边不存金额，金额的权威在 payment 这一侧），providerTransactionId 则是那条流水的幂等键
// ——同一个渠道流水重投时靠它撞唯一索引，而不是靠事件本身「只发一次」这个假设。
type AgreementChargeEventPayload struct {
	// AgreementID 是 payment_agreements.id，也就是 membership_subscriptions.agreement_id。
	// **消费方按它命中那一行订阅**，别的字段都定位不到。
	AgreementID string `json:"agreementId"`
	// AgreementNo 是我们自己的协议号，也是渠道侧的 contract_code，给人看与排查用。
	AgreementNo string `json:"agreementNo"`
	// UserID 是签约用户。消费方拿它与订阅上的 user_id 对一遍：对不上说明这条事件说的不是
	// 那一行，宁可不动也不要改错人的会员（与协议事件同一条规矩）。
	UserID string `json:"userId"`
	// BizPeriod 是这一期的标识（由 membership-service 从订阅的 next_charge_at 派生）。
	BizPeriod string `json:"bizPeriod"`
	// Amount 是这一期的金额，单位为分。它与 payment_agreement_charges.amount 逐字相同。
	Amount int64 `json:"amount"`
	// ProviderTransactionID 是渠道侧的流水号。**成功事件里它非空**（扣到钱才有号）；
	// 失败事件里可能是空的——渠道拒一笔时不给交易号。
	ProviderTransactionID string `json:"providerTransactionId"`
	// Status 是**变更之后**这一期的状态，取 model.ChargeStatusSucceeded 或
	// model.ChargeStatusFailed。
	//
	// 与协议事件一样给的是状态而不是「这是哪次变更」：事件名已经说了那次变更，消费方要的却是
	// 「现在是什么」。冗余是有意的——消费方少写一个判断，就少一处能和事件名对不上的地方。
	Status string `json:"status"`
	// FailureCode / FailureMessage 只在失败事件里有值（渠道给的原因），供消费方记流水与
	// 后台展示。它们**不是给人看的最终文案**：用户看到的那句话由会员侧组织。
	FailureCode    string `json:"failureCode,omitempty"`
	FailureMessage string `json:"failureMessage,omitempty"`
}
