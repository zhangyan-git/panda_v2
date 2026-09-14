package model

import "time"

// UserCoupon 对应 user_coupons 表，用户持有的优惠券实例及规则快照。
type UserCoupon struct {
	ID                   string  `db:"id"`
	TemplateID           string  `db:"template_id"`
	BatchID              *string `db:"batch_id"`
	UserID               string  `db:"user_id"`
	CouponTypeCode       string  `db:"coupon_type_code"`
	ClaimType            string  `db:"claim_type"`
	IssueReason          string  `db:"issue_reason"`
	CampaignClaimID      *string `db:"campaign_claim_id"`
	GrantSequence        int     `db:"grant_sequence"`
	Status               string  `db:"status"`
	RedemptionCodeDigest *string `db:"redemption_code_digest"`
	// 发券时从模板快照过来的金额，单位同样是「分」。
	FaceValue         int64      `db:"face_value"`
	MinPurchaseAmount int64      `db:"min_purchase_amount"`
	RedemptionType    string     `db:"redemption_type"`
	ValidFrom         time.Time  `db:"valid_from"`
	ExpiredAt         time.Time  `db:"expired_at"`
	ClaimedAt         time.Time  `db:"claimed_at"`
	HeldAt            *time.Time `db:"held_at"`
	RedeemedAt        *time.Time `db:"redeemed_at"`
	RefundedAt        *time.Time `db:"refunded_at"`
	InvalidatedAt     *time.Time `db:"invalidated_at"`
	CreatedAt         time.Time  `db:"created_at"`
	UpdatedAt         time.Time  `db:"updated_at"`
}
