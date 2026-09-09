package operationlog

import (
	"context"
	"time"
)

// Record 是一次平台后台业务操作的审计记录。
type Record struct {
	ID            string         `json:"id"`
	AdminUserID   string         `json:"adminUserId"`
	AdminUsername string         `json:"adminUsername"`
	AdminName     string         `json:"adminName"`
	Module        string         `json:"module"`
	Action        string         `json:"action"`
	Operation     string         `json:"operation"`
	TargetType    string         `json:"targetType"`
	TargetID      string         `json:"targetId"`
	TargetName    string         `json:"targetName"`
	MerchantID    string         `json:"merchantId"`
	Result        string         `json:"result"`
	ErrorCode     string         `json:"errorCode"`
	ErrorMessage  string         `json:"errorMessage"`
	BeforeData    map[string]any `json:"beforeData,omitempty"`
	AfterData     map[string]any `json:"afterData,omitempty"`
	OccurredAt    time.Time      `json:"occurredAt"`
}

type Query struct {
	AdminUserID string
	Module      string
	Action      string
	TargetType  string
	Result      string
	MerchantID  string
	Page        int
	PageSize    int
}

type Repository interface {
	Create(context.Context, *Record) error
	FindAll(context.Context, Query) ([]*Record, int, error)
}
