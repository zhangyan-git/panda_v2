package dto

// IssueCouponsRequest is the administrative batch issue request.
//
// 这两个结构体的 tag 是对外 JSON 契约（camelCase），但 IssueCouponsRequest 的
// 字段名和顺序**同时**是幂等哈希的输入：service 包里另有一份冻结的结构体
// （issueHashPayload）按旧 snake_case tag 渲染同一组字段，好让线上已建好的
// Idempotency-Key 在改名后仍然命中。改这里不影响哈希，别把两处合并。
type IssueCouponsRequest struct {
	TemplateID          string   `json:"templateId"`
	UserIDs             []string `json:"userIds"`
	QuantityPerUser     int      `json:"quantityPerUser"`
	Reason              string   `json:"reason"`
	SkipLimitValidation bool     `json:"skipLimitValidation"`
}
type IssueCouponsResponse struct {
	BatchID        string   `json:"batchId"`
	IssuedQuantity int      `json:"issuedQuantity"`
	UserCouponIDs  []string `json:"userCouponIds"`
}
