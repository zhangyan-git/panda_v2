package model

import "time"

// MessageInbox 对应 message_inbox 表，订单服务已接收消息事件及消费租约。
//
// 支付结果走事件进来（方案 7.3），重投是常态——broker 重连或「处理完但没来得及 ack」
// 都会产生，所以消费必须幂等，这张表就是那个依据。
type MessageInbox struct {
	EventID     string     `db:"event_id"`
	ClaimedAt   time.Time  `db:"claimed_at"`
	LeaseOwner  *string    `db:"lease_owner"`
	LeaseToken  *string    `db:"lease_token"`
	LeaseUntil  *time.Time `db:"lease_until"`
	CompletedAt *time.Time `db:"completed_at"`
}
