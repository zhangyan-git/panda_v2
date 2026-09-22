package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

// TestBrandAuditRequiresPending 覆盖并发审核的第一道闸：状态已经不是 pending 时，
// 连事务都不该开。真正的权威判定是 SetAuditInTx 的谓词（要连库才能验）。
func TestBrandAuditRequiresPending(t *testing.T) {
	var events []string
	brands := &brandRepoFake{events: &events, brands: map[string]model.Brand{
		"b1": {ID: "b1", MerchantID: "m1", AuditStatus: "approved"},
	}}
	svc := NewAdminBrandService(brands, nil, nil, nil, &fakeTransactor{events: &events})

	if err := svc.Audit(context.Background(), "b1", true, "", "admin"); !errors.Is(err, repository.ErrAuditNotPending) {
		t.Fatalf("err=%v want ErrAuditNotPending", err)
	}
	if !reflect.DeepEqual(events, []string{"find_brand"}) {
		t.Fatalf("events=%v want [find_brand]", events)
	}
}

// TestBrandAuditWritesEntityAndRecordInOneTx 覆盖审核的两步落在同一次 InTx 里。
func TestBrandAuditWritesEntityAndRecordInOneTx(t *testing.T) {
	var events []string
	brands := &brandRepoFake{events: &events, brands: map[string]model.Brand{
		"b1": {ID: "b1", MerchantID: "m1", AuditStatus: "pending"},
	}}
	audits := &auditRepoFake{events: &events, pending: &model.AuditRecord{ID: "r1", TargetID: "b1"}}
	svc := NewAdminBrandService(brands, nil, audits, nil, &fakeTransactor{events: &events})

	if err := svc.Audit(context.Background(), "b1", false, "驳回原因", "admin"); err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"find_brand", "in_tx", "audit_brand", "find_pending", "seal_record"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want %v", events, want)
	}
	if !reflect.DeepEqual(brands.audited, []string{"b1:rejected"}) {
		t.Fatalf("实体落章=%v want [b1:rejected]", brands.audited)
	}
	if !reflect.DeepEqual(audits.sealed, []string{"r1:rejected"}) {
		t.Fatalf("记录落章=%v want [r1:rejected]", audits.sealed)
	}
}

// TestBrandCreateWritesEntityAndRecordInOneTx 覆盖新建：品牌与待审核记录同一次提交，
// 实体写入失败时不再写记录。
func TestBrandCreateWritesEntityAndRecordInOneTx(t *testing.T) {
	failure := errors.New("insert failed")
	var events []string
	merchants := &merchantRepoFake{events: &events}
	brands := &brandRepoFake{events: &events, createErr: failure}
	audits := &auditRepoFake{events: &events}
	svc := NewAdminBrandService(brands, merchants, audits, nil, &fakeTransactor{events: &events})

	if _, err := svc.Create(context.Background(), BrandInput{MerchantID: "m1", Name: "品牌"}, "admin"); !errors.Is(err, failure) {
		t.Fatalf("err=%v want %v", err, failure)
	}
	want := []string{"find_merchant", "in_tx", "create_brand"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want %v", events, want)
	}
	if audits.created != nil {
		t.Fatal("品牌没写进去，不该留下审核记录")
	}
}
