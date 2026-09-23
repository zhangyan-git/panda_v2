package model

import "time"

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
	ScopeIDs     []string   `db:"scope_ids"`  // 范围目标 ID 集合，scope_type=merchant 时为空（不是 nil）
	Avatar       string     `db:"avatar"`
	LastLoginAt  *time.Time `db:"last_login_at"`
	LastLoginIP  string     `db:"last_login_ip"`
	LoginCount   int        `db:"login_count"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`

	// ScopeNames 范围品牌/门店名称，**与 ScopeIDs 一一对应同序**，仅展示用，不入库；
	// brands/stores 在商户库，由 service 层经 gRPC 批量解析后填入
	ScopeNames []string `db:"scope_names"`
}
