package model

import "time"

// IdempotencyKey 对应 coupon_idempotency_keys 表，优惠券操作幂等请求记录。
type IdempotencyKey struct {
	ID           string     `db:"id"`
	Scope        string     `db:"scope"`
	Key          string     `db:"idempotency_key"`
	RequestHash  string     `db:"request_hash"`
	ResourceType string     `db:"resource_type"`
	ResourceID   *string    `db:"resource_id"`
	Response     []byte     `db:"response"`
	Status       string     `db:"status"`
	ExpiresAt    *time.Time `db:"expires_at"`
	CreatedAt    time.Time  `db:"created_at"`
}
