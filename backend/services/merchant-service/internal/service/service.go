package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

type Service struct{ repo model.Repository }

func NewService(repo model.Repository) *Service { return &Service{repo: repo} }
func (s *Service) CreateMerchant(ctx context.Context, m model.Merchant) (model.Merchant, error) {
	if strings.TrimSpace(m.Name) == "" {
		return model.Merchant{}, model.ErrInvalidMerchant
	}
	if m.Status == "" {
		m.Status = model.StatusActive
	}
	if !validStatus(m.Status) {
		return model.Merchant{}, model.ErrInvalidStatus
	}
	return s.repo.CreateMerchant(ctx, m)
}
func (s *Service) GetMerchant(ctx context.Context, id string) (model.Merchant, error) {
	if strings.TrimSpace(id) == "" {
		return model.Merchant{}, model.ErrInvalidMerchant
	}
	return s.repo.GetMerchant(ctx, id)
}
func (s *Service) ListMerchants(ctx context.Context) ([]model.Merchant, error) {
	return s.repo.ListMerchants(ctx)
}
func (s *Service) UpdateMerchant(ctx context.Context, id string, m model.Merchant) (model.Merchant, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(m.Name) == "" {
		return model.Merchant{}, model.ErrInvalidMerchant
	}
	if m.Status != "" && !validStatus(m.Status) {
		return model.Merchant{}, model.ErrInvalidStatus
	}
	if m.Status == "" {
		current, err := s.repo.GetMerchant(ctx, id)
		if err != nil {
			return model.Merchant{}, err
		}
		m.Status = current.Status
	}
	return s.repo.UpdateMerchant(ctx, id, m)
}
func (s *Service) SetMerchantStatus(ctx context.Context, id string, status model.Status) (model.Merchant, error) {
	if status != model.StatusActive && status != model.StatusDisabled {
		return model.Merchant{}, model.ErrInvalidStatus
	}
	return s.repo.SetMerchantStatus(ctx, id, status)
}
func (s *Service) CreateStore(ctx context.Context, st model.Store) (model.Store, error) {
	if strings.TrimSpace(st.MerchantID) == "" || strings.TrimSpace(st.Name) == "" {
		return model.Store{}, model.ErrInvalidStore
	}
	if st.Status == "" {
		st.Status = model.StatusActive
	}
	if !validStatus(st.Status) {
		return model.Store{}, model.ErrInvalidStatus
	}
	st.AuditStatus = model.AuditPending
	return s.repo.CreateStore(ctx, st)
}
func (s *Service) GetStore(ctx context.Context, id string) (model.Store, error) {
	if strings.TrimSpace(id) == "" {
		return model.Store{}, model.ErrInvalidStore
	}
	return s.repo.GetStore(ctx, id)
}
func (s *Service) ListStoresByMerchant(ctx context.Context, id string) ([]model.Store, error) {
	if strings.TrimSpace(id) == "" {
		return nil, model.ErrInvalidMerchant
	}
	return s.repo.ListStoresByMerchant(ctx, id)
}
func (s *Service) UpdateStore(ctx context.Context, id string, st model.Store) (model.Store, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(st.Name) == "" {
		return model.Store{}, model.ErrInvalidStore
	}
	if st.Status != "" && !validStatus(st.Status) {
		return model.Store{}, model.ErrInvalidStatus
	}
	current, err := s.repo.GetStore(ctx, id)
	if err != nil {
		return model.Store{}, err
	}
	if st.MerchantID != "" && st.MerchantID != current.MerchantID {
		return model.Store{}, model.ErrStoreMerchant
	}
	st.MerchantID = current.MerchantID
	if st.Status == "" {
		st.Status = current.Status
	}
	st.AuditStatus = model.AuditPending
	return s.repo.UpdateStore(ctx, id, st)
}
func (s *Service) SetStoreStatus(ctx context.Context, id string, status model.Status) (model.Store, error) {
	if status != model.StatusActive && status != model.StatusDisabled {
		return model.Store{}, model.ErrInvalidStatus
	}
	return s.repo.SetStoreStatus(ctx, id, status)
}
func (s *Service) DeleteStore(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return model.ErrInvalidStore
	}
	return s.repo.DeleteStore(ctx, id)
}
func (s *Service) CreateMerchantAccount(ctx context.Context, a model.MerchantAccount) (model.MerchantAccount, error) {
	if strings.TrimSpace(a.MerchantID) == "" || strings.TrimSpace(a.AccountID) == "" {
		return model.MerchantAccount{}, model.ErrInvalidAccount
	}
	if !validPermission(a.PermissionType) {
		return model.MerchantAccount{}, model.ErrInvalidPermission
	}
	return s.repo.CreateMerchantAccount(ctx, a)
}
func (s *Service) GetMerchantAccountByAccountID(ctx context.Context, id string) (model.MerchantAccount, error) {
	if strings.TrimSpace(id) == "" {
		return model.MerchantAccount{}, model.ErrInvalidAccount
	}
	return s.repo.GetMerchantAccountByAccountID(ctx, id)
}
func (s *Service) UpdateMerchantAccount(ctx context.Context, id string, a model.MerchantAccount) (model.MerchantAccount, error) {
	if strings.TrimSpace(id) == "" || !validPermission(a.PermissionType) {
		return model.MerchantAccount{}, model.ErrInvalidAccount
	}
	return s.repo.UpdateMerchantAccount(ctx, id, a)
}
func validStatus(status model.Status) bool {
	return status == model.StatusActive || status == model.StatusDisabled
}

func validPermission(p model.PermissionType) bool {
	return p == model.PermissionAll || p == model.PermissionBrand || p == model.PermissionStore
}
func (s *Service) ScopedStores(ctx context.Context, accountID string) ([]model.Store, error) {
	a, err := s.GetMerchantAccountByAccountID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	stores, err := s.ListStoresByMerchant(ctx, a.MerchantID)
	if err != nil {
		return nil, err
	}
	if a.PermissionType == model.PermissionAll {
		return stores, nil
	}
	out := make([]model.Store, 0)
	var ids []string
	if a.PermissionType == model.PermissionBrand {
		ids = append([]string(nil), a.BrandIDs...)
	} else if a.PermissionType == model.PermissionStore {
		ids = append([]string(nil), a.StoreIDs...)
	}
	for _, st := range stores {
		for _, id := range ids {
			if (a.PermissionType == model.PermissionStore && id == st.ID) || (a.PermissionType == model.PermissionBrand && id == st.BrandID) {
				out = append(out, st)
				break
			}
		}
	}
	return out, nil
}
func (s *Service) SubmitForReview(ctx context.Context, storeID, submittedBy string) (model.StoreAuditRecord, error) {
	st, err := s.GetStore(ctx, storeID)
	if err != nil {
		return model.StoreAuditRecord{}, err
	}
	if strings.TrimSpace(submittedBy) == "" {
		return model.StoreAuditRecord{}, model.ErrInvalidAudit
	}
	a, err := s.GetMerchantAccountByAccountID(ctx, submittedBy)
	if err != nil {
		return model.StoreAuditRecord{}, err
	}
	if !canAccessStore(a, st) {
		return model.StoreAuditRecord{}, model.ErrForbidden
	}
	data, err := json.Marshal(st)
	if err != nil {
		return model.StoreAuditRecord{}, err
	}
	r := model.StoreAuditRecord{StoreID: storeID, Type: model.AuditUpdate, Status: model.AuditPending, SubmittedBy: submittedBy, NewData: data, OldData: data}
	return s.repo.CreateAudit(ctx, r)
}
func canAccessStore(a model.MerchantAccount, st model.Store) bool {
	if a.MerchantID != st.MerchantID {
		return false
	}
	if a.PermissionType == model.PermissionAll {
		return true
	}
	if a.PermissionType == model.PermissionBrand {
		for _, id := range a.BrandIDs {
			if id == st.BrandID {
				return true
			}
		}
	}
	if a.PermissionType == model.PermissionStore {
		for _, id := range a.StoreIDs {
			if id == st.ID {
				return true
			}
		}
	}
	return false
}

func (s *Service) Approve(ctx context.Context, auditID, auditedBy, remark string) (model.StoreAuditRecord, error) {
	return s.review(ctx, auditID, auditedBy, remark, model.AuditApproved)
}
func (s *Service) Reject(ctx context.Context, auditID, auditedBy, remark string) (model.StoreAuditRecord, error) {
	return s.review(ctx, auditID, auditedBy, remark, model.AuditRejected)
}
func (s *Service) review(ctx context.Context, id, by, remark string, status model.AuditStatus) (model.StoreAuditRecord, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(by) == "" {
		return model.StoreAuditRecord{}, model.ErrInvalidAudit
	}
	r, err := s.repo.GetAudit(ctx, id)
	if err != nil {
		return model.StoreAuditRecord{}, err
	}
	if r.Status != model.AuditPending {
		return model.StoreAuditRecord{}, model.ErrConflict
	}
	if status != model.AuditApproved && status != model.AuditRejected {
		return model.StoreAuditRecord{}, model.ErrInvalidAudit
	}
	r.Status = status
	r.AuditedBy = by
	r.AuditRemark = remark
	return s.repo.UpdateAudit(ctx, id, r)
}
