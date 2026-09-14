package model

import "time"

// UserCouponScope 对应 user_coupon_scopes 表，用户券适用范围快照。
type UserCouponScope struct {
	UserCouponID string    `db:"user_coupon_id"`
	ScopeType    string    `db:"scope_type"`
	ScopeID      string    `db:"scope_id"`
	CreatedAt    time.Time `db:"created_at"`
}
