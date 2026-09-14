package model

import "time"

// CouponBatch 对应 coupon_batches 表，优惠券发行批次及库存。
type CouponBatch struct {
	ID               string    `db:"id"`
	TemplateID       string    `db:"template_id"`
	BatchNo          string    `db:"batch_no"`
	Source           string    `db:"source"`
	TotalQuantity    int64     `db:"total_quantity"`
	ReservedQuantity int64     `db:"reserved_quantity"`
	IssuedQuantity   int64     `db:"issued_quantity"`
	ReleasedQuantity int64     `db:"released_quantity"`
	Status           string    `db:"status"`
	OrderID          *string   `db:"order_id"`
	UserID           *string   `db:"user_id"`
	RequestID        *string   `db:"request_id"`
	CreatedBy        *string   `db:"created_by"`
	CreatedAt        time.Time `db:"created_at"`
	UpdatedAt        time.Time `db:"updated_at"`
}
