package dto

import "time"

type CouponBatchQuery struct {
	Page       int
	PageSize   int
	TemplateID string
	Status     string
	Source     string
	BatchNo    string
}

type CouponBatchResponse struct {
	ID               string    `json:"id"`
	TemplateID       string    `json:"templateId"`
	BatchNo          string    `json:"batchNo"`
	Source           string    `json:"source"`
	TotalQuantity    int64     `json:"totalQuantity"`
	ReservedQuantity int64     `json:"reservedQuantity"`
	IssuedQuantity   int64     `json:"issuedQuantity"`
	ReleasedQuantity int64     `json:"releasedQuantity"`
	Status           string    `json:"status"`
	OrderID          *string   `json:"orderId,omitempty"`
	UserID           *string   `json:"userId,omitempty"`
	RequestID        *string   `json:"requestId,omitempty"`
	CreatedBy        *string   `json:"createdBy,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type CouponBatchStats struct {
	Total    int64 `json:"total"`
	Claimed  int64 `json:"claimed"`
	Redeemed int64 `json:"redeemed"`
	Expired  int64 `json:"expired"`
	Refunded int64 `json:"refunded"`
}
