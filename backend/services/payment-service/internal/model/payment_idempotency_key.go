package model

import (
	"encoding/json"
	"time"
)

// PaymentIdempotencyKey 对应 payment_idempotency_keys 表，发起支付等操作的幂等请求记录。
//
// 与 order_idempotency_keys、coupon_idempotency_keys 逐列同形：同一套幂等语义在仓库里
// 只有一种表结构，换服务时不用重新理解一遍。
type PaymentIdempotencyKey struct {
	ID           string  `db:"id"`
	Scope        string  `db:"scope"`
	Key          string  `db:"idempotency_key"`
	RequestHash  string  `db:"request_hash"`
	ResourceType string  `db:"resource_type"`
	ResourceID   *string `db:"resource_id"`
	// 成功时的响应体：同一个幂等键重放直接回放它，不重新执行一遍副作用。
	Response  json.RawMessage `db:"response"`
	Status    string          `db:"status"`
	ExpiresAt *time.Time      `db:"expires_at"`
	CreatedAt time.Time       `db:"created_at"`
}

// 幂等记录状态，与 payment_idempotency_keys.status 的 CHECK 逐字一致。
const (
	IdempotencyProcessing = "processing"
	IdempotencySucceeded  = "succeeded"
	IdempotencyFailed     = "failed"
)
