package api

import "net/http"

const (
	CodeOK               = 0
	CodeInvalidRequest   = 40000
	CodeUnauthorized     = 40100
	CodeForbidden        = 40300
	CodeNotFound         = 40400
	CodeConflict         = 40900
	CodeMethodNotAllowed = 40500
	CodeInternal         = 50000
	CodeUnavailable      = 50300
	CodeTimeout          = 50400
	CodeNotImplemented   = 50100
)

func CodeForStatus(status int) int {
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
