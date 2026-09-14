package model

import "time"

// CouponInventoryLedger 对应 coupon_inventory_ledger 表，优惠券库存变动流水。
type CouponInventoryLedger struct {
	ID            string    `db:"id"`
	TemplateID    string    `db:"template_id"`
	BatchID       *string   `db:"batch_id"`
	ReferenceType string    `db:"reference_type"`
	ReferenceID   *string   `db:"reference_id"`
	Quantity      int64     `db:"quantity"`
	Operation     string    `db:"operation"`
	RequestID     string    `db:"request_id"`
	CreatedAt     time.Time `db:"created_at"`
}
