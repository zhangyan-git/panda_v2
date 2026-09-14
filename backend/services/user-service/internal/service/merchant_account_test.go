package service

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// fakeMerchantUserRepo 内存版 MerchantUserRepository
type fakeMerchantUserRepo struct {
	repository.MerchantUserRepository
	users map[string]*model.MerchantUser
}

func newFakeMerchantUserRepo() *fakeMerchantUserRepo {
	return &fakeMerchantUserRepo{users: map[string]*model.MerchantUser{}}
}

func (f *fakeMerchantUserRepo) FindByUsername(_ context.Context, username string) (*model.MerchantUser, error) {
	for _, u := range f.users {
		if u.Username == username {
			return u, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (f *fakeMerchantUserRepo) FindByID(_ context.Context, id string) (*model.MerchantUser, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return u, nil
}

// FindPage 按 id 排序后再切片，让「哪几条落在这一页」可预测：map 的遍历顺序
// 是随机的，直接切片会让分页用例时对时错。
func (f *fakeMerchantUserRepo) FindPage(_ context.Context, merchantID string, limit, offset int) ([]*model.MerchantUser, error) {
	var all []*model.MerchantUser
	for _, u := range f.users {
		if u.MerchantID == merchantID {
			all = append(all, u)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if offset >= len(all) {
		return nil, nil
	}
	end := offset + limit
	if limit <= 0 || end > len(all) {
		end = len(all)
	}
	return all[offset:end], nil
}

func (f *fakeMerchantUserRepo) Count(_ context.Context, merchantID string) (int64, error) {
	var n int64
	for _, u := range f.users {
		if u.MerchantID == merchantID {
			n++
		}
	}
	return n, nil
}

func (f *fakeMerchantUserRepo) Create(_ context.Context, u *model.MerchantUser) error {
	f.users[u.ID] = u
	return nil
}

// 账号范围回收：品牌/门店删除时把指向它的账号回收为商户级
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

func (f *fakeMerchantUserRepo) ResetScopeByTarget(_ context.Context, scopeType, scopeID string) error {
	for _, u := range f.users {
		if u.ScopeID == scopeID && u.ScopeType == scopeType {
			u.ScopeType = "merchant"
			u.ScopeID = ""
		}
	}
	return nil
}

// fakeMerchantAccess 内存版 MerchantAccessPort：商户状态/名称查询
type fakeMerchantAccess struct {
	statuses map[string]string
	names    map[string]string
}

func newFakeMerchantAccess() *fakeMerchantAccess {
	return &fakeMerchantAccess{statuses: map[string]string{}, names: map[string]string{}}
}

func (f *fakeMerchantAccess) seed(id, status string) {
	f.statuses[id] = status
	f.names[id] = "商户" + id
}

func (f *fakeMerchantAccess) FindStatus(_ context.Context, merchantID string) (string, error) {
	status, ok := f.statuses[merchantID]
	if !ok {
		return "", pgx.ErrNoRows
	}
	return status, nil
}

func (f *fakeMerchantAccess) FindName(_ context.Context, merchantID string) (string, error) {
	name, ok := f.names[merchantID]
	if !ok {
		return "", pgx.ErrNoRows
	}
	return name, nil
}

// fakeMerchantResourceAccess 内存版 MerchantResourceAccess：品牌/门店归属与名称查询
type fakeMerchantResourceAccess struct {
	brandOwners map[string]string
	storeOwners map[string]string
	brandNames  map[string]string
	storeNames  map[string]string
	// namesErr 模拟 merchant-service 不可用；nameCalls 记录批量解析次数，
	// 用于断言列表接口只发一次 RPC 而不是每行一次。
	namesErr  error
	nameCalls int
}

func newFakeMerchantResourceAccess() *fakeMerchantResourceAccess {
	return &fakeMerchantResourceAccess{
		brandOwners: map[string]string{}, storeOwners: map[string]string{},
		brandNames: map[string]string{}, storeNames: map[string]string{},
	}
}

// ScopeNames 只返回确实存在的 id：删除后的范围不在结果里，由调用方留空。
func (f *fakeMerchantResourceAccess) ScopeNames(_ context.Context, brandIDs, storeIDs []string) (map[string]string, map[string]string, error) {
	f.nameCalls++
	if f.namesErr != nil {
		return nil, nil, f.namesErr
	}
	known := func(names map[string]string, ids []string) map[string]string {
		out := map[string]string{}
		for _, id := range ids {
			if name, ok := names[id]; ok {
				out[id] = name
			}
		}
		return out
	}
	return known(f.brandNames, brandIDs), known(f.storeNames, storeIDs), nil
}

func (f *fakeMerchantResourceAccess) FindBrandMerchantID(_ context.Context, brandID string) (string, error) {
	merchantID, ok := f.brandOwners[brandID]
	if !ok {
		return "", pgx.ErrNoRows
	}
	return merchantID, nil
}

func (f *fakeMerchantResourceAccess) FindStoreMerchantID(_ context.Context, storeID string) (string, error) {
	merchantID, ok := f.storeOwners[storeID]
	if !ok {
		return "", pgx.ErrNoRows
	}
	return merchantID, nil
}

func TestMerchantCreateUserDuplicateUsername(t *testing.T) {
	merchants := newFakeMerchantAccess()
	users := newFakeMerchantUserRepo()
	svc := NewMerchantAccountService(merchants, users, newFakeMerchantResourceAccess())
	ctx := context.Background()

	merchants.seed("m1", "active")
	if _, err := svc.CreateUser(ctx, "m1", "boss", "pass1234", "老板", "", "", false, "", ""); err != nil {
		t.Fatalf("首次创建应成功: %v", err)
	}
	// 全局查重：换一个商户用同名账号也要拒绝
	merchants.seed("m2", "active")
	if _, err := svc.CreateUser(ctx, "m2", "boss", "pass1234", "另一个老板", "", "", false, "", ""); !errors.Is(err, ErrMerchantUsernameTaken) {
		t.Fatalf("重复 username 应拒绝, got %v", err)
	}
	if _, err := svc.CreateUser(ctx, "missing", "newuser", "pass1234", "", "", "", false, "", ""); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("商户不存在应返回 pgx.ErrNoRows, got %v", err)
	}
}

// 拆库后范围名称来自 merchant-service：一次列表只发一次批量解析，而不是每行一次；
// 已删除的范围留空而不是让整个列表失败。
func TestListUsersResolvesScopeNamesOverOneCall(t *testing.T) {
	merchants := newFakeMerchantAccess()
	users := newFakeMerchantUserRepo()
	resources := newFakeMerchantResourceAccess()
	svc := NewMerchantAccountService(merchants, users, resources)
	ctx := context.Background()

	merchants.seed("m1", "active")
	resources.brandNames["b1"] = "一号品牌"
	resources.storeNames["s1"] = "一号门店"
	users.users["u1"] = &model.MerchantUser{ID: "u1", MerchantID: "m1", Username: "brand-boss", ScopeType: "brand", ScopeID: "b1"}
	users.users["u2"] = &model.MerchantUser{ID: "u2", MerchantID: "m1", Username: "store-boss", ScopeType: "store", ScopeID: "s1"}
	users.users["u3"] = &model.MerchantUser{ID: "u3", MerchantID: "m1", Username: "gone-boss", ScopeType: "brand", ScopeID: "deleted"}
	users.users["u4"] = &model.MerchantUser{ID: "u4", MerchantID: "m1", Username: "whole-merchant", ScopeType: "merchant"}

	// 一页装得下全部账号：这条用例关心的是范围名称怎么解析，不是分页本身。
	list, total, err := svc.ListUsers(ctx, "m1", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("总数应为 4, got %d", total)
	}
	got := map[string]string{}
	for _, u := range list {
		got[u.Username] = u.ScopeName
	}
	if got["brand-boss"] != "一号品牌" || got["store-boss"] != "一号门店" {
		t.Fatalf("范围名称未填: %v", got)
	}
	if got["gone-boss"] != "" || got["whole-merchant"] != "" {
		t.Fatalf("已删除范围与商户级范围都应留空: %v", got)
	}
	if resources.nameCalls != 1 {
		t.Fatalf("批量解析应只调用一次, got %d", resources.nameCalls)
	}

	// merchant-service 不可用时，列表必须失败而不是悄悄返回空名称。
	resources.namesErr = errors.New("merchant service unavailable")
	if _, _, err := svc.ListUsers(ctx, "m1", 1, 50); err == nil {
		t.Fatal("依赖不可用时应报错")
	}
}

// 创建成功后补名称失败不能把一次已成功的创建报成失败。
func TestCreateUserKeepsSuccessWhenScopeNameFails(t *testing.T) {
	merchants := newFakeMerchantAccess()
	users := newFakeMerchantUserRepo()
	resources := newFakeMerchantResourceAccess()
	svc := NewMerchantAccountService(merchants, users, resources)
	ctx := context.Background()

	merchants.seed("m1", "active")
	resources.brandOwners["b1"] = "m1"
	resources.namesErr = errors.New("merchant service unavailable")
	u, err := svc.CreateUser(ctx, "m1", "boss", "pass1234", "老板", "", "", false, "brand", "b1")
	if err != nil {
		t.Fatalf("创建应成功: %v", err)
	}
	if u.ScopeName != "" {
		t.Fatalf("解析失败时应留空, got %q", u.ScopeName)
	}
}

func TestResetScopeByTargetMatchesType(t *testing.T) {
	users := newFakeMerchantUserRepo()
	users.users["brand-user"] = &model.MerchantUser{ID: "brand-user", ScopeType: "brand", ScopeID: "same-id"}
	users.users["store-user"] = &model.MerchantUser{ID: "store-user", ScopeType: "store", ScopeID: "same-id"}

	if err := users.ResetScopeByTarget(context.Background(), "brand", "same-id"); err != nil {
		t.Fatalf("品牌范围回收失败: %v", err)
	}
	if users.users["brand-user"].ScopeType != "merchant" || users.users["brand-user"].ScopeID != "" {
		t.Fatalf("品牌范围未回收: %+v", users.users["brand-user"])
	}
	if users.users["store-user"].ScopeType != "store" || users.users["store-user"].ScopeID != "same-id" {
		t.Fatalf("跨类型门店范围被错误回收: %+v", users.users["store-user"])
	}
}

func TestScopeValidation(t *testing.T) {
	merchants := newFakeMerchantAccess()
	users := newFakeMerchantUserRepo()
	resources := newFakeMerchantResourceAccess()
	svc := NewMerchantAccountService(merchants, users, resources)
	ctx := context.Background()

	merchants.seed("m1", "active")
	merchants.seed("m2", "active")
	resources.brandOwners["b1"] = "m1"
	resources.storeOwners["s1"] = "m1"

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
