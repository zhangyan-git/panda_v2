package model

import "time"

// MessageInbox 对应 message_inbox 表，优惠券服务已接收消息事件及消费租约。
type MessageInbox struct {
	EventID     string     `db:"event_id"`
	ClaimedAt   time.Time  `db:"claimed_at"`
	LeaseOwner  *string    `db:"lease_owner"`
	LeaseToken  *string    `db:"lease_token"`
	LeaseUntil  *time.Time `db:"lease_until"`
	CompletedAt *time.Time `db:"completed_at"`
}
