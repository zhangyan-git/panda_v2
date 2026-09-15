package model

import "time"

// MessageInbox 对应 message_inbox 表，支付服务已接收消息事件及消费租约。
//
// 本轮支付服务不消费任何事件（发起支付是同步 RPC，回调是入站 HTTP），但这张表跟着
// 库一起建好了：服务订阅者（对账、退款编排）落地时不用再补一次迁移。留着不用的代价
// 是一张空表，不留的代价是加订阅者时要动 schema。
type MessageInbox struct {
	EventID     string     `db:"event_id"`
	ClaimedAt   time.Time  `db:"claimed_at"`
	LeaseOwner  *string    `db:"lease_owner"`
	LeaseToken  *string    `db:"lease_token"`
	LeaseUntil  *time.Time `db:"lease_until"`
	CompletedAt *time.Time `db:"completed_at"`
}
