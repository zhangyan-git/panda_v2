package dto

import "time"

// CouponTemplateResponse 是优惠券模板对外的 JSON 形态。model 只带 db tag，
// 直接序列化会输出 PascalCase，前端按下划线取值会全部落空。
type CouponTemplateResponse struct {
	ID                 string  `json:"id"`
	CouponTypeID       string  `json:"couponTypeId"`
	MerchantID         *string `json:"merchantId,omitempty"`
	Name               string  `json:"name"`
	ShortTitle         string  `json:"shortTitle"`
	Description        string  `json:"description"`
	CoverImage         string  `json:"coverImage"`
	UseRuleDescription string  `json:"useRuleDescription"`
	// 金额单位是「分」，JSON 里就是整数。
	FaceValue           int64      `json:"faceValue"`
	MinPurchaseAmount   int64      `json:"minPurchaseAmount"`
	PurchasePrice       int64      `json:"purchasePrice"`
	TotalQuantity       int64      `json:"totalQuantity"`
	IssuedQuantity      int64      `json:"issuedQuantity"`
	ReservedQuantity    int64      `json:"reservedQuantity"`
	ValidityMode        string     `json:"validityMode"`
	ValidFrom           *time.Time `json:"validFrom,omitempty"`
	ValidTo             *time.Time `json:"validTo,omitempty"`
	ValidDays           *int       `json:"validDays,omitempty"`
	ClaimLimitMode      string     `json:"claimLimitMode"`
	ClaimPeriodUnit     *string    `json:"claimPeriodUnit,omitempty"`
	ClaimPeriodQuantity *int       `json:"claimPeriodQuantity,omitempty"`
	RedemptionType      string     `json:"redemptionType"`
	ExternalUseMethod   *string    `json:"externalUseMethod,omitempty"`
	AuditStatus         string     `json:"auditStatus"`
	AuditRemark         string     `json:"auditRemark"`
	AuditedAt           *time.Time `json:"auditedAt,omitempty"`
	AuditedBy           *string    `json:"auditedBy,omitempty"`
	Status              string     `json:"status"`
	IsHot               bool       `json:"isHot"`
	IsRecommended       bool       `json:"isRecommended"`
	SortOrder           int        `json:"sortOrder"`
	Visible             bool       `json:"visible"`
	CreatedBy           *string    `json:"createdBy,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
	// 适用范围。不带 omitempty：空范围要序列化成 []，前端拿它判断「不限」；
	// nil 切片会序列化成 null，前端得多写一层兜底。
	BrandIDs []string `json:"brandIds"`
	StoreIDs []string `json:"storeIds"`
}
