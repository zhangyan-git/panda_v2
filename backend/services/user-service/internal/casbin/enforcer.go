package casbin

import (
	"context"
	"fmt"
	"strings"

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

// pgAdapter reads policies directly from casbin_rule table (read-only at startup).
// Policy changes go through the binding service and sync to casbin via Reload.
type pgAdapter struct {
	pool *pgxpool.Pool
}

func (a *pgAdapter) LoadPolicy(m model.Model) error {
	const q = `SELECT ptype, v0, v1, v2, v3, v4, v5 FROM casbin_rule`
	rows, err := a.pool.Query(context.Background(), q)
	if err != nil {
		return fmt.Errorf("casbin load policy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ptype, v0, v1, v2, v3, v4, v5 string
		if err := rows.Scan(&ptype, &v0, &v1, &v2, &v3, &v4, &v5); err != nil {
			return fmt.Errorf("casbin scan rule: %w", err)
		}
		// Build CSV line: include all non-empty fields, but preserve fields in
		// the middle even if they are empty (e.g. domain = ""), stopping only
		// once all remaining fields are empty.
		fields := []string{v0, v1, v2, v3, v4, v5}
		last := -1
		for i, v := range fields {
			if v != "" {
				last = i
			}
		}
		parts := make([]string, 0, last+2)
		parts = append(parts, ptype)
		for i := 0; i <= last; i++ {
			parts = append(parts, fields[i])
		}
		line := strings.Join(parts, ", ")
		if err := persist.LoadPolicyLine(line, m); err != nil {
			return fmt.Errorf("casbin load line %q: %w", line, err)
		}
	}
	return rows.Err()
}

// SavePolicy is not used; mutations go through the binding service.
func (a *pgAdapter) SavePolicy(model.Model) error { return nil }

// AddPolicy / RemovePolicy are not used; mutations go through the binding service.
func (a *pgAdapter) AddPolicy(string, string, []string) error    { return nil }
func (a *pgAdapter) RemovePolicy(string, string, []string) error { return nil }
func (a *pgAdapter) RemoveFilteredPolicy(string, string, int, ...string) error {
	return nil
}

// Enforcer wraps casbin.Enforcer and adds a Reload method for after policy changes.
type Enforcer struct {
	e *casbin.Enforcer
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
	return e.e.Enforce(sub, dom, obj, act)
}

// Reload re-reads all policies from the database. Call this after policy changes.
func (e *Enforcer) Reload() error {
	return e.e.LoadPolicy()
}

// AddRoleForUser adds a role for user.
func (e *Enforcer) AddRoleForUser(user, role string) error {
	_, err := e.e.AddRoleForUser(user, role)
	return err
}

// DeleteRoleForUser removes a role from user.
func (e *Enforcer) DeleteRoleForUser(user, role string) error {
	_, err := e.e.DeleteRoleForUser(user, role)
	return err
}
