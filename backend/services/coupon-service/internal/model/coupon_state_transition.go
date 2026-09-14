package model

import (
	"encoding/json"
	"time"
)

// CouponStateTransition 对应 coupon_state_transitions 表，优惠券状态变更审计记录。
type CouponStateTransition struct {
	ID            string          `db:"id"`
	AggregateType string          `db:"aggregate_type"`
	AggregateID   string          `db:"aggregate_id"`
	FromStatus    string          `db:"from_status"`
	ToStatus      string          `db:"to_status"`
	Reason        string          `db:"reason"`
	RequestID     string          `db:"request_id"`
	ActorID       *string         `db:"actor_id"`
	Metadata      json.RawMessage `db:"metadata"`
	CreatedAt     time.Time       `db:"created_at"`
}
