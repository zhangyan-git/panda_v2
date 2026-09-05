package model

import (
	"context"
	"time"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

type AuditStatus string

const (
	AuditPending  AuditStatus = "pending"
	AuditApproved AuditStatus = "approved"
	AuditRejected AuditStatus = "rejected"
)

type AuditType string

const (
	AuditCreate AuditType = "create"
	AuditUpdate AuditType = "update"
)

type PermissionType string

const (
	PermissionAll   PermissionType = "all"
	PermissionBrand PermissionType = "brand"
	PermissionStore PermissionType = "store"
)

type Merchant struct {
	ID, Name, ContactName, ContactPhone, BusinessLicense, Address string
	Status                                                        Status
	ExpireDate                                                    *time.Time
	CreatedAt, UpdatedAt                                          time.Time
}
type Store struct {
	ID, MerchantID, BrandID, Name, Logo, Phone, Province, City, District, Address, BusinessHours string
	Longitude, Latitude                                                                          float64
	Status                                                                                       Status
	AuditStatus                                                                                  AuditStatus
	AuditRemark                                                                                  string
	Visible                                                                                      bool
	CreatedAt, UpdatedAt                                                                         time.Time
}
type MerchantAccount struct {
	ID, MerchantID, AccountID, RealName string
	IsAdmin                             bool
	PermissionType                      PermissionType
	BrandIDs, StoreIDs                  []string
	CreatedAt, UpdatedAt                time.Time
}
type StoreAuditRecord struct {
	ID, StoreID                         string
	Type                                AuditType
	Status                              AuditStatus
	NewData, OldData                    []byte
	SubmittedBy, AuditedBy, AuditRemark string
	CreatedAt, UpdatedAt                time.Time
}

type Repository interface {
	CreateMerchant(context.Context, Merchant) (Merchant, error)
	GetMerchant(context.Context, string) (Merchant, error)
	ListMerchants(context.Context) ([]Merchant, error)
	UpdateMerchant(context.Context, string, Merchant) (Merchant, error)
	SetMerchantStatus(context.Context, string, Status) (Merchant, error)
	CreateStore(context.Context, Store) (Store, error)
	GetStore(context.Context, string) (Store, error)
	ListStoresByMerchant(context.Context, string) ([]Store, error)
	UpdateStore(context.Context, string, Store) (Store, error)
	SetStoreStatus(context.Context, string, Status) (Store, error)
	DeleteStore(context.Context, string) error
	CreateMerchantAccount(context.Context, MerchantAccount) (MerchantAccount, error)
	GetMerchantAccountByAccountID(context.Context, string) (MerchantAccount, error)
	UpdateMerchantAccount(context.Context, string, MerchantAccount) (MerchantAccount, error)
	CreateAudit(context.Context, StoreAuditRecord) (StoreAuditRecord, error)
	GetAudit(context.Context, string) (StoreAuditRecord, error)
	UpdateAudit(context.Context, string, StoreAuditRecord) (StoreAuditRecord, error)
	ListAuditsByStore(context.Context, string) ([]StoreAuditRecord, error)
}
