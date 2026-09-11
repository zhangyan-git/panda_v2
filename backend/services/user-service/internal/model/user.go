package model

import "time"

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

// MerchantUser 对应 merchant_users 表，商户员工
type MerchantUser struct {
	ID           string     `db:"id"`
	MerchantID   string     `db:"merchant_id"`
	Username     string     `db:"username"`
	PasswordHash string     `db:"password_hash"`
	Name         string     `db:"name"`
	Email        string     `db:"email"`
	Phone        string     `db:"phone"`
	Status       string     `db:"status"` // active / disabled
	IsAdmin      bool       `db:"is_admin"`
	ScopeType    string     `db:"scope_type"` // merchant / brand / store
	ScopeID      string     `db:"scope_id"`   // 范围目标 ID，scope_type=merchant 时为空
	Avatar       string     `db:"avatar"`
	LastLoginAt  *time.Time `db:"last_login_at"`
	LastLoginIP  string     `db:"last_login_ip"`
	LoginCount   int        `db:"login_count"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`

	// ScopeName 范围品牌/门店名称，仅展示用，不入库；brands/stores 在商户库，
	// 由 service 层经 gRPC 批量解析后填入
	ScopeName string `db:"scope_name"`
}
