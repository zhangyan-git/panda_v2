package dto

import "time"

type CouponTypeRequest struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
}
type CouponTypeResponse struct {
	ID          string    `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
type TemplateRequest struct {
	CouponTypeID       string  `json:"couponTypeId"`
	MerchantID         *string `json:"merchantId"`
	Name               string  `json:"name"`
	ShortTitle         string  `json:"shortTitle"`
	Description        string  `json:"description"`
	CoverImage         string  `json:"coverImage"`
	UseRuleDescription string  `json:"useRuleDescription"`
	// 金额一律是「分」的整数。传字符串或小数会在 decodeBody 的 json.Unmarshal
	// 阶段就被拒（400），不会走到 service——这也是这版不再需要字符串校验的原因。
	FaceValue           int64      `json:"faceValue"`
	MinPurchaseAmount   int64      `json:"minPurchaseAmount"`
	PurchasePrice       int64      `json:"purchasePrice"`
	TotalQuantity       int64      `json:"totalQuantity"`
	ValidityMode        string     `json:"validityMode"`
	ValidFrom           *time.Time `json:"validFrom"`
	ValidTo             *time.Time `json:"validTo"`
	ValidDays           *int       `json:"validDays"`
	ClaimLimitMode      string     `json:"claimLimitMode"`
	ClaimPeriodUnit     *string    `json:"claimPeriodUnit"`
	ClaimPeriodQuantity *int       `json:"claimPeriodQuantity"`
	RedemptionType      string     `json:"redemptionType"`
	ExternalUseMethod   *string    `json:"externalUseMethod"`
	IsHot               bool       `json:"isHot"`
	IsRecommended       bool       `json:"isRecommended"`
	SortOrder           int        `json:"sortOrder"`
	Visible             bool       `json:"visible"`
	// 适用范围的品牌/门店两层。「商户」那层不在这里：它是模板上的 merchantId
	// （单选，NULL = 平台券），scope 表只承担品牌/门店。
	BrandIDs []string `json:"brandIds"`
	StoreIDs []string `json:"storeIds"`
}
type AuditRequest struct {
	Status string `json:"status"`
	Remark string `json:"remark"`
}
type StatusRequest struct {
	Status string `json:"status"`
}
