package model

import "time"

// CouponRedemption 对应 coupon_redemptions 表，优惠券核销记录及金额快照。
type CouponRedemption struct {
	ID               string  `db:"id"`
	UserCouponID     string  `db:"user_coupon_id"`
	TemplateID       string  `db:"template_id"`
	RequestID        string  `db:"request_id"`
	RedemptionMethod string  `db:"redemption_method"`
	StoreID          *string `db:"store_id"`
	EmployeeID       *string `db:"employee_id"`
	OrderID          *string `db:"order_id"`
	// 三个金额都是「分」。
	AmountBefore   *int64     `db:"amount_before"`
	DiscountAmount *int64     `db:"discount_amount"`
	AmountAfter    *int64     `db:"amount_after"`
	Status         string     `db:"status"`
	FailureCode    string     `db:"failure_code"`
	CreatedAt      time.Time  `db:"created_at"`
	CompletedAt    *time.Time `db:"completed_at"`
	ReversedAt     *time.Time `db:"reversed_at"`
}
