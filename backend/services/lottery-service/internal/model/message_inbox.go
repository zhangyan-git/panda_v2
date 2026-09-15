package model

import "time"

// MessageInbox 对应 message_inbox 表，抽奖服务已接收消息事件及消费租约。
//
// 本轮抽奖服务**不消费任何事件**，所以这张表是空的：退款追回不做（account-service 自己
// 到今天也没有「退款成功后的追回」，lottery 不该做第一个扣这条线的人），参与是同步 HTTP，
// 开奖是进程内 worker。但表跟着库一起建好了——服务订阅者落地时不用再补一次迁移。留着
// 不用的代价是一张空表，不留的代价是加订阅者时要动 schema。
//
// 将来要接的第一条事件是 order.after_sale.refunded（见 migrations/lottery/001 的边界
// 说明与 service 包注释里那条已经写下的规则）。
type MessageInbox struct {
	EventID     string     `db:"event_id"`
	ClaimedAt   time.Time  `db:"claimed_at"`
	LeaseOwner  *string    `db:"lease_owner"`
	LeaseToken  *string    `db:"lease_token"`
	LeaseUntil  *time.Time `db:"lease_until"`
	CompletedAt *time.Time `db:"completed_at"`
}
