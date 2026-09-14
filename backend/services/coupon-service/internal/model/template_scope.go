package model

import "time"

// CouponTemplateScope 对应 coupon_template_scopes 表，优惠券模板适用范围。
type CouponTemplateScope struct {
	ID         string    `db:"id"`
	TemplateID string    `db:"template_id"`
	ScopeType  string    `db:"scope_type"`
	ScopeID    string    `db:"scope_id"`
	CreatedAt  time.Time `db:"created_at"`
}
