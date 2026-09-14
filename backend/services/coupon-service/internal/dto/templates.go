package dto

import "time"

// CouponTemplateQuery 是后台模板列表的筛选条件，形状与 CouponBatchQuery / UserCouponQuery
// 一致：空串 = 这一条不筛。
//
// name 是模糊匹配（后台按名字找模板是第一位的用法）；status / auditStatus 是精确匹配，
// 取值就是 001 迁移里 coupon_templates 那两条 CHECK 的枚举。这里不校验枚举合法性，
// 与 ListBatches 对 status/source 的处理保持一致：传了库里没有的值只会查出 0 行。
type CouponTemplateQuery struct {
	Page        int
	PageSize    int
	Name        string
	Status      string
	AuditStatus string
}

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
