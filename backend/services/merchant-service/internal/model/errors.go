package model

import "errors"

var (
	ErrNotFound          = errors.New("merchant resource not found")
	ErrInvalidMerchant   = errors.New("invalid merchant")
	ErrInvalidStore      = errors.New("invalid store")
	ErrInvalidAccount    = errors.New("invalid merchant account")
	ErrInvalidPermission = errors.New("invalid permission scope")
	ErrInvalidAudit      = errors.New("invalid audit request")
	ErrInvalidStatus     = errors.New("invalid status")
	ErrForbidden         = errors.New("forbidden")
	ErrConflict          = errors.New("resource conflict")
	ErrStoreMerchant     = errors.New("store merchant cannot be changed")
)
