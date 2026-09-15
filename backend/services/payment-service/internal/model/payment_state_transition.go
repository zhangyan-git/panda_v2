package model

import (
	"encoding/json"
	"time"
)

// PaymentStateTransition 对应 payment_state_transitions 表，与 order_state_transitions 逐列同形。
//
// 支付单、出资、退款、签约、扣款、对账都跨多个请求与回调，只看当前状态查不出「怎么走到这
// 一步的」。这张表只追加，不改不删——它是排查「这笔钱为什么是这个状态」的唯一依据。
//
// 注意它与 payment_transactions 的区别：那张记的是**钱**的进出（对账基准），这张记的是
// **状态**的变化（排查依据）。一次失败的重试在这里有一行、在流水里没有，这正是要的。
type PaymentStateTransition struct {
	ID string `db:"id"`
	// payment / funding / refund / agreement / charge / reconciliation。
	AggregateType string `db:"aggregate_type"`
	AggregateID   string `db:"aggregate_id"`
	FromStatus    string `db:"from_status"`
	ToStatus      string `db:"to_status"`
	Reason        string `db:"reason"`
	RequestID     string `db:"request_id"`
	// user / merchant / admin / system。admin 的人工退款/关单同时会在身份库
	// admin_operation_logs 留一条后台操作审计，这里记的是支付自己的口径。
	ActorType string          `db:"actor_type"`
	ActorID   *string         `db:"actor_id"`
	Metadata  json.RawMessage `db:"metadata"`
	CreatedAt time.Time       `db:"created_at"`
}

// 状态流水的聚合类型，与 payment_state_transitions.aggregate_type 的 CHECK 逐字一致。
const (
	AggregatePayment = "payment"
	AggregateFunding = "funding"
	AggregateRefund  = "refund"
)

// 动作人类型，与 payment_state_transitions.actor_type 的 CHECK 逐字一致。
//
// 本轮发起支付由 order-service 代用户触发、回调来自渠道，两者都记 ActorSystem：
// 它们都不是「有人在支付服务里做了这个动作」——真正的人在做那一步时已经在订单侧留了痕。
const (
	ActorUser     = "user"
	ActorMerchant = "merchant"
	ActorAdmin    = "admin"
	ActorSystem   = "system"
)
