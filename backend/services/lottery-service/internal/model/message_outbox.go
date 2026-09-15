package model

import "time"

// MessageOutbox 对应 message_outbox 表，抽奖服务待发布消息事件。
//
// 与 identity/003、merchant/002、coupon/001、coffee_machine/001、order/001、payment/001、
// account/001 里的同名表逐列一致，各库自带一份，跨库不共享表。八份拷贝由
// migrations 包的 TestMessageTablesStayInSyncAcrossSets 按列序比对，不能加列不能换序。
//
// 对抽奖域它不是可选件：lottery.round.drawn 与开奖记录必须在同一个事务里追加，
// relay 才不可能漏投一条已经生效的开奖；后台的人工开奖与期次作废还要在同一个事务里追加
// 一条 admin.operation.logged（platform/audit 的写法），由 relay 投到身份库的
// admin_operation_logs——**本库不建自己的审计表**。
type MessageOutbox struct {
	EventID       string     `db:"event_id"`
	EventType     string     `db:"event_type"`
	EventVersion  string     `db:"event_version"`
	TraceID       string     `db:"trace_id"`
	Payload       []byte     `db:"payload"`
	Attempts      int        `db:"attempts"`
	NextAttemptAt time.Time  `db:"next_attempt_at"`
	PublishedAt   *time.Time `db:"published_at"`
	LeaseOwner    *string    `db:"lease_owner"`
	LeaseToken    *string    `db:"lease_token"`
	LeaseUntil    *time.Time `db:"lease_until"`
	LastError     *string    `db:"last_error"`
	CreatedAt     time.Time  `db:"created_at"`
}
