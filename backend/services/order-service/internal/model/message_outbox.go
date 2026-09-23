package model

import "time"

// MessageOutbox 对应 message_outbox 表，订单服务待发布消息事件。
//
// 与其它九个库里的同名表逐列一致，各库自带一份，跨库不共享表。十份拷贝由 migrations
// 包的 TestMessageTablesStayInSyncAcrossSets 按列序比对，不能加列不能换序。
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
