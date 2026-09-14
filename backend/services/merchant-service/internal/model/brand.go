package model

import "time"

// Brand 对应 brands 表，隶属商户的品牌
type Brand struct {
	ID          string     `db:"id"`
	MerchantID  string     `db:"merchant_id"`
	Name        string     `db:"name"`
	Logo        string     `db:"logo"`
	Banner      string     `db:"banner"`
	Description string     `db:"description"`
	Status      string     `db:"status"`       // active / disabled
	AuditStatus string     `db:"audit_status"` // pending / approved / rejected
	AuditRemark string     `db:"audit_remark"`
	AuditAt     *time.Time `db:"audit_at"`
	AuditBy     string     `db:"audit_by"`
	Remark      string     `db:"remark"`
	Visible     bool       `db:"visible"`
	Sort        int        `db:"sort"`
	CreatedBy   string     `db:"created_by"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`

	// MerchantName 联表计算列（merchants.name），仅列表/详情展示用，不入库
	MerchantName string `db:"merchant_name"`
}

// Store 对应 stores 表，隶属商户与品牌的门店
