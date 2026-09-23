package model

import "time"

// MessageOutbox 对应 message_outbox 表，会员服务待发布消息事件。
//
// 与其它九个库里的同名表逐列一致，各库自带一份，跨库不共享表。十份拷贝由 migrations
// 包的 TestMessageTablesStayInSyncAcrossSets 按列序比对，不能加列不能换序。
//
// 对会员域它不是可选件：会员变更是**对外的事实源**，本身没有数据库外键能指过来，别的服务
// 只能靠事件知道「这个人现在是不是会员」。而变更流水是 append-only 的，写进去就撤不回，
// 所以「改了会员」与「告诉别人改了」必须同一次事务落地——否则一个开通了年度会员的人在
// 别处永远算不出会员价，而且没有任何补偿手段。
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
