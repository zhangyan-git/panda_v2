package repository

import (
	"context"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

func TestMemoryUpdateStoreRejectsMerchantMigration(t *testing.T) {
	r := NewMemory()
	_, _ = r.CreateStore(context.Background(), model.Store{ID: "store", MerchantID: "original", Name: "store"})
	if _, err := r.UpdateStore(context.Background(), "store", model.Store{MerchantID: "other", Name: "updated"}); err != model.ErrStoreMerchant {
		t.Fatalf("err=%v, want %v", err, model.ErrStoreMerchant)
	}
	got, _ := r.GetStore(context.Background(), "store")
	if got.MerchantID != "original" {
		t.Fatalf("merchant id changed: %q", got.MerchantID)
	}
}

func TestMemoryListStoresByMerchantIncludesHidden(t *testing.T) {
	r := NewMemory()
	_, err := r.CreateStore(context.Background(), model.Store{ID: "hidden", MerchantID: "m", Name: "hidden", Visible: false})
	if err != nil {
		t.Fatal(err)
	}
	stores, err := r.ListStoresByMerchant(context.Background(), "m")
	if err != nil || len(stores) != 1 || stores[0].ID != "hidden" {
		t.Fatalf("stores=%+v err=%v", stores, err)
	}
}

func TestMemoryGetAuditByID(t *testing.T) {
	r := NewMemory()
	created, err := r.CreateAudit(context.Background(), model.StoreAuditRecord{ID: "audit", StoreID: "store", NewData: []byte("new")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetAudit(context.Background(), created.ID)
	if err != nil || got.ID != "audit" {
		t.Fatalf("audit=%+v err=%v", got, err)
	}
	got.NewData[0] = 'x'
	again, _ := r.GetAudit(context.Background(), created.ID)
	if string(again.NewData) != "new" {
		t.Fatalf("audit data aliases repository: %q", again.NewData)
	}
}

func TestMemoryGetAuditNotFound(t *testing.T) {
	if _, err := NewMemory().GetAudit(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("err=%v, want %v", err, ErrNotFound)
	}
}

func TestMemoryAuditDataIsDeepCopied(t *testing.T) {
	r := NewMemory()
	newData, oldData := []byte("new"), []byte("old")
	created, err := r.CreateAudit(context.Background(), model.StoreAuditRecord{ID: "audit", StoreID: "store", NewData: newData, OldData: oldData})
	if err != nil {
		t.Fatal(err)
	}
	newData[0], oldData[0], created.NewData[0], created.OldData[0] = 'x', 'x', 'x', 'x'
	got, err := r.GetAudit(context.Background(), "audit")
	if err != nil || string(got.NewData) != "new" || string(got.OldData) != "old" {
		t.Fatalf("audit=%+v err=%v", got, err)
	}
}

func TestMemoryUpdateAuditRequiresPendingAndStore(t *testing.T) {
	r := NewMemory()
	_, _ = r.CreateStore(context.Background(), model.Store{ID: "store"})
	_, _ = r.CreateAudit(context.Background(), model.StoreAuditRecord{ID: "audit", StoreID: "store", Status: model.AuditPending})
	updated, err := r.UpdateAudit(context.Background(), "audit", model.StoreAuditRecord{StoreID: "store", Status: model.AuditApproved, AuditRemark: "ok", NewData: []byte("data")})
	if err != nil || updated.Status != model.AuditApproved {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	store, _ := r.GetStore(context.Background(), "store")
	if store.AuditStatus != model.AuditApproved || store.AuditRemark != "ok" {
		t.Fatalf("store=%+v", store)
	}
	if _, err := r.UpdateAudit(context.Background(), "audit", model.StoreAuditRecord{StoreID: "store", Status: model.AuditRejected}); err != model.ErrInvalidAudit {
		t.Fatalf("second update err=%v", err)
	}
}

func TestMemoryUpdateAuditMissingStoreIsAtomic(t *testing.T) {
	r := NewMemory()
	_, _ = r.CreateAudit(context.Background(), model.StoreAuditRecord{ID: "audit", StoreID: "missing", Status: model.AuditPending})
	if _, err := r.UpdateAudit(context.Background(), "audit", model.StoreAuditRecord{StoreID: "missing", Status: model.AuditApproved}); err != ErrNotFound {
		t.Fatalf("err=%v", err)
	}
	got, _ := r.GetAudit(context.Background(), "audit")
	if got.Status != model.AuditPending {
		t.Fatalf("audit changed: %+v", got)
	}
}
