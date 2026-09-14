package api

import "net/http"

// 错误码使用字符串常量，便于前端直接 switch 处理。
const (
	CodeOK               = "OK"
	CodeInvalidRequest   = "INVALID_REQUEST"
	CodeUnauthorized     = "UNAUTHORIZED"
	CodeForbidden        = "FORBIDDEN"
	CodeNotFound         = "NOT_FOUND"
	CodeConflict         = "CONFLICT"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	// CodeTooManyRequests 是唯一一个「这个请求本身没错，只是现在不能做」的错误码。
	// 客户端据此知道要等，而不是去改请求参数或提示用户检查输入。
	CodeTooManyRequests = "TOO_MANY_REQUESTS"
	CodeInternal        = "INTERNAL_ERROR"
	CodeUnavailable     = "SERVICE_UNAVAILABLE"
	CodeTimeout         = "TIMEOUT"
	CodeNotImplemented  = "NOT_IMPLEMENTED"
)

// CodeForStatus 根据 HTTP 状态码返回对应的业务错误码字符串。
func CodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalidRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusMethodNotAllowed:
		return CodeMethodNotAllowed
	case http.StatusTooManyRequests:
		return CodeTooManyRequests
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	case http.StatusGatewayTimeout:
		return CodeTimeout
	case http.StatusNotImplemented:
		return CodeNotImplemented
	default:
		return CodeInternal
	}
}
