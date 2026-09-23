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
	// 走哪条渠道，值是渠道在**代码里**的名字（catalog 里的 Channel.Provider，今天只有一个
	// `ums`）；纯账户出资（咖啡豆）没有第三方，这里是空串。
	//
	// 它从前是一个指向 payment_channels 的外键。那张表没了之后，这一列仍然值得留：后台
	// 列表要显示「这笔走的哪条渠道」，对账要按渠道分组，而这两件事都不该要求再查一次表。
	Provider string `db:"provider"`
	// 用户在收银台上选的那一个支付方式的 **code**（catalog 里的常量，如 `ums_h5_wechat`）。
	//
	// 与 Provider 的关系是「一个渠道下挂着几条方式」：小程序那条与 H5 那四条同属 ums，
	// 咖啡豆那条不属于任何渠道。所以这两列不是一回事，缺任一列都答不出「用户当初选了什么」。
	PaymentMethod string `db:"payment_method"`
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
	// 渠道支付永远是 nil——这条路上没有账户域可扣。见 payments.account_entry_id 的列注释。
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

// IsPaymentStatus 判断一个值是不是合法的支付单状态。
//
// 后台列表的 status 筛选用它**在进 SQL 之前**挡下打错的值（见 controller 的 listPayments）：
// 一个不在词表里的值送进 SQL 不报错，只会安静地返回空列表——而运营会把它当成「这段时间
// 真的没有成功的单」拿去对账。
//
// 写成 switch 而不是从上面那六个常量拼一个切片：这里要的是「整份摆出来」，多一个常量时
// 少写一行会当场编译不过（重复的 case），而漏在一个切片里不会。
func IsPaymentStatus(status string) bool {
	switch status {
	case PaymentCreated, PaymentPending, PaymentSucceeded, PaymentFailed, PaymentClosed, PaymentExpired:
		return true
	default:
		return false
	}
}

// 出资类型那套词表（wechat / unionpay / coffee_bean / wallet / other）**已经退场**，
// 连同 payments.funding_type 那一列一起。
//
// 从前它是「这笔钱从哪个通道出」的归纳，与「用户点了哪个支付方式」分两处记。代价是加一种
// 支付方式要同时在 catalog 里给 code、再在这套词表里给它找一档——找不到就得改两个库的 DDL，
// 支付宝落 `other` 就是这么来的（后台订单列表把一笔支付宝单显示成「其他」）。
//
// 今天只剩一套值：catalog 的 code。出资行、记账流水、退款出资行都存它。要判「这笔钱走不走
// 渠道」，看的是这条方式有没有渠道（catalog.Method.ChannelCode / 路由上的 Channel），
// 不是看某个词表值——唯一的例外见 service/refund.go 里那段关于 coffee_bean 的说明。
