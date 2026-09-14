package casbin

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// enforcerTestPool 在一个独立 schema 里建出鉴权用到的几张表，读写都不出这个 schema。
// 与 repository 的 bindingTestPool 同一套做法：没有 TEST_DATABASE_URL 就跳过。
//
// casbin_rule 也建出来：这个包要断言的事情之一就是「它里面有什么都不算数」。
func enforcerTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL policy tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	schema := "casbin_test_" + time.Now().Format("20060102150405") + "_" + uuid.NewString()[:8]
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal("cannot create isolated test schema")
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("cannot remove isolated test schema")
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot create test pool")
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
CREATE TABLE admin_roles (
 id UUID PRIMARY KEY, code TEXT UNIQUE NOT NULL, name TEXT NOT NULL DEFAULT '',
 description TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE admin_permissions (id UUID PRIMARY KEY, code TEXT UNIQUE NOT NULL,
 perm_group TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
 description TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE admin_role_permissions (
 role_id UUID REFERENCES admin_roles(id) ON DELETE CASCADE,
 permission_id UUID REFERENCES admin_permissions(id) ON DELETE CASCADE, PRIMARY KEY(role_id, permission_id));
CREATE TABLE admin_user_role_bindings (
 admin_user_id UUID NOT NULL,
 role_id UUID REFERENCES admin_roles(id) ON DELETE CASCADE, PRIMARY KEY(admin_user_id, role_id));
CREATE TABLE casbin_rule (
 ptype TEXT NOT NULL, v0 TEXT NOT NULL DEFAULT '', v1 TEXT NOT NULL DEFAULT '', v2 TEXT NOT NULL DEFAULT '',
 v3 TEXT NOT NULL DEFAULT '', v4 TEXT NOT NULL DEFAULT '', v5 TEXT NOT NULL DEFAULT '', UNIQUE(ptype,v0,v1,v2,v3,v4,v5));`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func execSQL(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

// TestLoadPolicyDerivesFromRelationalTables 是这次修法的回归测试。
//
// 修法之前策略只从 casbin_rule 加载，而关系的写入方不止一处：迁移与 seed 只写关系表，
// 于是同一个角色在两边可以差出任意条权限（dev 库实测超管 35 vs 13）。这里同时摆出两边
// 不一致的数据，断言关系表说的算、casbin_rule 说的不算。
func TestLoadPolicyDerivesFromRelationalTables(t *testing.T) {
	pool := enforcerTestPool(t)
	roleID, permA, permB, user, stranger := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	execSQL(t, pool, `INSERT INTO admin_roles (id, code) VALUES ($1, 'ops')`, roleID)
	execSQL(t, pool, `INSERT INTO admin_permissions (id, code) VALUES ($1,'admin:brands:view'),($2,'admin:stores:view')`, permA, permB)
	execSQL(t, pool, `INSERT INTO admin_role_permissions (role_id, permission_id) VALUES ($1,$2),($1,$3)`, roleID, permA, permB)
	execSQL(t, pool, `INSERT INTO admin_user_role_bindings (admin_user_id, role_id) VALUES ($1,$2)`, user, roleID)
	// casbin_rule 里留着另一套说法：角色 ops 多一个关系表里没有的权限码，另一个人也挂了 ops。
	execSQL(t, pool, `INSERT INTO casbin_rule (ptype,v0,v1,v2,v3) VALUES ('p','ops','','admin:ghost:view','*'),('g',$1,'ops','','')`, stranger)

	enforcer, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	allow := func(sub, dom, obj string) bool {
		t.Helper()
		ok, err := enforcer.Enforce(sub, dom, obj, "*")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// 关系表里的两条权限都生效。用 casbin_rule 当策略时这两条一条都查不到，
	// 而 RequirePermission 正是按权限码匹配的——那才是「配置了却没权限」的根因。
	for _, code := range []string{"admin:brands:view", "admin:stores:view"} {
		if !allow(user, "", code) {
			t.Errorf("%s: 关系表里绑定的权限没有生效", code)
		}
	}
	// casbin_rule 里的两行一条都不算数：多出来的权限码不授予，
	// 那里挂着的另一个人也没有角色（否则 g 行就是一条绕过绑定表的授权后门）。
	if allow(user, "", "admin:ghost:view") {
		t.Error("casbin_rule 里残留的 p 行被当成了授权")
	}
	if allow(stranger, "", "admin:brands:view") {
		t.Error("casbin_rule 里残留的 g 行被当成了角色绑定")
	}
	// 域不匹配不放行：RequirePermission 传进来的 Tenant 只会是 ""（admin_auth 挡掉了带
	// tenant 的 token），派生出来的规则域也恒为空，两者必须仍然对得上。
	if allow(user, "merchant-1", "admin:brands:view") {
		t.Error("非空域拿到了平台权限")
	}
}

// TestReloadRebuildsFromRelationalTables 覆盖改完之后那一半：仓储层写入 + Reload 必须
// 立刻改变判定结果，否则「改了权限要等重启才生效」。广播的兜底重载走的是同一条路。
func TestReloadRebuildsFromRelationalTables(t *testing.T) {
	pool := enforcerTestPool(t)
	roleID, perm, user := uuid.NewString(), uuid.NewString(), uuid.NewString()
	execSQL(t, pool, `INSERT INTO admin_roles (id, code) VALUES ($1, 'ops')`, roleID)
	execSQL(t, pool, `INSERT INTO admin_permissions (id, code) VALUES ($1,'admin:brands:view')`, perm)

	enforcer, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	allowed := func() bool {
		t.Helper()
		ok, err := enforcer.Enforce(user, "", "admin:brands:view", "*")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if allowed() {
		t.Fatal("没有任何绑定就放行了")
	}

	execSQL(t, pool, `INSERT INTO admin_user_role_bindings (admin_user_id, role_id) VALUES ($1,$2)`, user, roleID)
	execSQL(t, pool, `INSERT INTO admin_role_permissions (role_id, permission_id) VALUES ($1,$2)`, roleID, perm)
	if err := enforcer.Reload(); err != nil {
		t.Fatal(err)
	}
	if !allowed() {
		t.Fatal("Reload 之后新增的绑定没有生效")
	}

	// 换绑（等价于仓储层的整体替换：先删后插）。
	execSQL(t, pool, `DELETE FROM admin_user_role_bindings WHERE admin_user_id = $1`, user)
	if err := enforcer.Reload(); err != nil {
		t.Fatal(err)
	}
	if allowed() {
		t.Fatal("Reload 之后被移除的绑定仍然放行")
	}

	// 删角色要连带掉权限：绑定行靠 ON DELETE CASCADE 消失，派生出来的规则跟着没了。
	execSQL(t, pool, `INSERT INTO admin_user_role_bindings (admin_user_id, role_id) VALUES ($1,$2)`, user, roleID)
	execSQL(t, pool, `DELETE FROM admin_roles WHERE id = $1`, roleID)
	if err := enforcer.Reload(); err != nil {
		t.Fatal(err)
	}
	if allowed() {
		t.Fatal("角色删除后仍然放行")
	}
}
