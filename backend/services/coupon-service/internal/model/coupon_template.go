package model

import "time"

// CouponTemplate 对应 coupon_templates 表，优惠券模板及规则。
type CouponTemplate struct {
	ID                 string  `db:"id"`
	CouponTypeID       string  `db:"coupon_type_id"`
	MerchantID         *string `db:"merchant_id"`
	Name               string  `db:"name"`
	ShortTitle         string  `db:"short_title"`
	Description        string  `db:"description"`
	CoverImage         string  `db:"cover_image"`
	UseRuleDescription string  `db:"use_rule_description"`
	// 三个金额都是「分」，int64 直存 BIGINT。全 V2 只有这一个单位约定，
	// 没有「元」的定点字符串那套了。
	FaceValue           int64      `db:"face_value"`
	MinPurchaseAmount   int64      `db:"min_purchase_amount"`
	PurchasePrice       int64      `db:"purchase_price"`
	TotalQuantity       int64      `db:"total_quantity"`
	IssuedQuantity      int64      `db:"issued_quantity"`
	ReservedQuantity    int64      `db:"reserved_quantity"`
	ValidityMode        string     `db:"validity_mode"`
	ValidFrom           *time.Time `db:"valid_from"`
	ValidTo             *time.Time `db:"valid_to"`
	ValidDays           *int       `db:"valid_days"`
	ClaimLimitMode      string     `db:"claim_limit_mode"`
	ClaimPeriodUnit     *string    `db:"claim_period_unit"`
	ClaimPeriodQuantity *int       `db:"claim_period_quantity"`
	RedemptionType      string     `db:"redemption_type"`
	ExternalUseMethod   *string    `db:"external_use_method"`
	AuditStatus         string     `db:"audit_status"`
	AuditRemark         string     `db:"audit_remark"`
	AuditedAt           *time.Time `db:"audited_at"`
	AuditedBy           *string    `db:"audited_by"`
	Status              string     `db:"status"`
	IsHot               bool       `db:"is_hot"`
	IsRecommended       bool       `db:"is_recommended"`
	SortOrder           int        `db:"sort_order"`
	Visible             bool       `db:"visible"`
	CreatedBy           *string    `db:"created_by"`
	CreatedAt           time.Time  `db:"created_at"`
	UpdatedAt           time.Time  `db:"updated_at"`

	// Scopes 是 coupon_template_scopes 里的品牌/门店范围，不在这张表上，由
	// repository 单独查出来挂上（见 template.go 的 attachScopes）。因此它绝不能进
	// scanTemplates/scanTemplateRow 那两串位置化的 Scan 列表，否则整行错位。
	// 空切片表示不限。
	Scopes []*CouponTemplateScope `db:"-"`
}
