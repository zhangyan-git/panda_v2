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
	ID        string  `db:"id"`
	ChannelID *string `db:"channel_id"`
	Provider  string  `db:"provider"`
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
// 本轮只用到 CallOperationCreate。
const (
	CallOperationCreate = "create"
	CallOperationQuery  = "query"
	CallOperationClose  = "close"
	CallOperationRefund = "refund"
)
