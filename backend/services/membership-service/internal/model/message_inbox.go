package model

import "time"

// MessageInbox 对应 message_inbox 表，会员服务已接收消息事件及消费租约。
//
// 与上面 MessageOutbox 同一份约定：九份拷贝逐列一致，由 migrations 的
// TestMessageTablesStayInSyncAcrossSets 按列序比对。
//
// 本库有真正的写入方：`order.paid` 是会员开通与续费的**唯一**触发源（买会员是
// order-service 里的一个订单行，本库没有订单表，也就没有本地事务能替代它）。
// 而支付成功回调会被重放——老系统同一个回调重试三四次是常事。inbox 的
// event_id 主键加上 membership_changes_order_unique，是这条链路上唯一能保证
// 「重放一次不会续两次」的东西，两层都不能省。
type MessageInbox struct {
	EventID     string     `db:"event_id"`
	ClaimedAt   time.Time  `db:"claimed_at"`
	LeaseOwner  *string    `db:"lease_owner"`
	LeaseToken  *string    `db:"lease_token"`
	LeaseUntil  *time.Time `db:"lease_until"`
	CompletedAt *time.Time `db:"completed_at"`
}
