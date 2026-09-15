package model

import (
	"encoding/json"
	"time"
)

// Payment 对应 payments 表：一次「向用户收这笔钱」的尝试。
//
// 一个订单可以有多个支付单：第一次支付失败、超时被关，用户可以再发起一次，那是新的一行。
// 但**成功只能有一张**（payments_one_succeeded_per_order 部分唯一索引），因为订单只能被
// 收一次钱。所以「这次能不能建单」不是看有没有既存支付单，而是看有没有成功的那张。
type Payment struct {
	ID        string `db:"id"`
	PaymentNo string `db:"payment_no"`
	// 老库（MongoDB ObjectID 的 24 位 hex），仅迁移过来的行有值。
	LegacyID *string `db:"legacy_id"`
	// 订单号（orders.order_no），值引用：订单事实的归属方是订单服务，这里只存一个值。
	OrderNo string `db:"order_no"`
	UserID  string `db:"user_id"`
	// 应付总额，单位为分；等于各笔出资之和。
	Amount int64 `db:"amount"`
	// 主出资类型：支付结果事件里的 paymentMethod 取它，落到 orders.payment_method 上。
	FundingType string `db:"funding_type"`
	// 走哪套渠道配置；纯账户出资（咖啡豆）为空。
	ChannelID *string `db:"channel_id"`
	// 用户是在哪一档支付方式下发起的；渠道侧的支付可以没有。
	PaymentMethodID *string `db:"payment_method_id"`
	// created / pending / succeeded / failed / closed / expired，见下面的常量。
	Status string `db:"status"`
	// 渠道收银台与账单上显示的商品描述。
	Subject string `db:"subject"`
	// 渠道附加数据（小程序 openid、设备号等），**不放密钥**。
	Attach json.RawMessage `db:"attach"`
	// 渠道侧的交易号，回调与查单都以它为准。
	ProviderTransactionID string `db:"provider_transaction_id"`
	FailureCode           string `db:"failure_code"`
	FailureMessage        string `db:"failure_message"`
	// 发起方给的幂等号，落到这一行上供排查；真正挡重的是 payment_idempotency_keys。
	RequestID string     `db:"request_id"`
	ExpiresAt *time.Time `db:"expires_at"`
	// 账户出资（纯豆）扣豆的账变 ID，在**扣豆返回的那一刻**就落库、先于结算。
	//
	// 非空 = 这笔钱确实动过。它有两个用途：超时关单不碰这样的单（把钱已经动过的单当成
	// 没付过一样关掉、把出资标成 released，是一次记账上的撒谎），补偿任务把它们结算掉。
	// 渠道支付永远是 nil——这条路上没有账户域可扣。见 005 迁移。
	AccountEntryID *string `db:"account_entry_id"`
	// 扣豆成交的时刻（账户域那笔账变的发生时刻）。账户出资没有第三方给成交时间，
	// 结算与补偿都用它，而不是拿处理请求的那一刻顶上。
	AccountFundedAt *time.Time `db:"account_funded_at"`
	// 渠道给的成交时间，不是我们记账的时间。
	PaidAt    *time.Time `db:"paid_at"`
	ClosedAt  *time.Time `db:"closed_at"`
	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
}

// 支付单状态，与 payments.status 的 CHECK 逐字一致。
const (
	// PaymentCreated：已建单、还没向渠道发起。
	PaymentCreated = "created"
	// PaymentPending：已向渠道发起（或已返回支付参数），等回调或轮询。
	PaymentPending = "pending"
	// PaymentSucceeded / PaymentFailed 是终态。
	PaymentSucceeded = "succeeded"
	PaymentFailed    = "failed"
	// PaymentClosed：订单取消等主动关单；PaymentExpired：待支付超时。
	PaymentClosed  = "closed"
	PaymentExpired = "expired"
)

// 主出资类型取值，与 payments.funding_type / payment_fundings.line_type 的 CHECK
// 逐字一致，也与 order 库 order_payment_lines.line_type 同词表。
//
// 这里**没有** fortune_card：福卡是下单赠送的抽奖凭证，不是出资渠道（它的余额归
// account-service、消耗途径只有抽奖）。规划里从来没有把它列成出资渠道——§3.1 只说它
// 「订单完成发放、用于参与抽奖」，§5.6 把它的余额归在 Account 下。payment/004 已收窄。
const (
	FundingWechat     = "wechat"
	FundingUnionPay   = "unionpay"
	FundingCoffeeBean = "coffee_bean"
	FundingWallet     = "wallet"
	FundingOther      = "other"
)
