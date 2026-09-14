package model

import "time"

// UserLoginEvent 对应 user_login_events 表，一次登录尝试的审计记录。
//
// 成功和失败都记：单看成功记录看不出「有人在拿某个手机号反复试验证码」，
// 而那正是这条记录存在的理由。
//
// UserID 是软指针，库里没有外键，理由与 admin_operation_logs 相同：用户注销后
// 这些记录仍要能查，外键的 ON DELETE 会把它们连带删掉，审计恰好断在最需要它的
// 时候。登录失败时它为空——那时还没确定是谁。
type UserLoginEvent struct {
	ID     string `db:"id"`
	UserID string `db:"user_id"`
	// LoginType 取值同 model.LoginType*，库里有 CHECK 约束。
	LoginType string `db:"login_type"`
	// Identifier 是这次尝试凭据的标识：openid 或手机号。
	// 手机号应写入脱敏形式——这张表是给运维和安全查的，不是客服工具。
	Identifier string    `db:"identifier"`
	Success    bool      `db:"success"`
	FailReason string    `db:"fail_reason"`
	IP         string    `db:"ip"`
	UserAgent  string    `db:"user_agent"`
	CreatedAt  time.Time `db:"created_at"`
}
