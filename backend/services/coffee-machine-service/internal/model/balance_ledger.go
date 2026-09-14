package model

import "time"

// DeviceBalanceEntry 是 device_balance_ledger 的一行。
//
// 这张表只增不改不删（BEFORE UPDATE/DELETE 触发器直接报错），冲正也不是把原行改掉，
// 而是再写一条反向流水并指向被冲正的那条（ReversesEntryID）。所以读出来的是一个事件
// 序列，不是「每台设备一行当前状态」。
//
// 表里没有 balance_before 这一列：只存 balance_after，变动前的余额由 balance_after
// 减去 amount 推出来（见 dto.DeviceBalanceEntrySummary）。
type DeviceBalanceEntry struct {
	ID       string `db:"id"`
	DeviceID string `db:"device_id"`
	// Type 取值 recharge / deduct / adjust / reverse，由数据库 CHECK 限定。
	Type string `db:"type"`
	// Amount 有符号：充值为正、扣减为负，且不为 0（数据库 CHECK）。
	Amount int64 `db:"amount"`
	// BalanceAfter 是这次变动**之后**的余额，不是增量。
	BalanceAfter int64 `db:"balance_after"`
	// ReversesEntryID 只在 Type = reverse 时有值，指向被冲正的那条流水。
	ReversesEntryID *string `db:"reverses_entry_id"`
	// ReferenceType / ReferenceID 指向这次变动的来源单据（提货单等）。后台调整不走
	// 这条路，两个都是空。
	ReferenceType string  `db:"reference_type"`
	ReferenceID   *string `db:"reference_id"`
	// RequestID 是后台调整的幂等键：同一串重复提交只会留下一条流水。
	RequestID string `db:"request_id"`
	Remark    string `db:"remark"`
	// OperatorID 是操作人的管理员用户 ID。后台调整写的 operator_name 一律是空串，
	// 展示名由读侧按 id 解析（理由见 repository.AdjustBalance）。
	OperatorID   *string   `db:"operator_id"`
	OperatorName string    `db:"operator_name"`
	CreatedAt    time.Time `db:"created_at"`
}
