package model_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
)

func TestScopedStores(t *testing.T) {
	r := repository.NewMemory()
	m, _ := r.CreateMerchant(context.Background(), model.Merchant{Name: "m"})
	for _, v := range []model.Store{{MerchantID: m.ID, Name: "a", BrandID: "b1", Visible: true}, {MerchantID: m.ID, Name: "b", BrandID: "b2", Visible: false}} {
		r.CreateStore(context.Background(), v)
	}
	a, _ := r.CreateMerchantAccount(context.Background(), model.MerchantAccount{MerchantID: m.ID, AccountID: "x", PermissionType: model.PermissionBrand, BrandIDs: []string{"b1"}})
	s := service.NewService(r)
	got, err := s.ScopedStores(context.Background(), a.AccountID)
	if err != nil || len(got) != 1 || got[0].BrandID != "b1" {
		t.Fatalf("got %#v, %v", got, err)
	}
}

func TestCreateStoreStartsPending(t *testing.T) {
	r := repository.NewMemory()
	s := service.NewService(r)
	got, err := s.CreateStore(context.Background(), model.Store{MerchantID: "m", Name: "x"})
	if err != nil || got.AuditStatus != model.AuditPending {
		t.Fatalf("got %#v, %v", got, err)
	}
}

func TestAuditApprovalUpdatesStore(t *testing.T) {
	r := repository.NewMemory()
	s := service.NewService(r)
	st, _ := s.CreateStore(context.Background(), model.Store{MerchantID: "m", Name: "x", Visible: true})
	_, _ = r.CreateMerchantAccount(context.Background(), model.MerchantAccount{MerchantID: "m", AccountID: "u", PermissionType: model.PermissionAll})
	a, _ := s.SubmitForReview(context.Background(), st.ID, "u")
	if string(a.NewData) == st.ID {
		t.Fatal("audit snapshot must contain complete JSON")
	}
	var snapshot model.Store
	if err := json.Unmarshal(a.NewData, &snapshot); err != nil || snapshot.ID != st.ID || snapshot.Name != st.Name {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	if _, err := s.Approve(context.Background(), a.ID, "admin", "ok"); err != nil {
		t.Fatal(err)
	}
	st, err := s.GetStore(context.Background(), st.ID)
	if err != nil || st.AuditStatus != model.AuditApproved {
		t.Fatalf("store=%#v err=%v", st, err)
	}
}

func TestSubmitForReviewCopiesOldData(t *testing.T) {
	r := repository.NewMemory()
	s := service.NewService(r)
	st, _ := s.CreateStore(context.Background(), model.Store{MerchantID: "m", Name: "x"})
	_, _ = r.CreateMerchantAccount(context.Background(), model.MerchantAccount{MerchantID: "m", AccountID: "u", PermissionType: model.PermissionAll})
	a, err := s.SubmitForReview(context.Background(), st.ID, "u")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.OldData) != string(a.NewData) || len(a.OldData) == 0 {
		t.Fatalf("old=%s new=%s", a.OldData, a.NewData)
	}
}

func TestReviewTerminalStateConflicts(t *testing.T) {
	r := repository.NewMemory()
	s := service.NewService(r)
	_, _ = r.CreateAudit(context.Background(), model.StoreAuditRecord{ID: "audit", StoreID: "store", Status: model.AuditApproved})
	if _, err := s.Reject(context.Background(), "audit", "admin", "changed"); err != model.ErrConflict {
		t.Fatalf("err=%v, want %v", err, model.ErrConflict)
	}
}

func TestUpdateStoreEmptyStatusPreservesCurrent(t *testing.T) {
	r := repository.NewMemory()
	s := service.NewService(r)
	st, _ := s.CreateStore(context.Background(), model.Store{MerchantID: "m", Name: "x", Status: model.StatusDisabled})
	updated, err := s.UpdateStore(context.Background(), st.ID, model.Store{Name: "updated"})
	if err != nil || updated.Status != model.StatusDisabled {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}
