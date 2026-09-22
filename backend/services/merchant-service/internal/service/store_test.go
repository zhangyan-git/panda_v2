package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

// 这一组用例只覆盖 service 层的编排：谁在什么时候被调用、调用参数是什么、
// 错误怎么往外走。真实的行锁、RowsAffected 与事务回滚要连库才看得到，
// 不在 fake 里假装（见报告里的未覆盖清单）。

// nopTx 只占住 pgx.Tx 的位置：用例里的仓库全是 fake，不会真的碰它。
// 传 nil 也能跑，但那会把「谁误用了 tx」藏成一次 panic。
type nopTx struct{ pgx.Tx }

// fakeTransactor 记下「进过事务」这一步，并把回调的错误原样往外传——
// InTx 拿到错误就不提交，这正是「两步同生共死」的判据。
type fakeTransactor struct {
	events *[]string
	err    error
}

func (f *fakeTransactor) InTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	*f.events = append(*f.events, "in_tx")
	if f.err != nil {
		return f.err
	}
	return fn(ctx, nopTx{})
}

type storeRepoFake struct {
	repository.StoreRepository
	events    *[]string
	store     model.Store
	findErr   error
	createErr error
	updateErr error
	auditErr  error
	created   *model.Store
	updated   *model.Store
	audited   []string
}

func (f *storeRepoFake) FindByID(context.Context, string) (*model.Store, error) {
	*f.events = append(*f.events, "find_store")
	if f.findErr != nil {
		return nil, f.findErr
	}
	s := f.store
	return &s, nil
}

func (f *storeRepoFake) CreateInTx(_ context.Context, _ pgx.Tx, s *model.Store) error {
	*f.events = append(*f.events, "create_store")
	f.created = s
	return f.createErr
}

func (f *storeRepoFake) Update(_ context.Context, s *model.Store) error {
	*f.events = append(*f.events, "update_store")
	f.updated = s
	return f.updateErr
}

func (f *storeRepoFake) SetAuditInTx(_ context.Context, _ pgx.Tx, id, auditStatus, _, _ string) error {
	*f.events = append(*f.events, "audit_store")
	f.audited = append(f.audited, id+":"+auditStatus)
	return f.auditErr
}

type brandRepoFake struct {
	repository.BrandRepository
	events    *[]string
	brands    map[string]model.Brand
	findErr   error
	createErr error
	auditErr  error
	created   *model.Brand
	audited   []string
}

func (f *brandRepoFake) FindByID(_ context.Context, id string) (*model.Brand, error) {
	*f.events = append(*f.events, "find_brand")
	if f.findErr != nil {
		return nil, f.findErr
	}
	b, ok := f.brands[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return &b, nil
}

func (f *brandRepoFake) CreateInTx(_ context.Context, _ pgx.Tx, b *model.Brand) error {
	*f.events = append(*f.events, "create_brand")
	f.created = b
	return f.createErr
}

func (f *brandRepoFake) SetAuditInTx(_ context.Context, _ pgx.Tx, id, auditStatus, _, _ string) error {
	*f.events = append(*f.events, "audit_brand")
	f.audited = append(f.audited, id+":"+auditStatus)
	return f.auditErr
}

type auditRepoFake struct {
	repository.AuditRecordRepository
	events    *[]string
	pending   *model.AuditRecord
	findErr   error
	createErr error
	auditErr  error
	created   *model.AuditRecord
	sealed    []string
}

func (f *auditRepoFake) CreateInTx(_ context.Context, _ pgx.Tx, r *model.AuditRecord) error {
	*f.events = append(*f.events, "create_record")
	f.created = r
	return f.createErr
}

// Create 是「自己提交」的那条路，只有平台直接编辑（Update）还在走它，事件名与
// CreateInTx 分开记，好让用例里一眼看出哪条链路没有和实体写入共用事务。
func (f *auditRepoFake) Create(_ context.Context, r *model.AuditRecord) error {
	*f.events = append(*f.events, "create_record_untxed")
	f.created = r
	return f.createErr
}

func (f *auditRepoFake) FindLatestPendingInTx(context.Context, pgx.Tx, string) (*model.AuditRecord, error) {
	*f.events = append(*f.events, "find_pending")
	if f.findErr != nil {
		return nil, f.findErr
	}
	if f.pending == nil {
		return nil, pgx.ErrNoRows
	}
	return f.pending, nil
}

func (f *auditRepoFake) SetAuditedInTx(_ context.Context, _ pgx.Tx, id, status, _, _ string) error {
	*f.events = append(*f.events, "seal_record")
	f.sealed = append(f.sealed, id+":"+status)
	return f.auditErr
}

// TestStoreUpdateKeepsOwnershipAndBrandInSync 覆盖「门店归属」与「品牌归属」分叉这条路：
// 请求带的商户 ID 与库上不一致时拒绝，品牌必须属于门店**库上**的那个商户。
func TestStoreUpdateKeepsOwnershipAndBrandInSync(t *testing.T) {
	for _, tt := range []struct {
		name         string
		reqMerchant  string
		reqBrand     string
		findErr      error
		wantErr      error
		wantEvents   []string
		wantMerchant string
	}{
		{
			name:        "同商户换到自己名下的另一个品牌",
			reqMerchant: "m1", reqBrand: "b1b",
			wantEvents:   []string{"find_store", "find_brand", "update_store", "create_record_untxed"},
			wantMerchant: "m1",
		},
		{
			name:        "换到别的商户的品牌被拒",
			reqMerchant: "m1", reqBrand: "b2",
			wantErr:    ErrStoreBrandMismatch,
			wantEvents: []string{"find_store", "find_brand"},
		},
		{
			name:        "请求里的商户与库上不一致被拒（不静默忽略）",
			reqMerchant: "m2", reqBrand: "b1",
			wantErr:    ErrStoreMerchantImmutable,
			wantEvents: []string{"find_store"},
		},
		{
			name:        "请求不带商户时按库上归属处理",
			reqMerchant: "", reqBrand: "b1",
			wantEvents:   []string{"find_store", "find_brand", "update_store", "create_record_untxed"},
			wantMerchant: "m1",
		},
		{
			name:        "门店不存在",
			reqMerchant: "m1", reqBrand: "b1",
			findErr:    pgx.ErrNoRows,
			wantErr:    pgx.ErrNoRows,
			wantEvents: []string{"find_store"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			stores := &storeRepoFake{events: &events, findErr: tt.findErr}
			stores.store = model.Store{ID: "s1", MerchantID: "m1", BrandID: "b1"}
			// b1 属于 m1（门店现在的商户），b1b 也属于 m1，b2 属于 m2。
			brands := &brandRepoFake{events: &events, brands: map[string]model.Brand{
				"b1":  {ID: "b1", MerchantID: "m1"},
				"b1b": {ID: "b1b", MerchantID: "m1"},
				"b2":  {ID: "b2", MerchantID: "m2"},
			}}
			// audits 只服务 Update 末尾那条留痕记录（平台直接编辑，不是待审项）。
			audits := &auditRepoFake{events: &events}
			svc := NewAdminStoreService(stores, brands, nil, audits, nil, &fakeTransactor{events: &events})

			_, err := svc.Update(context.Background(), "s1", StoreInput{
				MerchantID: tt.reqMerchant, BrandID: tt.reqBrand, Name: "门店",
			}, "admin")

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(events, tt.wantEvents) {
				t.Fatalf("events=%v want %v", events, tt.wantEvents)
			}
			if tt.wantErr == nil {
				if stores.updated == nil {
					t.Fatal("没有写入门店")
				}
				// 归属不会被写歪：落库的 merchant_id 只能是门店原来那个。
				if stores.updated.MerchantID != tt.wantMerchant {
					t.Fatalf("merchant_id=%q want %q", stores.updated.MerchantID, tt.wantMerchant)
				}
				if stores.updated.BrandID != tt.reqBrand {
					t.Fatalf("brand_id=%q want %q", stores.updated.BrandID, tt.reqBrand)
				}
			} else if stores.updated != nil {
				t.Fatal("被拒的请求不该写库")
			}
		})
	}
}

// TestStoreAuditRequiresPending 覆盖并发审核的第一道闸：状态已经不是 pending 时，
// 连事务都不该开。真正的权威判定是 SetAuditInTx 的谓词（要连库才能验）。
func TestStoreAuditRequiresPending(t *testing.T) {
	for _, audited := range []string{"approved", "rejected"} {
		var events []string
		stores := &storeRepoFake{events: &events}
		stores.store = model.Store{ID: "s1", MerchantID: "m1", AuditStatus: audited}
		svc := NewAdminStoreService(stores, nil, nil, nil, nil, &fakeTransactor{events: &events})

		if err := svc.Audit(context.Background(), "s1", true, "", "admin"); !errors.Is(err, repository.ErrAuditNotPending) {
			t.Fatalf("audit_status=%s err=%v want ErrAuditNotPending", audited, err)
		}
		if !reflect.DeepEqual(events, []string{"find_store"}) {
			t.Fatalf("events=%v want [find_store]", events)
		}
	}
}

// TestStoreAuditWritesEntityAndRecordInOneTx 覆盖审核的两步落在同一次 InTx 里，
// 且驳回把实体落成 rejected（status 的降级在 SQL 里，此处只看得见 audit_status）。
func TestStoreAuditWritesEntityAndRecordInOneTx(t *testing.T) {
	for _, tt := range []struct {
		name       string
		approve    bool
		wantStatus string
	}{
		{"通过", true, "approved"},
		{"驳回", false, "rejected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			stores := &storeRepoFake{events: &events}
			stores.store = model.Store{ID: "s1", MerchantID: "m1", AuditStatus: "pending"}
			audits := &auditRepoFake{events: &events, pending: &model.AuditRecord{ID: "r1", TargetID: "s1"}}
			svc := NewAdminStoreService(stores, nil, nil, audits, nil, &fakeTransactor{events: &events})

			if err := svc.Audit(context.Background(), "s1", tt.approve, "备注", "admin"); err != nil {
				t.Fatalf("err=%v", err)
			}
			want := []string{"find_store", "in_tx", "audit_store", "find_pending", "seal_record"}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events=%v want %v", events, want)
			}
			if !reflect.DeepEqual(stores.audited, []string{"s1:" + tt.wantStatus}) {
				t.Fatalf("实体落章=%v want [s1:%s]", stores.audited, tt.wantStatus)
			}
			if !reflect.DeepEqual(audits.sealed, []string{"r1:" + tt.wantStatus}) {
				t.Fatalf("记录落章=%v want [r1:%s]", audits.sealed, tt.wantStatus)
			}
		})
	}
}

// TestStoreAuditRollsBackBothSteps 只造出「第二步失败」这一种情形：
// fakeTransactor 把回调的错误原样往外传，service 必须让它冒出去而不是吞掉——
// 真实回滚要连库才验得到，但如果这里把错误吞了，回滚也就无从谈起。
func TestStoreAuditRollsBackBothSteps(t *testing.T) {
	failure := errors.New("seal failed")
	var events []string
	stores := &storeRepoFake{events: &events}
	stores.store = model.Store{ID: "s1", MerchantID: "m1", AuditStatus: "pending"}
	audits := &auditRepoFake{events: &events, pending: &model.AuditRecord{ID: "r1"}, auditErr: failure}
	svc := NewAdminStoreService(stores, nil, nil, audits, nil, &fakeTransactor{events: &events})

	if err := svc.Audit(context.Background(), "s1", true, "", "admin"); !errors.Is(err, failure) {
		t.Fatalf("err=%v want %v", err, failure)
	}
	want := []string{"find_store", "in_tx", "audit_store", "find_pending", "seal_record"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want %v", events, want)
	}
}

// TestStoreAuditWithoutPendingRecord 覆盖历史数据：实体已落章但没有待审记录时不报错。
func TestStoreAuditWithoutPendingRecord(t *testing.T) {
	var events []string
	stores := &storeRepoFake{events: &events}
	stores.store = model.Store{ID: "s1", MerchantID: "m1", AuditStatus: "pending"}
	svc := NewAdminStoreService(stores, nil, nil, &auditRepoFake{events: &events}, nil, &fakeTransactor{events: &events})

	if err := svc.Audit(context.Background(), "s1", true, "", "admin"); err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"find_store", "in_tx", "audit_store", "find_pending"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want %v", events, want)
	}
}

// TestStoreCreateWritesEntityAndRecordInOneTx 覆盖新建：门店与待审核记录同一次提交，
// 第一步失败时第二步不该发生。
func TestStoreCreateWritesEntityAndRecordInOneTx(t *testing.T) {
	failure := errors.New("insert failed")
	for _, tt := range []struct {
		name       string
		createErr  error
		wantErr    error
		wantEvents []string
	}{
		{"成功", nil, nil, []string{"find_merchant", "find_brand", "in_tx", "create_store", "create_record"}},
		{"门店写入失败则不再写记录", failure, failure,
			[]string{"find_merchant", "find_brand", "in_tx", "create_store"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			merchants := &merchantRepoFake{events: &events}
			brands := &brandRepoFake{events: &events, brands: map[string]model.Brand{"b1": {ID: "b1", MerchantID: "m1"}}}
			stores := &storeRepoFake{events: &events, createErr: tt.createErr}
			audits := &auditRepoFake{events: &events}
			svc := NewAdminStoreService(stores, brands, merchants, audits, nil, &fakeTransactor{events: &events})

			st, err := svc.Create(context.Background(), StoreInput{
				MerchantID: "m1", BrandID: "b1", Name: "门店",
			}, "admin")

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err=%v want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(events, tt.wantEvents) {
				t.Fatalf("events=%v want %v", events, tt.wantEvents)
			}
			if tt.wantErr != nil {
				return
			}
			if st == nil || st.AuditStatus != "pending" || st.Status != "active" {
				t.Fatalf("store=%+v", st)
			}
			if audits.created == nil || audits.created.TargetID != st.ID || audits.created.Status != "pending" {
				t.Fatalf("record=%+v", audits.created)
			}
			if merchants.merchantID != "m1" {
				t.Fatalf("商户存在性检查用了 %q", merchants.merchantID)
			}
		})
	}
}
