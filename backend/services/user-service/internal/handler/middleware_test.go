package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// permissionRoles 是鉴权的角色来源。calls 用来证明每次请求都重新问过库，
// 而不是复用某次缓存或 token 里的快照。
type permissionRoles struct {
	roles []*model.AdminRole
	err   error
	calls int
}

func (f *permissionRoles) FindRolesByUser(context.Context, string) ([]*model.AdminRole, error) {
	f.calls++
	return f.roles, f.err
}

// permissionPolicy 记录 Enforce 收到什么、回什么。超管放行时 calls 必须为 0：
// 那条路径根本不查策略表，这正是「超管只要 token 有效就全权限」的实现方式。
type permissionPolicy struct {
	allow bool
	err   error
	calls int
	sub   string
}

func (f *permissionPolicy) Enforce(sub, dom, obj, act string) (bool, error) {
	f.calls++
	f.sub = sub
	return f.allow, f.err
}

// permissionAccounts 是账号状态的来源。calls 用来证明「停用」不需要等 token 过期：
// 每次请求都重新问一次库，同一个 identity 上一秒能过、下一秒就被挡住。
type permissionAccounts struct {
	user  *model.AdminUser
	err   error
	calls int
}

func (f *permissionAccounts) FindByID(_ context.Context, id string) (*model.AdminUser, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.user == nil {
		return nil, pgx.ErrNoRows
	}
	return f.user, nil
}

const testPermissionCode = "admin:roles:view"

// activeAccounts 是这些用例的默认账号来源：账号存在且启用，判定的差异才来自角色与策略。
func activeAccounts() *permissionAccounts {
	return &permissionAccounts{user: &model.AdminUser{ID: "admin", Status: "active"}}
}

// runPermission 按 cmd/main.go 里的接线方式组装最内层中间件：鉴权跑在认证之后，
// 所以这里直接把 identity 注入请求上下文，跳过认证这一层。
func runPermission(t *testing.T, policy PolicyEnforcer, roles RoleFinder, accounts AccountFinder, identity *auth.Identity) *httptest.ResponseRecorder {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := RequirePermission(policy, roles, accounts, testPermissionCode)(next)
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	if identity != nil {
		r = r.WithContext(auth.WithIdentity(r.Context(), *identity))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// C 端 token 的形状与平台管理员完全一致——subject 等于 user id、没有 tenant——
// 所以这条用例锁住 realm 是唯一的分水岭：策略就算放行、账号就算查得到也不能过。
// 挡在 Enforce 之前是对的：拿一个 C 端 id 去 admin_users 里查本身就是错的。
func TestRequirePermissionRejectsAConsumerIdentity(t *testing.T) {
	policy := &permissionPolicy{allow: true}
	accounts := &permissionAccounts{user: &model.AdminUser{ID: "user-1", Status: "active"}}
	roles := &permissionRoles{roles: []*model.AdminRole{{ID: "r1", Code: "operator"}}}

	w := runPermission(t, policy, roles, accounts,
		&auth.Identity{Realm: auth.RealmConsumer, Subject: "user-1", UserID: "user-1"})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", w.Code, w.Body)
	}
	if policy.calls != 0 {
		t.Fatalf("Enforce calls = %d, want 0", policy.calls)
	}
	if accounts.calls != 0 {
		t.Fatalf("账号查询 %d 次，want 0：C 端 id 不该拿去查 admin_users", accounts.calls)
	}
}

// 超管只认库里的角色：token 里 IsSuper=false（甚至已经过期成普通身份）也放行，
// 因为放行的依据是 admin_user_role_bindings 的当下内容。
func TestRequirePermissionSuperAdminBypassesPolicy(t *testing.T) {
	for _, tt := range []struct {
		name string
		code string
	}{
		{name: "super_admin", code: model.SuperRoleCode},
		{name: "legacy spelling", code: "超级管理员"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			roles := &permissionRoles{roles: []*model.AdminRole{{ID: "r1", Code: tt.code}}}
			// 策略里刻意什么都没有：超管能过必须来自角色，不能来自 casbin_rule。
			policy := &permissionPolicy{allow: false}
			identity := &auth.Identity{Subject: "admin", UserID: "admin", IsSuper: false}

			if w := runPermission(t, policy, roles, activeAccounts(), identity); w.Code != http.StatusNoContent {
				t.Fatalf("status=%d want 204 body=%s", w.Code, w.Body)
			}
			if policy.calls != 0 {
				t.Fatalf("Enforce calls = %d, want 0: 超管不查策略表", policy.calls)
			}
			if roles.calls != 1 {
				t.Fatalf("FindRolesByUser calls = %d, want 1", roles.calls)
			}
		})
	}
}

// token 说自己还是超管，但库里已经不是了——这就是「降级」：同一枚 token 立即失效，
// 不需要重新登录、也不用等 24 小时。
func TestRequirePermissionDowngradeTakesEffectOnSameToken(t *testing.T) {
	roles := &permissionRoles{roles: []*model.AdminRole{{ID: "r1", Code: model.SuperRoleCode}}}
	policy := &permissionPolicy{allow: false}
	// 整场测试都用同一个 identity：它扮演那枚一直有效的 token。
	identity := &auth.Identity{Subject: "admin", UserID: "admin", IsSuper: true, Roles: []string{model.SuperRoleCode}}

	if w := runPermission(t, policy, roles, activeAccounts(), identity); w.Code != http.StatusNoContent {
		t.Fatalf("降级前 status=%d want 204 body=%s", w.Code, w.Body)
	}
	if policy.calls != 0 {
		t.Fatalf("降级前 Enforce calls = %d, want 0", policy.calls)
	}

	// 解绑超管角色：token 一个字节没变，库里的绑定变了。
	roles.roles = nil
	if w := runPermission(t, policy, roles, activeAccounts(), identity); w.Code != http.StatusForbidden {
		t.Fatalf("降级后 status=%d want 403 body=%s", w.Code, w.Body)
	}
	if policy.calls != 1 {
		t.Fatalf("降级后 Enforce calls = %d, want 1: 降级后要走策略判定", policy.calls)
	}
	if roles.calls != 2 {
		t.Fatalf("FindRolesByUser calls = %d, want 2: 每次请求都要重新查", roles.calls)
	}
}

// 非超管走策略判定，参数按 (user, tenant, code, "*") 传——act 恒为 "*"，
// 与 casbin_rule 里 p 行的写法一致。
func TestRequirePermissionEnforcesForNonSuper(t *testing.T) {
	tests := []struct {
		name   string
		policy *permissionPolicy
		roles  *permissionRoles
		want   int
	}{
		{
			name:   "granted",
			policy: &permissionPolicy{allow: true},
			roles:  &permissionRoles{roles: []*model.AdminRole{{ID: "r1", Code: "operator"}}},
			want:   http.StatusNoContent,
		},
		{
			name:   "denied",
			policy: &permissionPolicy{allow: false},
			roles:  &permissionRoles{roles: []*model.AdminRole{{ID: "r1", Code: "operator"}}},
			want:   http.StatusForbidden,
		},
		{
			name:   "no roles and no policy",
			policy: &permissionPolicy{allow: false},
			roles:  &permissionRoles{},
			want:   http.StatusForbidden,
		},
		{
			name:   "policy error",
			policy: &permissionPolicy{err: errors.New("casbin load policy failed")},
			roles:  &permissionRoles{roles: []*model.AdminRole{{ID: "r1", Code: "operator"}}},
			want:   http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 无 tenant 的平台身份：这条中间件只挂在 /v1/admin/* 上，非平台身份
			// 在进 Enforce 之前就被挡掉了，带 tenant 的 identity 到不了这里。
			identity := &auth.Identity{Subject: "admin", UserID: "admin",
				Roles: []string{model.SuperRoleCode}, IsSuper: true}

			if w := runPermission(t, tt.policy, tt.roles, activeAccounts(), identity); w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if tt.policy.calls != 1 {
				t.Fatalf("Enforce calls = %d, want 1", tt.policy.calls)
			}
			// 每行用例的 token 都自称超管且带超管角色，但库里没有——不能因此放行，
			// 判定必须落到策略上，且主体用的是实时身份里的 user id。
			if tt.policy.sub != "admin" {
				t.Fatalf("Enforce sub = %q, want the live user id", tt.policy.sub)
			}
		})
	}
}

// 角色查不出来就什么都判不了：不当作普通用户（那会让超管莫名 403），也不放行。
// 关键是这里不能退回 token 的 claims——token 里写着超级管理员也一样 503。
func TestRequirePermissionFailsClosedWhenRolesUnavailable(t *testing.T) {
	roles := &permissionRoles{err: errors.New("database unavailable")}
	policy := &permissionPolicy{allow: true}
	identity := &auth.Identity{Subject: "admin", UserID: "admin",
		Roles: []string{model.SuperRoleCode}, IsSuper: true}

	w := runPermission(t, policy, roles, activeAccounts(), identity)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", w.Code, w.Body)
	}
	if policy.calls != 0 {
		t.Fatalf("Enforce calls = %d, want 0: 角色未确定时不该去查策略", policy.calls)
	}
}

// 没有身份就没有可判的东西；未认证不该走到角色查询。
func TestRequirePermissionRejectsUnidentified(t *testing.T) {
	roles := &permissionRoles{}
	policy := &permissionPolicy{allow: true}

	if w := runPermission(t, policy, roles, activeAccounts(), nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", w.Code, w.Body)
	}
	if roles.calls != 0 {
		t.Fatalf("FindRolesByUser calls = %d, want 0", roles.calls)
	}
	if policy.calls != 0 {
		t.Fatalf("Enforce calls = %d, want 0", policy.calls)
	}
}

// 停用账号立刻失去权限，且对超管同样成立：token 是 24 小时有效的，停用却不能等它过期。
// 这个检查必须在超管短路之前，否则「停用一个超管」在旧 token 到期前毫无作用。
func TestRequirePermissionRejectsDisabledAccount(t *testing.T) {
	for _, tt := range []struct {
		name   string
		roles  *model.AdminRole
		status string
		want   int
	}{
		{name: "disabled super admin", roles: &model.AdminRole{ID: "r1", Code: model.SuperRoleCode}, status: "disabled", want: http.StatusForbidden},
		{name: "disabled operator", roles: &model.AdminRole{ID: "r1", Code: "operator"}, status: "blocked", want: http.StatusForbidden},
		{name: "active operator still goes to the policy", roles: &model.AdminRole{ID: "r1", Code: "operator"}, status: "active", want: http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			roles := &permissionRoles{roles: []*model.AdminRole{tt.roles}}
			// 策略一律放行：被挡住只能来自账号状态，不能是策略碰巧说不行。
			policy := &permissionPolicy{allow: true}
			accounts := &permissionAccounts{user: &model.AdminUser{ID: "admin", Status: tt.status}}
			identity := &auth.Identity{Subject: "admin", UserID: "admin", IsSuper: true, Roles: []string{model.SuperRoleCode}}

			if w := runPermission(t, policy, roles, accounts, identity); w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if accounts.calls != 1 {
				t.Fatalf("FindByID calls = %d, want 1: 每次请求都要重新查账号状态", accounts.calls)
			}
			if tt.want == http.StatusForbidden {
				// 被停用时既不查角色也不查策略：状态是判定的一部分，不是提示信息。
				if roles.calls != 0 || policy.calls != 0 {
					t.Fatalf("roles calls=%d policy calls=%d, want 0/0", roles.calls, policy.calls)
				}
			}
		})
	}
}

// 账号查不出来（库不可用）失败关闭，账号已不存在（被删）当未登录——
// 两者都不许退回 token 里的角色/权限快照。
func TestRequirePermissionFailsClosedOnAccountLookup(t *testing.T) {
	identity := &auth.Identity{Subject: "admin", UserID: "admin", IsSuper: true, Roles: []string{model.SuperRoleCode}}
	for _, tt := range []struct {
		name     string
		accounts *permissionAccounts
		want     int
	}{
		{name: "lookup failed", accounts: &permissionAccounts{err: errors.New("database unavailable")}, want: http.StatusServiceUnavailable},
		{name: "account deleted", accounts: &permissionAccounts{}, want: http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			roles := &permissionRoles{roles: []*model.AdminRole{{ID: "r1", Code: model.SuperRoleCode}}}
			policy := &permissionPolicy{allow: true}

			if w := runPermission(t, policy, roles, tt.accounts, identity); w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if roles.calls != 0 || policy.calls != 0 {
				t.Fatalf("roles calls=%d policy calls=%d, want 0/0", roles.calls, policy.calls)
			}
		})
	}
}
