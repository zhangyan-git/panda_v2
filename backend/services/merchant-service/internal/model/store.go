package model

import "time"

// Store 对应 stores 表，隶属商户与品牌的门店
type Store struct {
	ID         string   `db:"id"`
	MerchantID string   `db:"merchant_id"`
	BrandID    string   `db:"brand_id"`
	Name       string   `db:"name"`
	Logo       string   `db:"logo"`
	Photos     []string `db:"photos"`
	Province   string   `db:"province"`
	City       string   `db:"city"`
	District   string   `db:"district"`
	// 区划编码与名称并存：名称给人看，编码用来回填、校验与迁移（见 003 迁移的注释）。
	// 历史行为空，回填不上的也为空。
	ProvinceCode string `db:"province_code"`
	CityCode     string `db:"city_code"`
	DistrictCode string `db:"district_code"`
	// 订货系统 xlsx「客户」页的三列（merchant/004）。客户编码与 DMS 编码是跟供应商、
	// DMS 对账的业务键，各自带一条「空串不参与」的部分唯一索引；客户类型取值未定型，
	// 刻意不做 CHECK。三者都允许为空串——历史门店还没有编码。
	CustomerCode  string     `db:"customer_code"`
	DMSCode       string     `db:"dms_code"`
	CustomerType  string     `db:"customer_type"`
	Address       string     `db:"address"`
	Longitude     *float64   `db:"longitude"`
	Latitude      *float64   `db:"latitude"`
	Phone         string     `db:"phone"`
	ContactName   string     `db:"contact_name"`
	ContactPhone  string     `db:"contact_phone"`
	Detail        string     `db:"detail"`
	BusinessHours string     `db:"business_hours"`
	Status        string     `db:"status"`       // active / disabled
	AuditStatus   string     `db:"audit_status"` // pending / approved / rejected
	AuditRemark   string     `db:"audit_remark"`
	AuditAt       *time.Time `db:"audit_at"`
	AuditBy       string     `db:"audit_by"`
	Remark        string     `db:"remark"`
	Visible       bool       `db:"visible"`
	CreatedBy     string     `db:"created_by"`
	CreatedAt     time.Time  `db:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at"`

	// MerchantName / BrandName 联表计算列，仅列表/详情展示用，不入库
	MerchantName string `db:"merchant_name"`
	BrandName    string `db:"brand_name"`
}

// AuditRecord 对应 brand_audit_records / store_audit_records，一次提交一条
