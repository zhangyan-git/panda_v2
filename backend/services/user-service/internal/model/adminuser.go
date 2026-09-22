package model

import (
	"errors"
	"time"
)

// AdminUser 对应 admin_users 表，平台后台管理员
type AdminUser struct {
	ID           string    `db:"id"`
	Username     string    `db:"username"`
	PasswordHash string    `db:"password_hash"`
	Name         string    `db:"name"`
	Email        string    `db:"email"`
	Status       string    `db:"status"` // active / disabled
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}

// 唯一键冲突的哨兵错误：repository 把 SQLSTATE 23505 翻译成它们，service 再
// 原样上抛，handler 据此回 409 而不是 500。没有它们，撞一次用户名重名在后台
// 看起来就是「服务内部错误」。
var (
	// ErrAdminUsernameTaken 用户名已被占用（admin_users.username 唯一）
	ErrAdminUsernameTaken = errors.New("用户名已存在")
	// ErrAdminEmailTaken 邮箱已被占用（admin_users.email 唯一）
	ErrAdminEmailTaken = errors.New("邮箱已存在")
)

// MerchantUser 对应 merchant_users 表，商户员工
