package dto

// 代扣协议（payment_agreements）在本服务这一侧的形状。
//
// **它不是本域的事实**：协议住在 payment-service，本服务只在 membership_subscriptions 上留
// 两个值引用（agreement_id / contract_code）。这三个结构体是**跨服务调用的输入输出**，所以放
// 在 dto 里而不是 service 里：client 与 service 都要用它们，放在任何一边都会让另一边反过来
// import——order-service 那条链路上的 dto.PayAction 是同一个位置、同一个理由。
//
// 与 payment v1 的 proto 一比一，但**不直接用生成的类型**：那会把 proto 的字段名、可空语义
// （map 的 nil 与空、bool 的 zero）泄进 service 层，而这一层要的是「签约这件事在我们这儿长
// 什么样」。转换只发生在 client 里，一处。

// CreateAgreementParams 是发起一次签约要交给支付服务的事实。
//
// 每个字段都从**本域的权威来源**来，没有一个是客户端直接给的：
//
//	UserID / WalletOpenID  令牌解出来的用户 + 身份域查出来的 openid
//	PlanCode / ProviderPlanID / MaxChargeAmount / Subject  套餐（签约那一刻的快照）
type CreateAgreementParams struct {
	UserID string
	// PlanCode 是套餐编码。支付服务**不解释它**，只落库与它一起带回来，事后排查时手里有个号。
	PlanCode string
	// ProviderPlanID 是渠道侧的签约模板 id（微信的 plan_id），取自套餐的 wechat_plan_id。
	//
	// 由本服务给而不是支付服务配：模板是**按套餐**配的，而套餐住在会员库。
	ProviderPlanID string
	// WalletOpenID 是用户在微信小程序下的 openid，**签约要素**：渠道拿它认「签的是谁」。
	// 它来自身份域（见 client.WalletIdentityReader），不来自请求体。
	WalletOpenID string
	// Subject 是签约用途，渠道账单上的一句话（如「熊猫咖啡连续包月」）。
	Subject string
	// MaxChargeAmount 是单次扣款上限，单位为分；0 表示未约定上限。
	//
	// 它随签约一起**冻在渠道与协议上**（渠道要求签约时就写明），所以取值只能是套餐价——
	// 给一个比套餐价小的数，以后每一期都扣不动；给大了则失去它本来的意义。
	MaxChargeAmount int64
	// RequestID 是这次签约的幂等号，原样交给支付服务做协议的幂等键。
	RequestID string
}

// AgreementSigningResult 是发起签约的结果。
//
// **Status 恒为 pending**：真正的渠道动作（用户在微信里点同意）发生在客户端跳过去之后。它
// 留着是为了让「拿到的是当时那份协议的状态」这句话在重放时也成立。
type AgreementSigningResult struct {
	// AgreementID 落进 membership_subscriptions.agreement_id。
	AgreementID string
	// AgreementNo 是我们自己的协议号，同时就是交给渠道的 contract_code——**签约那一刻就要
	// 写进 subscription.contract_code**：对账时手里拿到的往往就是它。
	AgreementNo string
	Status      string
	// Action 是支付方式目录里的行为码（今天签约是 jump_miniapp），客户端只 switch 它。
	Action string
	// PayParams 是跳转微信签约小程序要带的参数，**含签名，是一次性凭据**。
	PayParams map[string]string
}

// AgreementState 是回渠道核一份协议之后的状态。
//
// Status 是**纠正之后的本库状态**（支付库的），不是渠道的原话——渠道的原话在 ProviderState
// 里。两者的分工：Status 决定本服务动不动订阅，ProviderState 是排查时先看的那一眼。
type AgreementState struct {
	AgreementNo string
	// Status 取 active / terminated / pending（payment 的 model.AgreementStatus*）。
	//
	// **terminated 同时包括「查无此约」**：对订阅来说这两件事的结论一样——这份授权今天不能用
	// 了。要分清它们得看 ProviderState。
	Status string
	// ContractNo 是**渠道侧的协议号**（微信的 contract_id），只留在 payment_agreements.contract_no。
	//
	// **不要把它写进订阅**：membership_subscriptions.contract_code 装的是我们自己的商户协议号
	// （= AgreementNo）。两者长得像，但用途完全不同——回渠道查协议、解约、对账用的都是商户协议号，
	// 拿渠道那个号去查会得到一句看不懂的错。本服务到今天为止**从不读它**，留着是因为它随状态一起
	// 回来，排查时手里要看的就是它。
	ContractNo string
	// ProviderState 是渠道给的原始状态词（signed / terminated / pending），脱敏、不翻译。
	ProviderState string
	// Changed 是这次核查有没有改支付库那一行。后台的「同步」按钮拿它决定提示「已同步」还是
	// 「无需更正」——两个都是成功，但运营要知道自己那一下有没有起作用。
	Changed bool
}

// 协议状态的三个取值，**逐字照抄 payment-service 的 model.AgreementStatus\***：它们是那边的
// 库值，本服务只是认它（认的方式是拿去做 switch，见 service.settleTargetFor）。
//
// 抄成常量而不是散在 switch 里的字面量：这三个词一旦哪边改了拼写，散着的写法会静默走进
// default 那一支——而那一支的语义是「还没结果，什么都别动」，于是签约生效这件事会**静默
// 不发生**，没有任何报错，用户签完了还是 pending。
const (
	AgreementStatusPending    = "pending"
	AgreementStatusActive     = "active"
	AgreementStatusTerminated = "terminated"
)

// ChargeAgreementParams 是发起一期代扣要交给支付服务的事实。
//
// 金额与期次都**由本服务权威给出**（订阅上的 PriceCents 快照与 NextChargeAt 派生的期次），
// 支付服务不回头核对套餐——「这一期该扣多少」是会员域的事实（见 model.Subscription 的说明）。
type ChargeAgreementParams struct {
	// AgreementNo 是商户协议号，即 subscription.contract_code。
	//
	// **不是渠道侧的 contract_id**：签约时交给渠道的是 contract_code，扣款与解约认的才是
	// contract_id，而那个号冻在支付库里、本服务拿不到也不需要拿。
	AgreementNo string
	// BizPeriod 是这一期的幂等键，由 NextChargeAt 派生（见 model.ChargePeriod）。
	BizPeriod string
	// Amount 是这一期要扣的金额，单位为分，取自订阅上的 PriceCents 快照（签约时约定死的，
	// 套餐后来调价不影响已签约的人）。
	Amount int64
	// Subject 是渠道账单上的一句话，取套餐名，与签约那条路同一个来源（见 signing.go）。
	Subject string
	// RequestID 只进支付侧的流水，**不参与幂等判定**——幂等键是 (agreement_no, biz_period)。
	RequestID string
}

// AgreementChargeResult 是一次发起扣款的结论。
//
// **它不是「扣到钱了没有」**：渠道同步回的受理只说明请求被收下了（Status=charging），钱到没
// 到在渠道推回来的通知里，本服务从 dto.EventAgreementChargeSucceeded 那两条事件得知。
type AgreementChargeResult struct {
	AgreementNo string
	BizPeriod   string
	// Status 取下面这组常量（与 payment_agreement_charges 的 CHECK 同一套词）。
	Status string
	// ProviderTransactionID 是渠道那一期的流水号，受理即返回；没有别的用途，出问题时拿去
	// 微信商户平台比对的它。
	ProviderTransactionID string
	FailureCode           string
	FailureMessage        string
}

// 代扣那一期的状态，**逐字照抄 payment-service 的 model.ChargeStatus\***。理由与上面那三个
// 协议状态逐字相同：认错了会静默走进 default——那一边的语义是「这不是个我认识的状态」。
//
// 六个取值本服务**一个都不改订阅状态**，只按它们决定日志的轻重（见 service/charge.go 的
// switch）。成与不成分别由 charge_succeeded / charge_failed 两条事件推进——那两条知道的不比
// 这里少，还知道钱到底到没到。
const (
	ChargeStatusPending   = "pending"
	ChargeStatusCharging  = "charging"
	ChargeStatusSucceeded = "succeeded"
	ChargeStatusFailed    = "failed"
	ChargeStatusSkipped   = "skipped"
	ChargeStatusCancelled = "cancelled"
)

// TerminateAgreementParams 是解一份协议要交给支付服务的事实。
type TerminateAgreementParams struct {
	AgreementNo string
	// Reason 落支付库也进渠道的备注。它是**给人和渠道看的**（「用户关闭自动续费」/「管理员取消
	// 订阅」），不是一个给程序判的分支。
	Reason string
	// RequestID 只进支付侧的流水。
	RequestID string
}

// AgreementTermination 是解约的结论。
type AgreementTermination struct {
	AgreementNo string
	// Status 见上面那组协议状态常量，成功即 terminated。
	Status string
	// FailureCode 非空表示**渠道明确拒绝了解约**——它是业务结论不是 error，调用方拿到它时该做
	// 的是什么都不改（见 client.AgreementClient.Terminate）。
	FailureCode    string
	FailureMessage string
}
