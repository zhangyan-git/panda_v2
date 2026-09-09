package model

import "time"

// Merchant 对应 merchants 表，商户主体（多租户体系的租户）
type Merchant struct {
	ID           string    `db:"id"`
	Name         string    `db:"name"`
	Status       string    `db:"status"` // pending=待审核 / active=正常 / suspended=已暂停
	ContactName  string    `db:"contact_name"`
	ContactPhone string    `db:"contact_phone"`
	ContactEmail string    `db:"contact_email"`
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}
