package repository

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/domain"
)

var ErrNotFound = errors.New("merchant resource not found")

type Memory struct {
	mu        sync.RWMutex
	merchants map[string]domain.Merchant
	stores    map[string]domain.Store
	accounts  map[string]domain.MerchantAccount
	audits    map[string]domain.StoreAuditRecord
}

func NewMemory() *Memory {
	return &Memory{merchants: map[string]domain.Merchant{}, stores: map[string]domain.Store{}, accounts: map[string]domain.MerchantAccount{}, audits: map[string]domain.StoreAuditRecord{}}
}
func cloneStore(v domain.Store) domain.Store { return v }
func cloneAccount(v domain.MerchantAccount) domain.MerchantAccount {
	v.BrandIDs = append([]string(nil), v.BrandIDs...)
	v.StoreIDs = append([]string(nil), v.StoreIDs...)
	return v
}
func cloneAudit(v domain.StoreAuditRecord) domain.StoreAuditRecord {
	v.NewData = append([]byte(nil), v.NewData...)
	v.OldData = append([]byte(nil), v.OldData...)
	return v
}
func (r *Memory) CreateMerchant(_ context.Context, m domain.Merchant) (domain.Merchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	r.merchants[m.ID] = m
	return m, nil
}
func (r *Memory) GetMerchant(_ context.Context, id string) (domain.Merchant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.merchants[id]
	if !ok {
		return domain.Merchant{}, ErrNotFound
	}
	return v, nil
}
func (r *Memory) ListMerchants(_ context.Context) ([]domain.Merchant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.Merchant, 0, len(r.merchants))
	for _, v := range r.merchants {
		out = append(out, v)
	}
	return out, nil
}
func (r *Memory) UpdateMerchant(_ context.Context, id string, m domain.Merchant) (domain.Merchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.merchants[id]; !ok {
		return domain.Merchant{}, ErrNotFound
	}
	m.ID = id
	r.merchants[id] = m
	return m, nil
}
func (r *Memory) SetMerchantStatus(_ context.Context, id string, s domain.Status) (domain.Merchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.merchants[id]
	if !ok {
		return domain.Merchant{}, ErrNotFound
	}
	v.Status = s
	r.merchants[id] = v
	return v, nil
}
func (r *Memory) CreateStore(_ context.Context, v domain.Store) (domain.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	r.stores[v.ID] = v
	return v, nil
}
func (r *Memory) GetStore(_ context.Context, id string) (domain.Store, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.stores[id]
	if !ok {
		return domain.Store{}, ErrNotFound
	}
	return cloneStore(v), nil
}
func (r *Memory) ListStoresByMerchant(_ context.Context, id string) ([]domain.Store, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []domain.Store{}
	for _, v := range r.stores {
		if v.MerchantID == id {
			out = append(out, cloneStore(v))
		}
	}
	return out, nil
}
func (r *Memory) UpdateStore(_ context.Context, id string, v domain.Store) (domain.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.stores[id]
	if !ok {
		return domain.Store{}, ErrNotFound
	}
	if v.MerchantID != "" && v.MerchantID != current.MerchantID {
		return domain.Store{}, domain.ErrStoreMerchant
	}
	v.ID = id
	v.MerchantID = current.MerchantID
	r.stores[id] = v
	return v, nil
}
func (r *Memory) SetStoreStatus(_ context.Context, id string, s domain.Status) (domain.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.stores[id]
	if !ok {
		return domain.Store{}, ErrNotFound
	}
	v.Status = s
	r.stores[id] = v
	return v, nil
}
func (r *Memory) DeleteStore(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.stores[id]; !ok {
		return ErrNotFound
	}
	delete(r.stores, id)
	return nil
}
func (r *Memory) CreateMerchantAccount(_ context.Context, v domain.MerchantAccount) (domain.MerchantAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	r.accounts[v.AccountID] = v
	return v, nil
}
func (r *Memory) GetMerchantAccountByAccountID(_ context.Context, id string) (domain.MerchantAccount, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.accounts[id]
	if !ok {
		return domain.MerchantAccount{}, ErrNotFound
	}
	return cloneAccount(v), nil
}
func (r *Memory) UpdateMerchantAccount(_ context.Context, id string, v domain.MerchantAccount) (domain.MerchantAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, a := range r.accounts {
		if a.ID == id {
			v.ID = id
			r.accounts[key] = v
			return cloneAccount(v), nil
		}
	}
	return domain.MerchantAccount{}, ErrNotFound
}
func (r *Memory) CreateAudit(_ context.Context, v domain.StoreAuditRecord) (domain.StoreAuditRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	v = cloneAudit(v)
	r.audits[v.ID] = v
	return cloneAudit(v), nil
}
func (r *Memory) GetAudit(_ context.Context, id string) (domain.StoreAuditRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.audits[id]
	if !ok {
		return domain.StoreAuditRecord{}, ErrNotFound
	}
	return cloneAudit(v), nil
}

func (r *Memory) UpdateAudit(_ context.Context, id string, v domain.StoreAuditRecord) (domain.StoreAuditRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.audits[id]
	if !ok {
		return domain.StoreAuditRecord{}, ErrNotFound
	}
	if current.Status != domain.AuditPending || (v.Status != domain.AuditApproved && v.Status != domain.AuditRejected) {
		return domain.StoreAuditRecord{}, domain.ErrInvalidAudit
	}
	st, ok := r.stores[current.StoreID]
	if !ok || (v.StoreID != "" && v.StoreID != current.StoreID) {
		return domain.StoreAuditRecord{}, ErrNotFound
	}
	v.ID = id
	v.StoreID = current.StoreID
	v = cloneAudit(v)
	r.audits[id] = v
	st.AuditStatus = v.Status
	st.AuditRemark = v.AuditRemark
	r.stores[v.StoreID] = st
	return cloneAudit(v), nil
}
func (r *Memory) ListAuditsByStore(_ context.Context, id string) ([]domain.StoreAuditRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []domain.StoreAuditRecord{}
	for _, v := range r.audits {
		if id == "" || v.StoreID == id {
			out = append(out, cloneAudit(v))
		}
	}
	return out, nil
}

var _ domain.Repository = (*Memory)(nil)
