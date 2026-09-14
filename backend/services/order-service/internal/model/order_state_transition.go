package model

import (
	"encoding/json"
	"time"
)

// OrderStateTransition 对应 order_state_transitions 表，订单与售后的状态流水。
//
// 只追加，不改不删（order_state_transitions_append_only 触发器兜底）。订单跨越多个
// 请求、事件和补偿任务，只靠主表上的当前状态查不出「怎么走到这一步的」。
type OrderStateTransition struct {
	ID string `db:"id"`
	// order / order_line / after_sale / payment_line。
	AggregateType string `db:"aggregate_type"`
	AggregateID   string `db:"aggregate_id"`
	FromStatus    string `db:"from_status"`
	ToStatus      string `db:"to_status"`
	Reason        string `db:"reason"`
	RequestID     string `db:"request_id"`
	// 谁推动了这次变更。admin/merchant 的操作同时会在身份库 admin_operation_logs 留一条
	// 后台操作审计，这里记的是订单自己的口径。
	ActorType string          `db:"actor_type"`
	ActorID   *string         `db:"actor_id"`
	Metadata  json.RawMessage `db:"metadata"`
	CreatedAt time.Time       `db:"created_at"`
}

// 状态流水的聚合类型与操作者类型，与迁移里的 CHECK 逐字一致。
const (
	AggregateOrder       = "order"
	AggregateOrderLine   = "order_line"
	AggregateAfterSale   = "after_sale"
	AggregatePaymentLine = "payment_line"
)

const (
	ActorUser     = "user"
	ActorMerchant = "merchant"
	ActorAdmin    = "admin"
	ActorSystem   = "system"
)
