package model

import (
	"encoding/json"
	"time"
)

// PaymentProviderCall 对应 payment_provider_calls 表：我们主动打给渠道的每一次调用。
//
// 方案 6 要求所有第三方适配器都有调用流水。**超时与「结果未知」必须留痕**：一笔状态不明
// 的转账是运维要拿它去渠道查的线索——没有这条记录，一次超时过后我们既不知道发出去没有，
// 也不知道该拿什么号去问。
//
// RequestSummary / ResponseSummary 是**脱敏后**的摘要：不放签名原文、密钥、完整卡号与
// 身份证（方案 11.5），只放能定位这一笔的键（商户单号、渠道单号、金额、返回码）。
type PaymentProviderCall struct {
	ID string `db:"id"`
	// Provider 是这条流水打给哪条渠道（渠道名，如 `ums`）。它从前还有一个指向
	// payment_channels 的 channel_id，那一列随那张表一起删了——两者说的是同一件事，
	// 而渠道名字本来就写在报文里，不再需要一个 id 去指。
	Provider string `db:"provider"`
	// create / query / close / refund / query_refund / agreement_sign / agreement_charge /
	// agreement_terminate / reconcile。
	Operation string `db:"operation"`
	// 定位键三件套，本轮只用到 PaymentNo。
	PaymentNo   string `db:"payment_no"`
	RefundNo    string `db:"refund_no"`
	AgreementNo string `db:"agreement_no"`
	// 调用方的幂等号与 trace，出问题时拿它们和上游日志对时间线。
	RequestID string `db:"request_id"`
	TraceID   string `db:"trace_id"`
	// 第几次尝试。同一个逻辑调用被重试时递增，好让「重试之后才成功」看得出来。
	AttemptNo       int             `db:"attempt_no"`
	RequestSummary  json.RawMessage `db:"request_summary"`
	ResponseSummary json.RawMessage `db:"response_summary"`
	HTTPStatus      *int            `db:"http_status"`
	ProviderCode    string          `db:"provider_code"`
	ProviderMessage string          `db:"provider_message"`
	// success / failed / timeout / unknown。
	Result     string    `db:"result"`
	DurationMS *int      `db:"duration_ms"`
	CreatedAt  time.Time `db:"created_at"`
}

// 渠道调用结果，与 payment_provider_calls.result 的 CHECK 逐字一致。
const (
	CallSuccess = "success"
	CallFailed  = "failed"
	// CallTimeout：请求发出去了但我们没等到应答。与 CallFailed 分开是必须的——失败可以
	// 重试，超时不能直接重试（钱可能已经收了），得先查单。
	CallTimeout = "timeout"
	// CallUnknown：连「发出去没有」都不确定。
	CallUnknown = "unknown"
)

// 渠道调用操作类型，与 payment_provider_calls.operation 的 CHECK 逐字一致。
//
// 这里只列**代码里真有写路径**的那几个，不是把 CHECK 抄全：迁移里还有一个 reconcile 值，
// 而对账这件事整个系统都还没有（见 provider.Operation 的注释）。给它留一个没有调用方的常量，
// 会让「对账做了没有」在读代码时看起来是「做了」。
const (
	CallOperationCreate = "create"
	// CallOperationQuery：回渠道问一件事。它今天有两个调用方——支付单的主动查单，以及
	// **查签约协议**（`papay/querycontract`）。签约那边没有单独的 operation 值：CHECK 里
	// 没有 agreement_query，而这一行靠 agreement_no 就能定死是哪一份协议。
	CallOperationQuery  = "query"
	CallOperationClose  = "close"
	CallOperationRefund = "refund"
	// CallOperationQueryRefund：查一笔退款在渠道那边的状态。退款那条路的
	// PROCESSING / UNKNOWN 只有它能给出结论（见 provider.OperationQueryRefund）。
	CallOperationQueryRefund = "query_refund"
	// CallOperationAgreementSign：一次签约的签名计算。
	//
	// 它记的是**我们这边算出了签约参数**这件事，不是一次出网调用——纯签约由客户端跳到
	// 渠道的签约页完成（见 wechatpay 的包注释）。所以这一行的 http_status / duration_ms
	// 是空的、attempt_no 恒为 1：它没有「发了几次」可言。
	CallOperationAgreementSign = "agreement_sign"
	// CallOperationAgreementCharge：一次代扣扣款（微信的 `pay/pappayapply`）。
	//
	// **预扣费通知不单独记一行**：它没有结果要回给我们（单向告知），而且每一次预扣费通知都
	// 紧跟着一次扣款尝试——单独记一行会让「这一期问过渠道几次」这个数变成两倍。它的成败写在
	// 扣款那一行的 response_summary 的 preNotify 那一段里（见 wechatpay 的 Provider.preNotify）。
	CallOperationAgreementCharge = "agreement_charge"
	// CallOperationAgreementTerminate：一次解约（微信的 `papay/deletecontract`）。
	//
	// 它有两种来路，同记一个值：用户在小程序点「关闭自动续费」，以及运营在后台取消订阅。
	// 两条路都是「我们主动去撤这份授权」，流水上分得开的是 request_id 与 trace_id。
	CallOperationAgreementTerminate = "agreement_terminate"
)
