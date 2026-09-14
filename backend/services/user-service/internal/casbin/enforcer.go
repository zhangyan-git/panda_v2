package casbin

import (
	"context"
	"fmt"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Model is the RBAC model definition for Casbin.
// p = policy: sub (user/role), dom (tenant/""), obj (resource), act (action)
// g = role inheritance: user, role。平台单域，domain 恒为 ""，
// 由 matcher 里的 r.dom == p.dom 承担域匹配，g 只保留两元。
const rbacModel = `
[request_definition]
r = sub, dom, obj, act

[policy_definition]
p = sub, dom, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.dom == p.dom && r.obj == p.obj && (p.act == "*" || r.act == p.act)
`

// pgAdapter 从关系表派生策略，只读。
//
// 事实来源是 admin_role_permissions（p：角色 → 权限码）与 admin_user_role_bindings
// （g：用户 → 角色）。casbin_rule 既不是读的来源、也不再被任何代码写入：历史上两边都
// 写，但写入方不一致——迁移与 cmd/seed 只写关系表，业务写入两边都写——于是两边会漂移。
// dev 库实测超管在关系表里绑了 35 个权限码，casbin_rule 里只有 13 个，非超管的权限
// 判定因此比配置少 22 个码。从关系表派生之后这条漂移路径不复存在。
type pgAdapter struct {
	pool *pgxpool.Pool
}

// rolePermissionsQuery 取角色 → 权限码。dom 恒为空：平台单域，RequirePermission 传进来
// 的 identity.Tenant 只可能是 ""（带 tenant 的 token 在 admin_auth 就被挡在平台接口外）。
// act 恒为 "*"：授权以权限码（obj）为粒度整体授予，动作语义由码自身承载。
const rolePermissionsQuery = `
	SELECT r.code, p.code
	FROM admin_role_permissions rp
	JOIN admin_roles r ON r.id = rp.role_id
	JOIN admin_permissions p ON p.id = rp.permission_id`

// userRolesQuery 取用户 → 角色。
const userRolesQuery = `
	SELECT b.admin_user_id::text, r.code
	FROM admin_user_role_bindings b
	JOIN admin_roles r ON r.id = b.role_id`

func (a *pgAdapter) LoadPolicy(m model.Model) error {
	if err := a.loadRolePermissions(m); err != nil {
		return err
	}
	return a.loadUserRoles(m)
}

func (a *pgAdapter) loadRolePermissions(m model.Model) error {
	rows, err := a.pool.Query(context.Background(), rolePermissionsQuery)
	if err != nil {
		return fmt.Errorf("casbin load policy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var roleCode, code string
		if err := rows.Scan(&roleCode, &code); err != nil {
			return fmt.Errorf("casbin scan policy: %w", err)
		}
		// 用 LoadPolicyArray 而不是拼 CSV 行：角色码/权限码里出现逗号时拼行会被
		// 当成字段分隔符切开。Array 版本直接给字段，没有这层转义问题。
		if err := persist.LoadPolicyArray([]string{"p", roleCode, "", code, "*"}, m); err != nil {
			return fmt.Errorf("casbin load p %s/%s: %w", roleCode, code, err)
		}
	}
	return rows.Err()
}

func (a *pgAdapter) loadUserRoles(m model.Model) error {
	rows, err := a.pool.Query(context.Background(), userRolesQuery)
	if err != nil {
		return fmt.Errorf("casbin load grouping policy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var userID, roleCode string
		if err := rows.Scan(&userID, &roleCode); err != nil {
			return fmt.Errorf("casbin scan grouping policy: %w", err)
		}
		if err := persist.LoadPolicyArray([]string{"g", userID, roleCode}, m); err != nil {
			return fmt.Errorf("casbin load g %s/%s: %w", userID, roleCode, err)
		}
	}
	return rows.Err()
}

// SavePolicy / AddPolicy / RemovePolicy / RemoveFilteredPolicy 都是空实现：这个 adapter
// 只读，策略的写入路径是仓储层，改完由 Broadcaster 触发 Reload。casbin 的 auto-save
// 因此永远没有落点——这是有意的，别在这里补实现。
func (a *pgAdapter) SavePolicy(model.Model) error { return nil }

func (a *pgAdapter) AddPolicy(string, string, []string) error    { return nil }
func (a *pgAdapter) RemovePolicy(string, string, []string) error { return nil }
func (a *pgAdapter) RemoveFilteredPolicy(string, string, int, ...string) error {
	return nil
}

// Enforcer wraps casbin.Enforcer and adds a Reload method for after policy changes.
type Enforcer struct {
	e  *casbin.Enforcer
	mu sync.RWMutex
}

// New creates an Enforcer backed by the given pgx pool.
func New(pool *pgxpool.Pool) (*Enforcer, error) {
	m, err := model.NewModelFromString(rbacModel)
	if err != nil {
		return nil, fmt.Errorf("casbin model: %w", err)
	}
	adapter := &pgAdapter{pool: pool}
	e, err := casbin.NewEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("casbin enforcer: %w", err)
	}
	return &Enforcer{e: e}, nil
}

// Enforce returns true if subject sub in domain dom may perform act on obj.
func (e *Enforcer) Enforce(sub, dom, obj, act string) (bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.e.Enforce(sub, dom, obj, act)
}

// Reload re-reads all policies from the database. Call this after policy changes.
func (e *Enforcer) Reload() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.e.LoadPolicy()
}

// 这里曾经有 AddRoleForUser / DeleteRoleForUser 两个包装：它们改的是进程内策略，
// 既不落库也不加锁（Enforce 拿 RLock，它们什么都不拿），而且没有任何调用方——授权变更
// 只走仓储层 + Reload。留着就是给未来的人准备一条绕开持久化的捷径，已删。
