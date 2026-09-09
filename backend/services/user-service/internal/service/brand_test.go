package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// fakeBrandRepo 内存版 BrandRepository，仅覆盖单测需要的行为
type fakeBrandRepo struct {
	repository.BrandRepository
	brands    map[string]*model.Brand
	hasStores map[string]bool
}

func newFakeBrandRepo() *fakeBrandRepo {
	return &fakeBrandRepo{brands: map[string]*model.Brand{}, hasStores: map[string]bool{}}
}

func (f *fakeBrandRepo) FindByID(_ context.Context, id string) (*model.Brand, error) {
	b, ok := f.brands[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return b, nil
}

func (f *fakeBrandRepo) Create(_ context.Context, b *model.Brand) error {
	f.brands[b.ID] = b
	return nil
}

func (f *fakeBrandRepo) Update(_ context.Context, b *model.Brand) error {
	f.brands[b.ID] = b
	return nil
}

func (f *fakeBrandRepo) UpdateStatus(_ context.Context, id, status string) error {
	b, ok := f.brands[id]
	if !ok {
		return pgx.ErrNoRows
	}
	b.Status = status
	return nil
}

func (f *fakeBrandRepo) SetAudit(_ context.Context, id, auditStatus, remark, by string) error {
	b, ok := f.brands[id]
	if !ok {
		return pgx.ErrNoRows
	}
	b.AuditStatus = auditStatus
	b.AuditRemark = remark
	b.AuditBy = by
	return nil
}

func (f *fakeBrandRepo) Delete(_ context.Context, id string) error {
	delete(f.brands, id)
	return nil
}

func (f *fakeBrandRepo) HasStores(_ context.Context, id string) (bool, error) {
	return f.hasStores[id], nil
}

// fakeStoreRepo 内存版 StoreRepository
type fakeStoreRepo struct {
	repository.StoreRepository
	stores   map[string]*model.Store
	hasStore bool // HasStores 结果（品牌删除保护）
}

func newFakeStoreRepo() *fakeStoreRepo {
	return &fakeStoreRepo{stores: map[string]*model.Store{}}
}

func (f *fakeStoreRepo) FindByID(_ context.Context, id string) (*model.Store, error) {
	s, ok := f.stores[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return s, nil
}

func (f *fakeStoreRepo) Create(_ context.Context, s *model.Store) error {
	f.stores[s.ID] = s
	return nil
}

// fakeAuditRepo 内存版 AuditRecordRepository
type fakeAuditRepo struct {
	repository.AuditRecordRepository
	records []*model.AuditRecord
}

func (f *fakeAuditRepo) Create(_ context.Context, r *model.AuditRecord) error {
	f.records = append(f.records, r)
	return nil
}

func (f *fakeAuditRepo) FindLatestPending(_ context.Context, targetID string) (*model.AuditRecord, error) {
	for i := len(f.records) - 1; i >= 0; i-- {
		if f.records[i].TargetID == targetID && f.records[i].Status == "pending" {
			return f.records[i], nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeAuditRepo) SetAudited(_ context.Context, id, status, remark, by string) error {
	for _, r := range f.records {
		if r.ID == id {
			r.Status = status
			r.AuditRemark = remark
			r.AuditBy = by
			return nil
		}
	}
	return pgx.ErrNoRows
}

// 账号范围回收：给 merchant_test.go 的 fakeMerchantUserRepo 补实现
func (f *fakeMerchantUserRepo) UpdateScope(_ context.Context, id, scopeType, scopeID string, isAdmin bool) error {
	u, ok := f.users[id]
	if !ok {
		return pgx.ErrNoRows
	}
	u.ScopeType = scopeType
	u.ScopeID = scopeID
	u.IsAdmin = isAdmin
	return nil
}

func (f *fakeMerchantUserRepo) ResetScopeByTarget(_ context.Context, scopeID string) error {
	for _, u := range f.users {
		if u.ScopeID == scopeID {
			u.ScopeType = "merchant"
			u.ScopeID = ""
		}
	}
	return nil
}

func TestBrandCreateAndAuditFlow(t *testing.T) {
	merchants := newFakeMerchantRepo()
	brands := newFakeBrandRepo()
	audits := &fakeAuditRepo{}
	users := newFakeMerchantUserRepo()
	svc := NewAdminBrandService(brands, merchants, audits, users)
	ctx := context.Background()

	seedMerchant(merchants, "m1", "active")

	// 创建：状态 active、审核状态 pending，并生成一条 pending 审核记录
	b, err := svc.Create(ctx, BrandInput{MerchantID: "m1", Name: "品牌A", Visible: true}, "admin-1")
	if err != nil {
		t.Fatalf("创建品牌失败: %v", err)
	}
	if b.Status != "active" || b.AuditStatus != "pending" {
		t.Fatalf("初始状态错误: status=%s audit=%s", b.Status, b.AuditStatus)
	}
	if len(audits.records) != 1 || audits.records[0].Type != "create" || audits.records[0].Status != "pending" {
		t.Fatalf("审核记录未生成: %+v", audits.records)
	}

	// 审核通过：实体与记录同步落章
	if err := svc.Audit(ctx, b.ID, true, "没问题", "admin-1"); err != nil {
		t.Fatalf("审核应成功: %v", err)
	}
	if brands.brands[b.ID].AuditStatus != "approved" {
		t.Fatalf("实体审核状态未更新: %s", brands.brands[b.ID].AuditStatus)
	}
	if audits.records[0].Status != "approved" || audits.records[0].AuditRemark != "没问题" {
		t.Fatalf("审核记录未落章: %+v", audits.records[0])
	}

	// 非 pending 不可再审
	if err := svc.Audit(ctx, b.ID, false, "重审", "admin-1"); !errors.Is(err, ErrAuditNotPending) {
		t.Fatalf("重复审核应拒绝, got %v", err)
	}

	// 驳回链路：新建→拒绝
	b2, err := svc.Create(ctx, BrandInput{MerchantID: "m1", Name: "品牌B"}, "admin-1")
	if err != nil {
		t.Fatalf("创建品牌失败: %v", err)
	}
	if err := svc.Audit(ctx, b2.ID, false, "资料不全", "admin-1"); err != nil {
		t.Fatalf("驳回应成功: %v", err)
	}
	if brands.brands[b2.ID].AuditStatus != "rejected" || audits.records[1].Status != "rejected" {
		t.Fatalf("驳回未落章: %s / %s", brands.brands[b2.ID].AuditStatus, audits.records[1].Status)
	}

	// 名称必填 / 商户必须存在
	if _, err := svc.Create(ctx, BrandInput{MerchantID: "m1", Name: ""}, "admin-1"); !errors.Is(err, ErrBrandNameRequired) {
		t.Fatalf("空名称应拒绝, got %v", err)
	}
	if _, err := svc.Create(ctx, BrandInput{MerchantID: "missing", Name: "X"}, "admin-1"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("商户不存在应返回 pgx.ErrNoRows, got %v", err)
	}
}

func TestBrandDeleteGuards(t *testing.T) {
	merchants := newFakeMerchantRepo()
	brands := newFakeBrandRepo()
	audits := &fakeAuditRepo{}
	users := newFakeMerchantUserRepo()
	svc := NewAdminBrandService(brands, merchants, audits, users)
	ctx := context.Background()

	seedMerchant(merchants, "m1", "active")
	brands.brands["b1"] = &model.Brand{ID: "b1", MerchantID: "m1", Name: "品牌A", Status: "active"}

	// 范围指向品牌的账号
	users.users["u1"] = &model.MerchantUser{ID: "u1", MerchantID: "m1", ScopeType: "brand", ScopeID: "b1"}

	// 名下有门店 → 拒绝删除
	brands.hasStores["b1"] = true
	if err := svc.Delete(ctx, "b1"); !errors.Is(err, ErrBrandHasStores) {
		t.Fatalf("有门店时应拒绝删除, got %v", err)
	}

	// 无门店时删除成功，账号范围回收为商户级
	brands.hasStores["b1"] = false
	if err := svc.Delete(ctx, "b1"); err != nil {
		t.Fatalf("删除应成功: %v", err)
	}
	if _, ok := brands.brands["b1"]; ok {
		t.Fatal("品牌未被删除")
	}
	if users.users["u1"].ScopeType != "merchant" || users.users["u1"].ScopeID != "" {
		t.Fatalf("账号范围未回收: %+v", users.users["u1"])
	}
}

func TestStoreCreateBrandMismatch(t *testing.T) {
	merchants := newFakeMerchantRepo()
	brands := newFakeBrandRepo()
	stores := newFakeStoreRepo()
	audits := &fakeAuditRepo{}
	users := newFakeMerchantUserRepo()
	svc := NewAdminStoreService(stores, brands, merchants, audits, users)
	ctx := context.Background()

	seedMerchant(merchants, "m1", "active")
	seedMerchant(merchants, "m2", "active")
	brands.brands["b1"] = &model.Brand{ID: "b1", MerchantID: "m1", Name: "品牌A"}

	// 品牌属于 m1，挂到 m2 下 → 拒绝
	if _, err := svc.Create(ctx, StoreInput{MerchantID: "m2", BrandID: "b1", Name: "门店X"}, "admin-1"); !errors.Is(err, ErrStoreBrandMismatch) {
		t.Fatalf("跨商户品牌应拒绝, got %v", err)
	}

	// 正常创建：pending + 审核记录
	s, err := svc.Create(ctx, StoreInput{MerchantID: "m1", BrandID: "b1", Name: "门店A"}, "admin-1")
	if err != nil {
		t.Fatalf("创建门店失败: %v", err)
	}
	if s.Status != "active" || s.AuditStatus != "pending" {
		t.Fatalf("初始状态错误: status=%s audit=%s", s.Status, s.AuditStatus)
	}
	if len(audits.records) != 1 || audits.records[0].Type != "create" {
		t.Fatalf("审核记录未生成: %+v", audits.records)
	}

	// 品牌/名称必填校验由 handler + service 承担
	if _, err := svc.Create(ctx, StoreInput{MerchantID: "m1", BrandID: "b1", Name: ""}, "admin-1"); !errors.Is(err, ErrStoreNameRequired) {
		t.Fatalf("空名称应拒绝, got %v", err)
	}
	if _, err := svc.Create(ctx, StoreInput{MerchantID: "m1", BrandID: "missing", Name: "门店B"}, "admin-1"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("品牌不存在应返回 pgx.ErrNoRows, got %v", err)
	}
}

func TestScopeValidation(t *testing.T) {
	merchants := newFakeMerchantRepo()
	users := newFakeMerchantUserRepo()
	brands := newFakeBrandRepo()
	stores := newFakeStoreRepo()
	svc := NewAdminMerchantService(merchants, users, brands, stores)
	ctx := context.Background()

	seedMerchant(merchants, "m1", "active")
	seedMerchant(merchants, "m2", "active")
	brands.brands["b1"] = &model.Brand{ID: "b1", MerchantID: "m1", Name: "品牌A"}
	stores.stores["s1"] = &model.Store{ID: "s1", MerchantID: "m1", BrandID: "b1", Name: "门店A"}

	// 品牌范围：目标不属于该商户 → 拒绝
	if _, err := svc.CreateUser(ctx, "m2", "u-m2", "pass1234", "", "", "", false, "brand", "b1"); !errors.Is(err, ErrScopeOutOfMerchant) {
		t.Fatalf("跨商户品牌范围应拒绝, got %v", err)
	}
	// 品牌范围：目标不存在 → 拒绝
	if _, err := svc.CreateUser(ctx, "m1", "u-x", "pass1234", "", "", "", false, "brand", "missing"); !errors.Is(err, ErrScopeOutOfMerchant) {
		t.Fatalf("品牌不存在应拒绝, got %v", err)
	}
	// 品牌范围缺 ID → 拒绝
	if _, err := svc.CreateUser(ctx, "m1", "u-y", "pass1234", "", "", "", false, "brand", ""); !errors.Is(err, ErrScopeIDRequired) {
		t.Fatalf("品牌范围缺 ID 应拒绝, got %v", err)
	}
	// 非法范围类型 → 拒绝
	if _, err := svc.CreateUser(ctx, "m1", "u-z", "pass1234", "", "", "", false, "region", "r1"); !errors.Is(err, ErrScopeTypeInvalid) {
		t.Fatalf("非法范围类型应拒绝, got %v", err)
	}
	// 门店范围合法 → 成功
	u, err := svc.CreateUser(ctx, "m1", "store-boss", "pass1234", "店长", "", "", true, "store", "s1")
	if err != nil {
		t.Fatalf("门店范围创建应成功: %v", err)
	}
	if u.ScopeType != "store" || u.ScopeID != "s1" || !u.IsAdmin {
		t.Fatalf("范围未落库: %+v", u)
	}
	// merchant 范围归一化：带多余 scopeID 也清空
	u2, err := svc.CreateUser(ctx, "m1", "boss", "pass1234", "老板", "", "", false, "merchant", "whatever")
	if err != nil {
		t.Fatalf("商户级创建应成功: %v", err)
	}
	if u2.ScopeType != "merchant" || u2.ScopeID != "" {
		t.Fatalf("merchant 范围应归一化: %+v", u2)
	}

	// UpdateUserScope：跨商户拒绝、合法更新成功
	if err := svc.UpdateUserScope(ctx, u.ID, "brand", "b1", true); err != nil {
		t.Fatalf("同商户品牌范围应成功: %v", err)
	}
	if users.users[u.ID].ScopeType != "brand" || users.users[u.ID].ScopeID != "b1" {
		t.Fatalf("范围更新未落库: %+v", users.users[u.ID])
	}
	other := &model.MerchantUser{ID: "u-m2-1", MerchantID: "m2", Username: "m2user", ScopeType: "merchant"}
	users.users[other.ID] = other
	if err := svc.UpdateUserScope(ctx, other.ID, "store", "s1", false); !errors.Is(err, ErrScopeOutOfMerchant) {
		t.Fatalf("跨商户门店范围应拒绝, got %v", err)
	}
}
