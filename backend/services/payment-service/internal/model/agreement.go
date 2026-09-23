package model

import (
	"encoding/json"
	"time"
)

// PaymentAgreement 对应 payment_agreements 表：一次委托代扣签约。
//
// 它是「用户授权我们以后按期从他的账户扣钱」这件事的本地记录。协议本身是**渠道侧的**一份
// 授权（由渠道签发、由渠道的状态机决定生死、由渠道在扣款时校验），这张表记的是我们这边的
// 那一半：我们发起过、渠道确认了没有、渠道侧的协议号是多少、下一次该在什么时候扣。
//
// # 只有一个协议编号
//
// AgreementNo 同时就是签约时交给渠道的 contract_code（见 service 里 createAgreement 的注释）。
// 老系统有两个号（自己的 _id 与 contract_code），扣款回传的 out_trade_no 与回调查询用的号
// 又对不上，于是续费回调永远查不到订阅、微信无限重推——那条路上真正缺的就是「一个两边同源
// 的键」。这里只留一个。
type PaymentAgreement struct {
	ID string `db:"id"`
	// AgreementNo 是我们自己的协议号，业务唯一。**它同时是渠道侧的 contract_code**，
	// 渠道推回来的通知按它认这一份协议。
	AgreementNo string `db:"agreement_no"`
	// LegacyID 是老库代扣/订阅签约记录的 ObjectID（24 位 hex），只有存量行有值。
	LegacyID *string `db:"legacy_id"`
	UserID   string  `db:"user_id"`
	// Provider 是渠道名（如 `wechat_pay`）。代扣只能走渠道，所以它非空。
	Provider string `db:"provider"`
	// PaymentMethod 是签约时用户选的那一档支付方式 code（catalog 里的常量，如 `wechat_papay`）。
	PaymentMethod string `db:"payment_method"`
	// ContractNo 是**渠道侧的协议号**（微信的 contract_id）。它只有渠道确认之后才有值：
	// 签约发起时它还是空的，用户签完由通知或查约补上。扣款与解约都按它认这一份协议。
	ContractNo string `db:"contract_no"`
	// Subject 是签约用途，如「会员自动续费」。它进渠道账单，也是后台列表上唯一的人话。
	Subject string `db:"subject"`
	// PlanCode 是**业务侧**的签约计划标识（如会员套餐代码）。本库不解释它的含义，只在
	// 查约与排查时带出来——「这个计划是干嘛的」是业务方的事实。
	PlanCode string `db:"plan_code"`
	// MaxChargeAmount 是单次扣款上限，单位为分；0 表示签约未约定上限。
	//
	// 它是**签约要素**：渠道要求签约时就写明单次上限，超过它的扣款请求渠道会拒。所以它随
	// 签约一起冻结在这里，而不是每次扣款时现问业务方。
	MaxChargeAmount int64 `db:"max_charge_amount"`
	// NextChargeAt 是下一次该扣款的时间，**由业务方更新**。
	//
	// 本服务不推算续费周期、也**不按这一列扫描该扣谁**：排期的权威是 membership-service 的
	// membership_subscriptions.next_charge_at，它到点了才调 ChargeAgreement 进来。这一列是
	// 业务方写进来给人看的那一份（后台列表、排查「这份协议下次什么时候动钱」），
	// payment_agreements_pending_charge_idx 今天没有调用方。
	NextChargeAt *time.Time `db:"next_charge_at"`
	// Status 取下面那六个常量之一。
	Status string `db:"status"`
	// Metadata 装「本库不解释、但要留给下一刀用」的那几个值（见 AgreementMetadataKeys）。
	Metadata json.RawMessage `db:"metadata"`
	// SignedAt / ActivatedAt / TerminatedAt 是三个时间点。它们各自有列而不是挤在一个
	// updated_at 里，是因为后台要回答的是「他什么时候授权的」而不是「这行什么时候改过」。
	SignedAt     *time.Time `db:"signed_at"`
	ActivatedAt  *time.Time `db:"activated_at"`
	TerminatedAt *time.Time `db:"terminated_at"`
	// TerminateReason 是解约原因。用户在渠道那边解约时渠道不给理由，那时它记的是我们自己
	// 写的一句话（见签约通知那条路）。
	TerminateReason string    `db:"terminate_reason"`
	CreatedAt       time.Time `db:"created_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}

// 协议状态，与 payment_agreements.status 的 CHECK 逐字一致。
const (
	// AgreementStatusPending：已发起签约，等用户在渠道那边确认。
	AgreementStatusPending = "pending"
	// AgreementStatusActive：用户已确认，可以扣款。
	//
	// 表注释里 pending 与它之间还有一个 signed（「用户已确认」），本服务**不写那个值**：
	// 今天「用户确认了」与「这份协议能扣款了」是同一件事——确认之后没有任何一步还要再等
	// （业务侧的排期由 next_charge_at 表达，不是一个中间状态）。写成 signed 的话，未来那条
	// 扫待扣款的查询（`WHERE status='active'`）会把每一份刚签好的协议都漏掉。
	AgreementStatusActive = "active"
	// AgreementStatusTerminated：已解约。
	AgreementStatusTerminated = "terminated"
)

// 协议 metadata 里认识的键。**不做成列**：它们是渠道侧的实现细节（换一家渠道就不是这几个
// 值），而业务侧要用的三个（计划、上限、下次扣款）都已经有列了。
const (
	// AgreementMetaProviderPlanID 是渠道侧的签约模板 id（微信的 plan_id）。查约要带它，
	// 而它按业务套餐配、存在业务库里，签约那一刻由调用方给进来。
	AgreementMetaProviderPlanID = "providerPlanId"
)

// PaymentAgreementCharge 对应 payment_agreement_charges 表：一份协议上的某一期扣款。
//
// # 一行就是这一期的全部事实
//
// 「一期只扣一次」的锁是库上那条 UNIQUE (AgreementID, BizPeriod)：重试是**同一行**把
// attempt_count 加一、状态往前推，不是新开一行（新开一行就可能扣两次）。所以这一行同时承担
// 三件事：幂等凭据（重试时先来这儿看一眼）、渠道单号的存放处（OutTradeNo）、以及这一期的账
// （status / charged_at）。
type PaymentAgreementCharge struct {
	ID          string `db:"id"`
	AgreementID string `db:"agreement_id"`
	AgreementNo string `db:"agreement_no"`
	// BizPeriod 是期次，由业务方给（如 2026-10）。本库不推算、不校验格式。
	//
	// 业务方那边它必须是**稳定**的：同一期重试时算出来要一样，否则 UNIQUE 挡不住第二笔。
	// membership-service 从订阅的 next_charge_at 派生它，正是因为那个值扣成之前不推进。
	BizPeriod string `db:"biz_period"`
	Amount    int64  `db:"amount"`
	// OutTradeNo 是这一期在渠道那边的商户单号（微信报文里的 out_trade_no），**扣款结果通知
	// 回来时唯一的关联键**——报文里没有别的字段能定位到这一行。
	//
	// **建行时生成一次，重试复用同一个值，绝不重算**：重算就是在渠道侧开出第二笔订单，而两个
	// 单号不同，渠道那边无法去重，用户会被扣两次（见 payment_agreement_charges.out_trade_no 的列注释）。
	OutTradeNo string `db:"out_trade_no"`
	// ProviderTransactionID 是这一期在渠道那边的流水号（微信的 transaction_id），由扣款结果
	// 通知写进来。它与 OutTradeNo 是**两个方向**的号：那个是我们发给渠道的，这个是渠道发的，
	// 用户在微信账单里看到的是这一个。
	//
	// 列上有部分唯一索引（payment_agreement_charges_transaction_unique）：同一笔渠道流水挂到两期上，含义就是同一笔钱被记了两次。
	ProviderTransactionID string `db:"provider_transaction_id"`
	// PaymentNo 是这一期成功扣款对应的支付单号（值引用）；今天恒为空——代扣不建 payments
	// 行（payments.order_no 是 NOT NULL 的订单号，而代扣没有订单）。
	PaymentNo string `db:"payment_no"`
	// Status 取下面那六个常量之一。
	Status       string `db:"status"`
	AttemptCount int    `db:"attempt_count"`
	// NextRetryAt 是下一次可以再试的时间。退避由本服务算（24h × 已试次数），**由调用方反复
	// 来问**：没到期就原样回现状、不碰渠道。封顶见 service.chargeMaxAttempts。
	NextRetryAt    *time.Time `db:"next_retry_at"`
	FailureCode    string     `db:"failure_code"`
	FailureMessage string     `db:"failure_message"`
	ChargedAt      *time.Time `db:"charged_at"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
}

// 一期扣款的状态，与 payment_agreement_charges.status 的 CHECK 逐字一致。
const (
	// ChargeStatusPending：这一行刚建出来，还没问过渠道。
	ChargeStatusPending = "pending"
	// ChargeStatusCharging：渠道**受理了**这一笔，钱到没到还不知道。
	//
	// 它是这一刀最要紧的一个状态。老系统把「受理」当成了「扣到钱」——拿到 transaction_id 就
	// 记账、续会员，而渠道那边可能随后扣款失败（余额不足、风控）。真正的结论只有渠道推回来的
	// 那条通知能给，所以受理之后必须停在这里等。
	ChargeStatusCharging = "charging"
	// ChargeStatusSucceeded：渠道确认扣到了钱（由通知推进，不由受理推进）。
	ChargeStatusSucceeded = "succeeded"
	// ChargeStatusFailed：这一期的一次尝试失败了。**不是终态**——重试复用同一行，
	// attempt_count 加一、NextRetryAt 往后推；试满 chargeMaxAttempts 次之后不再重试。
	ChargeStatusFailed = "failed"
	// ChargeStatusSkipped：这一期有意不扣（业务方说不用了，如签约生效前已经退订）。
	ChargeStatusSkipped = "skipped"
	// ChargeStatusCancelled：协议在扣款途中被解约，这一期作废（终态）。
	ChargeStatusCancelled = "cancelled"
)

// ChargeMaxAttempts 是一期扣款最多问渠道几次。
//
// 3 是老系统那个数（它数到 3 就把订阅关掉），这里沿用**次数**但改了处置：试满之后这一期停在
// failed、不再重试，而订阅由 membership-service 置 suspended（不是 cancelled）。理由见
// ChargeStatusFailed 与计划 §六.4——扣款失败的常见原因是余额不足，那是会变的，系统不该替
// 用户把授权撤了。
const ChargeMaxAttempts = 3

// ChargeBackoff 是两次尝试之间的间隔：24h × 已试次数。
//
// 递增而不是固定 24h：第一次失败多半是当天余额不足，隔一天再试同一个时间点没有意义；拉到
// 48h / 72h 之后，「这一期到底还扣不扣」有了一个收敛的期限。与老系统不同——它有两个互不知情
// 的定时任务在扫（计划 §六.3），退避写在某一处的 NextDeductionDate 上，另一处根本不看它。
const ChargeBackoff = 24 * time.Hour

// ChargeNextRetryAt 算出这次尝试失败之后，下一次可以再试的时间；**已试满则返回 nil**。
//
// nil 的含义是「不再重试」，不是「没排期」——两者在库上都是 NULL 那一列，读的一侧必须靠
// AttemptCount 区分（见 service 里那条状态分流）。做成 model 里的纯函数是因为**两条路都要
// 用**：同步被拒时（service 记的那次尝试）与通知说失败时（仓储写的那一行），两条路算出来的
// 退避必须一致，否则同一期的重试节奏取决于失败是怎么回来的。
func ChargeNextRetryAt(now time.Time, attemptCount int) *time.Time {
	if attemptCount >= ChargeMaxAttempts {
		return nil
	}
	next := now.Add(ChargeBackoff * time.Duration(attemptCount))
	return &next
}
