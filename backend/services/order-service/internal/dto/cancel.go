package dto

// CancelOrderRequest 是取消订单的请求体。
//
// 没有「取消人」字段：取消人一律取自令牌（用户取消是自己，后台取消是当前管理员），
// 让调用方自报身份就等于让被审计的人填审计字段。
type CancelOrderRequest struct {
	Reason string `json:"reason"`
}
