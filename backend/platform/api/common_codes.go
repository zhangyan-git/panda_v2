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
	CodeInternal         = "INTERNAL_ERROR"
	CodeUnavailable      = "SERVICE_UNAVAILABLE"
	CodeTimeout          = "TIMEOUT"
	CodeNotImplemented   = "NOT_IMPLEMENTED"
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
