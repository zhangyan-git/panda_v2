package dto

import "time"

type UserCouponQuery struct {
	Page       int
	PageSize   int
	UserID     string
	Status     string
	BatchID    string
	TemplateID string
}
type UserCouponStats struct {
	Total       int64 `json:"total"`
	Claimed     int64 `json:"claimed"`
	Held        int64 `json:"held"`
	Redeemed    int64 `json:"redeemed"`
	Expired     int64 `json:"expired"`
	Refunded    int64 `json:"refunded"`
	Invalidated int64 `json:"invalidated"`
}
type RedeemCouponRequest struct {
	Reason string `json:"reason"`
}
type RevokeCouponRequest struct {
	Reason string `json:"reason"`
}
type UserCouponDetail struct {
	ID                string     `json:"id"`
	TemplateID        string     `json:"templateId"`
	BatchID           *string    `json:"batchId"`
	UserID            string     `json:"userId"`
	CouponTypeCode    string     `json:"couponTypeCode"`
	ClaimType         string     `json:"claimType"`
	IssueReason       string     `json:"issueReason"`
	Status            string     `json:"status"`
	FaceValue         int64      `json:"faceValue"`
	MinPurchaseAmount int64      `json:"minPurchaseAmount"`
	RedemptionType    string     `json:"redemptionType"`
	ValidFrom         time.Time  `json:"validFrom"`
	ExpiredAt         time.Time  `json:"expiredAt"`
	ClaimedAt         time.Time  `json:"claimedAt"`
	HeldAt            *time.Time `json:"heldAt"`
	RedeemedAt        *time.Time `json:"redeemedAt"`
	RefundedAt        *time.Time `json:"refundedAt"`
	InvalidatedAt     *time.Time `json:"invalidatedAt"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}
