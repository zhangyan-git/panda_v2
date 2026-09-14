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
