package dto

// 支付域在本服务这一侧的**只读**形状：一期代扣的账，以及一张支付单的摘要。
//
// 它们与 agreement.go 里那批（签约、查约、扣款、解约）分开一个文件，是因为用途不同：那一批是
// 本服务**推进代扣**用的输入输出，这一批只服务后台「包月订阅」详情抽屉里那两块展示（续费明细、
// 首月支付信息），本服务一个字都不改。分开之后，「这条链路上哪些调用会改别的域的状态」在目录
// 上一眼可见。
//
// 同样与 payment v1 的 proto 一比一，同样不直接用生成的类型（理由见 agreement.go 开头那段）。

// AgreementCharge 是一期代扣在本服务这一侧的形状，逐字对应 payment v1 的 AgreementCharge。
//
// **带 json tag，与 PaymentSummary / OrderSummary 那两个不同**：这一个会随订阅详情原样发给前端
// （`charges[]` 就是它）。少了 tag 出去的是 `BizPeriod` 这种大写开头的键，而前端照 camelCase 写
// ——那一块会显示成一片空白，两边都不报错。
type AgreementCharge struct {
	// BizPeriod 是这一期的期次（yyyyMMdd），也是这一期在渠道那边的业务标识。
	BizPeriod string `json:"bizPeriod"`
	// Amount 是这一期**应收**的金额（分），不是已收——失败的那几期一分钱都没收到。
	Amount int64 `json:"amount"`
	// Status 取 pending / charging / succeeded / failed / skipped / cancelled，
	// 取值同 ChargeStatus* 那组常量。
	Status string `json:"status"`
	// AttemptCount 是已经向渠道发起过几次。读它比读状态更能说明「这一期在反复重试」。
	AttemptCount int32 `json:"attemptCount"`
	// NextRetryAt / ChargedAt / CreatedAt 都是 RFC3339（UTC）字符串，空串表示没有这个时刻。
	//
	// **保持字符串**，不在这一层解成时间：这三个值一路原样回给前端显示，解一次再编回去只会多
	// 一个出错的环节（同 OrderSummary.PaidAt 的取舍）。而空串与「零值时刻」在这里是两件事
	// ——未扣成的期次不该在界面上显示一个公元元年的日期。
	NextRetryAt string `json:"nextRetryAt"`
	ChargedAt   string `json:"chargedAt"`
	CreatedAt   string `json:"createdAt"`
	// ProviderTransactionID 是渠道流水号，成功那一期就是它；未受理时为空串。
	//
	// 它是这一页给人看的最要紧的一格：出了争议时，运营拿它去微信商户平台查这一笔。
	ProviderTransactionID string `json:"providerTransactionId"`
	// FailureCode / FailureMessage：失败的机器可读原因与给人看的那句话，成功与进行中的期次
	// 都是空串。
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// PaymentSummary 是一张支付单在本服务这一侧的形状，逐字对应 payment v1 的 GetPaymentResponse。
//
// 它对这一页的**唯一**用途是 provider_transaction_id 那一格（首月那笔的微信流水）。其余几格
// 留着是因为它们随同一次调用一起回来，摆在同一块信息里比不摆更有用。
type PaymentSummary struct {
	PaymentNo string
	Status    string
	Amount    int64
	// PaymentMethod 是 payment 目录里的 code（ums_h5_alipay / coffee_bean …），与订单上那一列
	// 同一个词表。
	PaymentMethod         string
	ProviderTransactionID string
	// PaidAt 是 RFC3339（UTC），未支付成功时为空串。
	PaidAt string
	// OrderNo 是订单号，值引用。
	OrderNo string
}
