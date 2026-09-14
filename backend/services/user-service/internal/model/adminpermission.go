package model

import "time"

// AdminPermission 对应 admin_permissions 表
type AdminPermission struct {
	ID          string    `db:"id"`
	Code        string    `db:"code"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	PermGroup   string    `db:"perm_group"`
	CreatedAt   time.Time `db:"created_at"`
}
