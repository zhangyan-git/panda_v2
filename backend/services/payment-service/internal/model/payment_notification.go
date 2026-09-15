package model

import (
	"encoding/json"
	"time"
)

// PaymentNotification 对应 payment_notifications 表：渠道回调的原文与处理结论。
//
// UNIQUE (provider, notification_id) 是防重放的那道闸：同一个通知重投是常态（渠道重试、
// 我们应答超时），靠它挡在业务事务之外——冲突即重投，直接回渠道要的成功应答，不碰任何状态。
//
// Body 是渠道原始报文，**只落在这一张表里**：方案 11.5 禁止「未脱敏的第三方完整回调报文」
// 进日志，所以日志侧只允许记 BodySHA256 与摘要。
type PaymentNotification struct {
	ID        string  `db:"id"`
	Provider  string  `db:"provider"`
	ChannelID *string `db:"channel_id"`
	// 渠道侧的通知/流水唯一号（微信 resource.id、银联请求流水号、丰选通知序号）。
	NotificationID string `db:"notification_id"`
	EventType      string `db:"event_type"`
	PaymentNo      string `db:"payment_no"`
	RefundNo       string `db:"refund_no"`
	Body           []byte `db:"body"`
	BodySHA256     string `db:"body_sha256"`
	// 只留定位用的请求头（请求号、时间戳、签名算法名）；签名原文与密钥不在这里。
	Headers json.RawMessage `db:"headers"`
	// 验签结论。false 且 status='failed' 表示「这条回调我们没认」，此时支付状态绝不改动。
	SignatureVerified bool `db:"signature_verified"`
	// received / processed / ignored / failed。
	Status string `db:"status"`
	// 失败原因写脱敏后的一句话，不写验签原文。
	FailureReason string     `db:"failure_reason"`
	ReceivedAt    time.Time  `db:"received_at"`
	ProcessedAt   *time.Time `db:"processed_at"`
}

// 回调记录状态，与 payment_notifications.status 的 CHECK 逐字一致。
const (
	// NotificationReceived：已落库、还没处理。
	NotificationReceived = "received"
	// NotificationProcessed：已落进业务事务（或已确认是重投而跳过）。
	NotificationProcessed = "processed"
	// NotificationIgnored：认了但不需要动状态（例如非支付结果的通知类型）。
	NotificationIgnored = "ignored"
	// NotificationFailed：没认——验签不过、金额对不上、状态冲突。
	NotificationFailed = "failed"
)
