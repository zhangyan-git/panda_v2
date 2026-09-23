package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
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

func (f *fakeMerchantUserRepo) UpdateScope(_ context.Context, id, scopeType string, scopeIDs []string, isAdmin bool) error {
	u, ok := f.users[id]
	if !ok {
		return pgx.ErrNoRows
	}
	u.ScopeType = scopeType
	u.ScopeIDs = scopeIDs
	u.IsAdmin = isAdmin
	return nil
}

// ResetScopeByTarget 对齐真实仓储那条 SQL：只从数组里摘掉被删的那一个目标，
// 摘空之后才回落到商户级——同档位还有别的目标时，这一档必须原样留着。
func (f *fakeMerchantUserRepo) ResetScopeByTarget(_ context.Context, scopeType, scopeID string) error {
	for _, u := range f.users {
		if u.ScopeType != scopeType || !slices.Contains(u.ScopeIDs, scopeID) {
			continue
		}
		u.ScopeIDs = slices.DeleteFunc(u.ScopeIDs, func(id string) bool { return id == scopeID })
		if len(u.ScopeIDs) == 0 {
			u.ScopeType = "merchant"
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
	// storeBrands 记录门店挂在哪个品牌下，范围展开的 brand 档要用。
	storeBrands map[string]string
	brandNames  map[string]string
	storeNames  map[string]string
	// namesErr 模拟 merchant-service 不可用；nameCalls 记录批量解析次数，
	// 用于断言列表接口只发一次 RPC 而不是每行一次。
	namesErr  error
	nameCalls int
	// listCalls 记录范围展开次数：失败路径上不该发生的那次调用，要靠它才看得见。
	listCalls int
}

func newFakeMerchantResourceAccess() *fakeMerchantResourceAccess {
	return &fakeMerchantResourceAccess{
		brandOwners: map[string]string{}, storeOwners: map[string]string{},
		storeBrands: map[string]string{},
		brandNames:  map[string]string{}, storeNames: map[string]string{},
	}
}

// ListStoreIDs 内存版范围展开：merchant 档给全部点位，brand/store 档取所给目标的并集。
// 认不出的档位返回错误而不是空集——空集是一个正常答案，"这个账号没有点位"
// 与"这次请求答不了"必须在调用方那里分得开。
func (f *fakeMerchantResourceAccess) ListStoreIDs(_ context.Context, merchantID, scopeType string, scopeIDs []string) ([]string, error) {
	f.listCalls++
	if f.namesErr != nil {
		return nil, f.namesErr
	}
	switch scopeType {
	case "merchant", "brand", "store":
	default:
		return nil, errors.New("unknown store scope type")
	}
	// 三档都回落到同一条判定上：「这个点位属于该商户，且落在这一档里」。真实实现是一条
	// 带 merchant_id 交叉校验的 SQL，形状相同——跨商户的目标展开出来必须是空集。
	// 目标可以有多个，展开出来的是它们的并集。
	ids := []string{}
	for store, owner := range f.storeOwners {
		switch {
		case owner != merchantID:
		case scopeType == "store" && !slices.Contains(scopeIDs, store):
		case scopeType == "brand" && !slices.Contains(scopeIDs, f.storeBrands[store]):
		default:
			ids = append(ids, store)
		}
	}
	sort.Strings(ids)
	return ids, nil
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
	if _, err := svc.CreateUser(ctx, "m1", "boss", "pass1234", "老板", "", "", false, "", nil); err != nil {
		t.Fatalf("首次创建应成功: %v", err)
	}
	// 全局查重：换一个商户用同名账号也要拒绝
	merchants.seed("m2", "active")
	if _, err := svc.CreateUser(ctx, "m2", "boss", "pass1234", "另一个老板", "", "", false, "", nil); !errors.Is(err, ErrMerchantUsernameTaken) {
		t.Fatalf("重复 username 应拒绝, got %v", err)
	}
	if _, err := svc.CreateUser(ctx, "missing", "newuser", "pass1234", "", "", "", false, "", nil); !errors.Is(err, pgx.ErrNoRows) {
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
	users.users["u1"] = &model.MerchantUser{ID: "u1", MerchantID: "m1", Username: "brand-boss", ScopeType: "brand", ScopeIDs: []string{"b1"}}
	users.users["u2"] = &model.MerchantUser{ID: "u2", MerchantID: "m1", Username: "store-boss", ScopeType: "store", ScopeIDs: []string{"s1"}}
	users.users["u3"] = &model.MerchantUser{ID: "u3", MerchantID: "m1", Username: "gone-boss", ScopeType: "brand", ScopeIDs: []string{"deleted"}}
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
		got[u.Username] = strings.Join(u.ScopeNames, "、")
	}
	if got["brand-boss"] != "一号品牌" || got["store-boss"] != "一号门店" {
		t.Fatalf("范围名称未填: %v", got)
	}
	// 商户档没有目标，名称是空切片；被删掉的目标占一个空串（位置留着，见 decorateScopeNames）。
	if got["gone-boss"] != "" || got["whole-merchant"] != "" {
		t.Fatalf("已删除范围与商户级范围都应留空: %v", got)
	}
	for _, u := range list {
		if len(u.ScopeNames) != len(u.ScopeIDs) {
			t.Fatalf("%s 的名称应与目标同序等长: %v / %v", u.Username, u.ScopeIDs, u.ScopeNames)
		}
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
	u, err := svc.CreateUser(ctx, "m1", "boss", "pass1234", "老板", "", "", false, "brand", []string{"b1"})
	if err != nil {
		t.Fatalf("创建应成功: %v", err)
	}
	if len(u.ScopeNames) != 0 {
		t.Fatalf("解析失败时应留空, got %v", u.ScopeNames)
	}
}

// 目标被删除时只摘掉那一个：同一档位还有别的目标，这一档必须原样留着；
// 摘到空才回落到商户级（那是「没有目标可锚」唯一安全的收法）。
func TestResetScopeByTargetDropsOnlyTheDeletedTarget(t *testing.T) {
	users := newFakeMerchantUserRepo()
	users.users["brand-user"] = &model.MerchantUser{ID: "brand-user", ScopeType: "brand", ScopeIDs: []string{"same-id"}}
	users.users["store-user"] = &model.MerchantUser{ID: "store-user", ScopeType: "store", ScopeIDs: []string{"same-id"}}
	users.users["multi-brand"] = &model.MerchantUser{ID: "multi-brand", ScopeType: "brand", ScopeIDs: []string{"same-id", "keep"}}

	if err := users.ResetScopeByTarget(context.Background(), "brand", "same-id"); err != nil {
		t.Fatalf("品牌范围回收失败: %v", err)
	}
	if users.users["brand-user"].ScopeType != "merchant" || len(users.users["brand-user"].ScopeIDs) != 0 {
		t.Fatalf("最后一个目标被删后应回落到商户级: %+v", users.users["brand-user"])
	}
	if got := users.users["multi-brand"]; got.ScopeType != "brand" || !reflect.DeepEqual(got.ScopeIDs, []string{"keep"}) {
		t.Fatalf("还有目标时不应降档，只摘掉被删的那个: %+v", got)
	}
	if users.users["store-user"].ScopeType != "store" || !reflect.DeepEqual(users.users["store-user"].ScopeIDs, []string{"same-id"}) {
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
	resources.brandOwners["b2"] = "m1"
	resources.brandOwners["b-other"] = "m2"
	resources.storeOwners["s1"] = "m1"

	// 品牌范围：目标不属于该商户 → 拒绝
	if _, err := svc.CreateUser(ctx, "m2", "u-m2", "pass1234", "", "", "", false, "brand", []string{"b1"}); !errors.Is(err, ErrScopeOutOfMerchant) {
		t.Fatalf("跨商户品牌范围应拒绝, got %v", err)
	}
	// 品牌范围：目标不存在 → 拒绝
	if _, err := svc.CreateUser(ctx, "m1", "u-x", "pass1234", "", "", "", false, "brand", []string{"missing"}); !errors.Is(err, ErrScopeOutOfMerchant) {
		t.Fatalf("品牌不存在应拒绝, got %v", err)
	}
	// 品牌范围缺 ID → 拒绝
	if _, err := svc.CreateUser(ctx, "m1", "u-y", "pass1234", "", "", "", false, "brand", nil); !errors.Is(err, ErrScopeIDRequired) {
		t.Fatalf("品牌范围缺 ID 应拒绝, got %v", err)
	}
	// 多选里混进一个不属于本商户的目标：整条创建都要拒，不能只丢那一个。
	// 丢掉等于把用户勾的范围悄悄缩小，而界面上仍然显示他勾了两个。
	if _, err := svc.CreateUser(ctx, "m1", "u-mix", "pass1234", "", "", "", false, "brand", []string{"b1", "b-other"}); !errors.Is(err, ErrScopeOutOfMerchant) {
		t.Fatalf("多目标里有一个跨商户应整条拒绝, got %v", err)
	}
	// 非法范围类型 → 拒绝
	if _, err := svc.CreateUser(ctx, "m1", "u-z", "pass1234", "", "", "", false, "region", []string{"r1"}); !errors.Is(err, ErrScopeTypeInvalid) {
		t.Fatalf("非法范围类型应拒绝, got %v", err)
	}
	// 门店范围合法 → 成功
	u, err := svc.CreateUser(ctx, "m1", "store-boss", "pass1234", "店长", "", "", true, "store", []string{"s1"})
	if err != nil {
		t.Fatalf("门店范围创建应成功: %v", err)
	}
	if u.ScopeType != "store" || !reflect.DeepEqual(u.ScopeIDs, []string{"s1"}) || !u.IsAdmin {
		t.Fatalf("范围未落库: %+v", u)
	}
	// merchant 范围归一化：带多余目标也清空
	u2, err := svc.CreateUser(ctx, "m1", "boss", "pass1234", "老板", "", "", false, "merchant", []string{"b1"})
	if err != nil {
		t.Fatalf("商户级创建应成功: %v", err)
	}
	if u2.ScopeType != "merchant" || len(u2.ScopeIDs) != 0 {
		t.Fatalf("merchant 范围应归一化: %+v", u2)
	}

	// UpdateUserScope：跨商户拒绝、合法更新成功。多个目标按 id 去重并排序后落库，
	// 这样同一次勾选无论提交顺序如何，落库的行都一样。
	if err := svc.UpdateUserScope(ctx, u.ID, "brand", []string{"b2", "b1", "b1"}, true); err != nil {
		t.Fatalf("同商户品牌范围应成功: %v", err)
	}
	if got := users.users[u.ID]; got.ScopeType != "brand" || !reflect.DeepEqual(got.ScopeIDs, []string{"b1", "b2"}) {
		t.Fatalf("范围更新未落库: %+v", got)
	}
	other := &model.MerchantUser{ID: "u-m2-1", MerchantID: "m2", Username: "m2user", ScopeType: "merchant"}
	users.users[other.ID] = other
	if err := svc.UpdateUserScope(ctx, other.ID, "store", []string{"s1"}, false); !errors.Is(err, ErrScopeOutOfMerchant) {
		t.Fatalf("跨商户门店范围应拒绝, got %v", err)
	}
}
