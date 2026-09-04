package domain

import (
	"context"
	"encoding/json"
	"strings"
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

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }
func (s *Service) CreateMerchant(ctx context.Context, m Merchant) (Merchant, error) {
	if strings.TrimSpace(m.Name) == "" {
		return Merchant{}, ErrInvalidMerchant
	}
	if m.Status == "" {
		m.Status = StatusActive
	}
	if !validStatus(m.Status) {
		return Merchant{}, ErrInvalidStatus
	}
	return s.repo.CreateMerchant(ctx, m)
}
func (s *Service) GetMerchant(ctx context.Context, id string) (Merchant, error) {
	if strings.TrimSpace(id) == "" {
		return Merchant{}, ErrInvalidMerchant
	}
	return s.repo.GetMerchant(ctx, id)
}
func (s *Service) ListMerchants(ctx context.Context) ([]Merchant, error) {
	return s.repo.ListMerchants(ctx)
}
func (s *Service) UpdateMerchant(ctx context.Context, id string, m Merchant) (Merchant, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(m.Name) == "" {
		return Merchant{}, ErrInvalidMerchant
	}
	if m.Status != "" && !validStatus(m.Status) {
		return Merchant{}, ErrInvalidStatus
	}
	if m.Status == "" {
		current, err := s.repo.GetMerchant(ctx, id)
		if err != nil {
			return Merchant{}, err
		}
		m.Status = current.Status
	}
	return s.repo.UpdateMerchant(ctx, id, m)
}
func (s *Service) SetMerchantStatus(ctx context.Context, id string, status Status) (Merchant, error) {
	if status != StatusActive && status != StatusDisabled {
		return Merchant{}, ErrInvalidStatus
	}
	return s.repo.SetMerchantStatus(ctx, id, status)
}
func (s *Service) CreateStore(ctx context.Context, st Store) (Store, error) {
	if strings.TrimSpace(st.MerchantID) == "" || strings.TrimSpace(st.Name) == "" {
		return Store{}, ErrInvalidStore
	}
	if st.Status == "" {
		st.Status = StatusActive
	}
	if !validStatus(st.Status) {
		return Store{}, ErrInvalidStatus
	}
	st.AuditStatus = AuditPending
	return s.repo.CreateStore(ctx, st)
}
func (s *Service) GetStore(ctx context.Context, id string) (Store, error) {
	if strings.TrimSpace(id) == "" {
		return Store{}, ErrInvalidStore
	}
	return s.repo.GetStore(ctx, id)
}
func (s *Service) ListStoresByMerchant(ctx context.Context, id string) ([]Store, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrInvalidMerchant
	}
	return s.repo.ListStoresByMerchant(ctx, id)
}
func (s *Service) UpdateStore(ctx context.Context, id string, st Store) (Store, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(st.Name) == "" {
		return Store{}, ErrInvalidStore
	}
	if st.Status != "" && !validStatus(st.Status) {
		return Store{}, ErrInvalidStatus
	}
	current, err := s.repo.GetStore(ctx, id)
	if err != nil {
		return Store{}, err
	}
	if st.MerchantID != "" && st.MerchantID != current.MerchantID {
		return Store{}, ErrStoreMerchant
	}
	st.MerchantID = current.MerchantID
	if st.Status == "" {
		st.Status = current.Status
	}
	st.AuditStatus = AuditPending
	return s.repo.UpdateStore(ctx, id, st)
}
func (s *Service) SetStoreStatus(ctx context.Context, id string, status Status) (Store, error) {
	if status != StatusActive && status != StatusDisabled {
		return Store{}, ErrInvalidStatus
	}
	return s.repo.SetStoreStatus(ctx, id, status)
}
func (s *Service) DeleteStore(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return ErrInvalidStore
	}
	return s.repo.DeleteStore(ctx, id)
}
func (s *Service) CreateMerchantAccount(ctx context.Context, a MerchantAccount) (MerchantAccount, error) {
	if strings.TrimSpace(a.MerchantID) == "" || strings.TrimSpace(a.AccountID) == "" {
		return MerchantAccount{}, ErrInvalidAccount
	}
	if !validPermission(a.PermissionType) {
		return MerchantAccount{}, ErrInvalidPermission
	}
	return s.repo.CreateMerchantAccount(ctx, a)
}
func (s *Service) GetMerchantAccountByAccountID(ctx context.Context, id string) (MerchantAccount, error) {
	if strings.TrimSpace(id) == "" {
		return MerchantAccount{}, ErrInvalidAccount
	}
	return s.repo.GetMerchantAccountByAccountID(ctx, id)
}
func (s *Service) UpdateMerchantAccount(ctx context.Context, id string, a MerchantAccount) (MerchantAccount, error) {
	if strings.TrimSpace(id) == "" || !validPermission(a.PermissionType) {
		return MerchantAccount{}, ErrInvalidAccount
	}
	return s.repo.UpdateMerchantAccount(ctx, id, a)
}
func validStatus(status Status) bool {
	return status == StatusActive || status == StatusDisabled
}

func validPermission(p PermissionType) bool {
	return p == PermissionAll || p == PermissionBrand || p == PermissionStore
}
func (s *Service) ScopedStores(ctx context.Context, accountID string) ([]Store, error) {
	a, err := s.GetMerchantAccountByAccountID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	stores, err := s.ListStoresByMerchant(ctx, a.MerchantID)
	if err != nil {
		return nil, err
	}
	if a.PermissionType == PermissionAll {
		return stores, nil
	}
	out := make([]Store, 0)
	var ids []string
	if a.PermissionType == PermissionBrand {
		ids = append([]string(nil), a.BrandIDs...)
	} else if a.PermissionType == PermissionStore {
		ids = append([]string(nil), a.StoreIDs...)
	}
	for _, st := range stores {
		for _, id := range ids {
			if (a.PermissionType == PermissionStore && id == st.ID) || (a.PermissionType == PermissionBrand && id == st.BrandID) {
				out = append(out, st)
				break
			}
		}
	}
	return out, nil
}
func (s *Service) SubmitForReview(ctx context.Context, storeID, submittedBy string) (StoreAuditRecord, error) {
	st, err := s.GetStore(ctx, storeID)
	if err != nil {
		return StoreAuditRecord{}, err
	}
	if strings.TrimSpace(submittedBy) == "" {
		return StoreAuditRecord{}, ErrInvalidAudit
	}
	a, err := s.GetMerchantAccountByAccountID(ctx, submittedBy)
	if err != nil {
		return StoreAuditRecord{}, err
	}
	if !canAccessStore(a, st) {
		return StoreAuditRecord{}, ErrForbidden
	}
	data, err := json.Marshal(st)
	if err != nil {
		return StoreAuditRecord{}, err
	}
	r := StoreAuditRecord{StoreID: storeID, Type: AuditUpdate, Status: AuditPending, SubmittedBy: submittedBy, NewData: data, OldData: data}
	return s.repo.CreateAudit(ctx, r)
}
func canAccessStore(a MerchantAccount, st Store) bool {
	if a.MerchantID != st.MerchantID {
		return false
	}
	if a.PermissionType == PermissionAll {
		return true
	}
	if a.PermissionType == PermissionBrand {
		for _, id := range a.BrandIDs {
			if id == st.BrandID {
				return true
			}
		}
	}
	if a.PermissionType == PermissionStore {
		for _, id := range a.StoreIDs {
			if id == st.ID {
				return true
			}
		}
	}
	return false
}

func (s *Service) Approve(ctx context.Context, auditID, auditedBy, remark string) (StoreAuditRecord, error) {
	return s.review(ctx, auditID, auditedBy, remark, AuditApproved)
}
func (s *Service) Reject(ctx context.Context, auditID, auditedBy, remark string) (StoreAuditRecord, error) {
	return s.review(ctx, auditID, auditedBy, remark, AuditRejected)
}
func (s *Service) review(ctx context.Context, id, by, remark string, status AuditStatus) (StoreAuditRecord, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(by) == "" {
		return StoreAuditRecord{}, ErrInvalidAudit
	}
	r, err := s.repo.GetAudit(ctx, id)
	if err != nil {
		return StoreAuditRecord{}, err
	}
	if r.Status != AuditPending {
		return StoreAuditRecord{}, ErrConflict
	}
	if status != AuditApproved && status != AuditRejected {
		return StoreAuditRecord{}, ErrInvalidAudit
	}
	r.Status = status
	r.AuditedBy = by
	r.AuditRemark = remark
	return s.repo.UpdateAudit(ctx, id, r)
}
