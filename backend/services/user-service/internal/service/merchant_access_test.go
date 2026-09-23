package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// accessFixture 装一条与生产同形的链路：一个 active 商户、一个 active 账号，
// 账号挂在 m1 下的 b1/b2 品牌、b1 有 s1/s2 两个点位、b2 有 s4，m1 另有一个
// 不属任何品牌的 s3。
func accessFixture(scopeType string, scopeIDs ...string) (*MerchantAccessService, *fakeMerchantUserRepo, *fakeMerchantResourceAccess) {
	merchants := newFakeMerchantAccess()
	users := newFakeMerchantUserRepo()
	resources := newFakeMerchantResourceAccess()

	merchants.seed("m1", "active")
	resources.brandOwners["b1"], resources.brandOwners["b2"] = "m1", "m1"
	resources.storeOwners["s1"], resources.storeOwners["s2"], resources.storeOwners["s3"] = "m1", "m1", "m1"
	resources.storeOwners["s4"] = "m1"
	resources.storeBrands["s1"], resources.storeBrands["s2"] = "b1", "b1"
	resources.storeBrands["s4"] = "b2"
	users.users["u1"] = &model.MerchantUser{
		ID: "u1", MerchantID: "m1", Status: "active", ScopeType: scopeType, ScopeIDs: scopeIDs,
	}
	authSvc := NewMerchantAuthService(users, merchants, nil)
	return NewMerchantAccessService(users, authSvc, resources), users, resources
}

// 三档范围必须展开成同一组点位 id：下游只认这一种形状，任何一档走岔都会变成
// 「列表按范围过滤了、详情按别的口径放行」。
func TestResolveExpandsEveryScopeLevelIntoStoreIDs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		scopeType string
		scopeIDs  []string
		want      []string
		wantIDs   []string
	}{
		{name: "merchant", scopeType: "merchant", want: []string{"s1", "s2", "s3", "s4"}},
		{name: "brand", scopeType: "brand", scopeIDs: []string{"b1"}, want: []string{"s1", "s2"}, wantIDs: []string{"b1"}},
		{name: "store", scopeType: "store", scopeIDs: []string{"s2"}, want: []string{"s2"}, wantIDs: []string{"s2"}},
		// 多选：展开出来的是并集，去重后按 id 排序。目标本身也跟着走，
		// 「这个边界有几个来源」不能被展开结果盖掉——两个品牌合起来只落一个点位时，
		// 展开结果看起来像单选，而用户勾的是两个。
		{
			name: "brand over several targets is their union", scopeType: "brand",
			scopeIDs: []string{"b1", "b2"}, want: []string{"s1", "s2", "s4"}, wantIDs: []string{"b1", "b2"},
		},
		{name: "store over several targets is their union", scopeType: "store", scopeIDs: []string{"s2", "s4"}, want: []string{"s2", "s4"}, wantIDs: []string{"s2", "s4"}},
		// 空串是历史行里真实存在的值（列可空）。读成商户档是唯一安全的收法：
		// 它仍被 merchant_id 锚住，而读成「不过滤」就是全平台。
		{name: "blank falls back to merchant", scopeType: "", want: []string{"s1", "s2", "s3", "s4"}},
		// 商户档的 scope_ids 即便库里存了值也不采用。
		{name: "merchant ignores stray targets", scopeType: "merchant", scopeIDs: []string{"b1"}, want: []string{"s1", "s2", "s3", "s4"}},
		// 品牌档但一个目标都没有：展开成空集（而不是回落到全量）。空集是 fail-closed
		// 的那一边，与商家档的「全部门店」差着一整个商户的数据。
		{name: "brand with no target expands to nothing", scopeType: "brand", want: []string{}, wantIDs: []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := accessFixture(tt.scopeType, tt.scopeIDs...)
			access, err := svc.Resolve(context.Background(), "u1", "m1")
			if err != nil {
				t.Fatal(err)
			}
			wantType := tt.scopeType
			if wantType == "" {
				wantType = "merchant"
			}
			wantIDs := tt.wantIDs
			if wantIDs == nil {
				wantIDs = []string{}
			}
			if access.MerchantID != "m1" || access.ScopeType != wantType ||
				!reflect.DeepEqual(access.ScopeIDs, wantIDs) || !reflect.DeepEqual(access.StoreIDs, tt.want) {
				t.Fatalf("边界不对: %+v", access)
			}
			if access.StoreIDs == nil {
				t.Fatal("空范围必须是空切片而不是 nil")
			}
		})
	}
}

// 展开不出来必须原样报错。降级成空集会让界面显示成「你没有点位」，降级成全量则直接
// 越权——两条都在这里钉死。
func TestResolveNeverDegradesAFailedExpansion(t *testing.T) {
	svc, _, resources := accessFixture("brand", "b1")
	resources.namesErr = errors.New("merchant service unavailable")

	access, err := svc.Resolve(context.Background(), "u1", "m1")
	if err == nil {
		t.Fatalf("依赖不可用时应报错, got %+v", access)
	}
	if !reflect.DeepEqual(access, MerchantAccess{}) {
		t.Fatalf("报错时不应回半个边界: %+v", access)
	}
}

// 认不出的档位既不能当商户全量也不能当空集，而且不该白跑一次范围展开。
func TestResolveRejectsUnknownScopeTypeBeforeExpanding(t *testing.T) {
	svc, _, resources := accessFixture("region", "r1")

	if _, err := svc.Resolve(context.Background(), "u1", "m1"); !errors.Is(err, ErrScopeTypeInvalid) {
		t.Fatalf("未知档位应报 ErrScopeTypeInvalid, got %v", err)
	}
	if resources.listCalls != 0 {
		t.Fatalf("未知档位不应触发范围展开, got %d", resources.listCalls)
	}
}

// 账号行与身份对不上时一律拒绝：行没了、id 变了、不属于本商户。
func TestResolveRejectsMismatchedAccounts(t *testing.T) {
	for _, tt := range []struct {
		name   string
		setup  func(users *fakeMerchantUserRepo)
		userID string
		tenant string
		want   error
	}{
		{name: "account deleted", setup: func(u *fakeMerchantUserRepo) { delete(u.users, "u1") }, userID: "u1", tenant: "m1", want: pgx.ErrNoRows},
		{name: "blank merchant id", setup: func(u *fakeMerchantUserRepo) { u.users["u1"].MerchantID = " " }, userID: "u1", tenant: "m1", want: ErrMerchantAccountMismatch},
		{name: "tenant mismatch", setup: func(u *fakeMerchantUserRepo) {}, userID: "u1", tenant: "m2", want: ErrMerchantAccountMismatch},
		{name: "account disabled", setup: func(u *fakeMerchantUserRepo) { u.users["u1"].Status = "disabled" }, userID: "u1", tenant: "m1", want: ErrMerchantUserDisabled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, users, resources := accessFixture("merchant", "")
			tt.setup(users)

			if _, err := svc.Resolve(context.Background(), tt.userID, tt.tenant); !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
			if resources.listCalls != 0 {
				t.Fatalf("被拒的账号不应触发范围展开, got %d", resources.listCalls)
			}
		})
	}
}

// rowReturningRepo 不管问谁，都把同一行返回来——模拟一个忽略了入参的仓储实现。
type rowReturningRepo struct {
	repository.MerchantUserRepository
	user *model.MerchantUser
}

func (f *rowReturningRepo) FindByID(_ context.Context, _ string) (*model.MerchantUser, error) {
	return f.user, nil
}

// 拿到的行不是被问的那一行时必须拒绝。真实仓储按 id 查不会出现这种情况，这一条守的是
// 「边界锚在谁身上」：锚错了就是拿别人的范围去放行。
func TestResolveRejectsAnAccountRowThatIsNotTheRequestedOne(t *testing.T) {
	merchants := newFakeMerchantAccess()
	merchants.seed("m1", "active")
	resources := newFakeMerchantResourceAccess()
	users := &rowReturningRepo{user: &model.MerchantUser{ID: "other", MerchantID: "m1", Status: "active"}}
	svc := NewMerchantAccessService(users, NewMerchantAuthService(users, merchants, nil), resources)

	if _, err := svc.Resolve(context.Background(), "u1", "m1"); !errors.Is(err, ErrMerchantAccountMismatch) {
		t.Fatalf("want ErrMerchantAccountMismatch, got %v", err)
	}
	if resources.listCalls != 0 {
		t.Fatalf("对不上的账号不应触发范围展开, got %d", resources.listCalls)
	}
}

// 商户主体被暂停时，已经登录的账号必须在下一个请求上就失去边界——这正是范围不签进
// 令牌的意义。
func TestResolveFollowsTheMerchantStatusEveryTime(t *testing.T) {
	merchants := newFakeMerchantAccess()
	users := newFakeMerchantUserRepo()
	resources := newFakeMerchantResourceAccess()
	merchants.seed("m1", "active")
	resources.storeOwners["s1"] = "m1"
	users.users["u1"] = &model.MerchantUser{ID: "u1", MerchantID: "m1", Status: "active", ScopeType: "merchant"}
	svc := NewMerchantAccessService(users, NewMerchantAuthService(users, merchants, nil), resources)

	if _, err := svc.Resolve(context.Background(), "u1", "m1"); err != nil {
		t.Fatal(err)
	}
	merchants.statuses["m1"] = "suspended"
	if _, err := svc.Resolve(context.Background(), "u1", "m1"); !errors.Is(err, ErrMerchantSuspended) {
		t.Fatalf("商户暂停后应立即失去边界, got %v", err)
	}
}

// 空集是一个正常答案，nil 不是：下游的过滤谓词判的正是这两者的差别，nil 在那里
// 意味着「不过滤」。服务层必须在这里收口。
func TestResolveTurnsNilStoreIDsIntoAnEmptySlice(t *testing.T) {
	svc, _, _ := accessFixture("brand", "empty-brand")
	access, err := svc.Resolve(context.Background(), "u1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if access.StoreIDs == nil || len(access.StoreIDs) != 0 {
		t.Fatalf("空范围应是空切片而不是 nil: %#v", access.StoreIDs)
	}
}

// ScopeOf 是给 /users/me 用的：那条路径自己做过账号与商户状态判定。这里钉住它是
// **不重复判定**的——否则 Me 会拿同一个答案判两次，两份判定迟早分叉。
func TestScopeOfDoesNotRecheckTheAccount(t *testing.T) {
	svc, users, _ := accessFixture("merchant", "")
	users.users["u1"].Status = "disabled"

	if _, err := svc.Resolve(context.Background(), "u1", "m1"); !errors.Is(err, ErrMerchantUserDisabled) {
		t.Fatalf("Resolve 应做状态判定, got %v", err)
	}
	access, err := svc.ScopeOf(context.Background(), users.users["u1"])
	if err != nil {
		t.Fatalf("ScopeOf 不应重复判定: %v", err)
	}
	if !reflect.DeepEqual(access.StoreIDs, []string{"s1", "s2", "s3", "s4"}) {
		t.Fatalf("ScopeOf 应照常展开: %+v", access)
	}
	// 导出的入口自己也要能挡住「没有商户可锚」的入参。
	if _, err := svc.ScopeOf(context.Background(), nil); !errors.Is(err, ErrMerchantAccountMismatch) {
		t.Fatalf("nil 账号应被拒, got %v", err)
	}
}

// 商户端要显示「你的数据范围」。商户档没有目标可解析，回空切片由前端配文案；
// 范围目标被删掉时留一个空串占位——展示数据不该把一次登录态查询变成错误。
func TestMerchantScopeNames(t *testing.T) {
	svc, _, resources := accessFixture("brand", "b1")
	resources.brandNames["b1"] = "一号品牌"
	resources.brandNames["b2"] = "二号品牌"
	resources.storeNames["s1"] = "一号门店"

	for _, tt := range []struct {
		name  string
		scope MerchantAccess
		want  []string
	}{
		{name: "merchant tier has no target", scope: MerchantAccess{ScopeType: "merchant", ScopeIDs: []string{}}, want: []string{}},
		{name: "brand", scope: MerchantAccess{ScopeType: "brand", ScopeIDs: []string{"b1"}}, want: []string{"一号品牌"}},
		{name: "store", scope: MerchantAccess{ScopeType: "store", ScopeIDs: []string{"s1", "s2"}}, want: []string{"一号门店", ""}},
		{
			name:  "several targets keep their order",
			scope: MerchantAccess{ScopeType: "brand", ScopeIDs: []string{"b1", "gone", "b2"}},
			want:  []string{"一号品牌", "", "二号品牌"},
		},
		{
			// 名称与目标必须同序等长：少一项就会让「b2 的名字」贴到「b1 这个 id」上，
			// 界面上显示的范围与实际过滤的范围从此对不上。
			name:  "deleted target keeps its slot",
			scope: MerchantAccess{ScopeType: "brand", ScopeIDs: []string{"gone"}},
			want:  []string{""},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc.MerchantScopeNames(context.Background(), tt.scope)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q; want %q", got, tt.want)
			}
			if len(got) != len(tt.scope.ScopeIDs) {
				t.Fatalf("名称应与目标同序等长: %v / %v", tt.scope.ScopeIDs, got)
			}
		})
	}

	if _, err := svc.MerchantScopeNames(context.Background(), MerchantAccess{ScopeType: "region"}); !errors.Is(err, ErrScopeTypeInvalid) {
		t.Fatalf("未知档位应报错, got %v", err)
	}
}
