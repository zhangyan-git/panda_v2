package model

import "time"

// AdminRole 对应 admin_roles 表
type AdminRole struct {
	ID          string    `db:"id"`
	Code        string    `db:"code"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

// AdminPermission 对应 admin_permissions 表
