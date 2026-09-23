package repository

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/casbin"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// 显式测试连接；只在独立临时 schema 内建表，不读写业务表。
func bindingTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL binding integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	schema := "binding_test_" + time.Now().Format("20060102150405") + "_" + uuid.NewString()[:8]
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
-- 列要够 Update/selectPermissionForUpdate 用（它们读写 name/description/perm_group），
-- 否则权限改名那条路径在测试里直接报缺列，而不是报出真正的行为。
CREATE TABLE admin_permissions (id UUID PRIMARY KEY, code TEXT UNIQUE NOT NULL,
 perm_group TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
 description TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now());
-- username 会被审计快照读出来（TargetName 用），不是装饰列。
CREATE TABLE admin_users (id UUID PRIMARY KEY, username TEXT);
-- 级联与真实迁移（migrations/identity 里这两张绑定表上的两个外键）保持一致：
-- 删角色/权限时绑定行是靠 ON DELETE CASCADE 跟着走的，如果这里不写，
-- 删除路径会因为外键报错而根本跑不起来，删除相关的 bug 也就永远测不出来。
CREATE TABLE admin_role_permissions (
 role_id UUID REFERENCES admin_roles(id) ON DELETE CASCADE,
 permission_id UUID REFERENCES admin_permissions(id) ON DELETE CASCADE, PRIMARY KEY(role_id, permission_id));
CREATE TABLE admin_user_role_bindings (
 admin_user_id UUID REFERENCES admin_users(id),
 role_id UUID REFERENCES admin_roles(id) ON DELETE CASCADE, PRIMARY KEY(admin_user_id, role_id));
CREATE TABLE casbin_rule (
 ptype TEXT NOT NULL, v0 TEXT NOT NULL DEFAULT '', v1 TEXT NOT NULL DEFAULT '', v2 TEXT NOT NULL DEFAULT '',
 v3 TEXT NOT NULL DEFAULT '', v4 TEXT NOT NULL DEFAULT '', v5 TEXT NOT NULL DEFAULT '', UNIQUE(ptype,v0,v1,v2,v3,v4,v5));
-- 只列出 messaging.PostgreSQL.Append 会碰到的列：这份 schema 是手写的，
-- 目的是验证审计追加与业务写入同事务，不是复刻迁移文件。但「只列会碰到的列」
-- 必须真的是那几列——Append 的 VALUES 里带 created_at，少一列就是这条 SQL 在
-- 测试里直接报缺列，而它报出来的位置是审计写入，看着像审计坏了。已按
-- platform/messaging/postgres.go 的 INSERT 列表逐列对过（与 outbox_order_test 那份
-- outboxDDL 同源）。
CREATE TABLE message_outbox (
 event_id TEXT PRIMARY KEY, event_type TEXT NOT NULL DEFAULT '', event_version TEXT NOT NULL DEFAULT '',
 trace_id TEXT NOT NULL DEFAULT '', payload BYTEA, created_at TIMESTAMPTZ NOT NULL DEFAULT now());`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestBindingReplacement(t *testing.T) {
	pool := bindingTestPool(t)
	ctx := context.Background()
	repo := NewAdminBindingRepository(pool, audit.NewRecorder())
	r1, r2, p1, p2, u1, u2 := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO admin_roles (id, code) VALUES ($1,'r1'),($2,'r2')`, r1, r2)
	exec(`INSERT INTO admin_permissions VALUES ($1,'read'),($2,'write')`, p1, p2)
	exec(`INSERT INTO admin_users VALUES ($1),($2)`, u1, u2)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	values := func(q string, args ...any) []string {
		t.Helper()
		rows, err := pool.Query(ctx, q, args...)
		must(err)
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var s string
			must(rows.Scan(&s))
			out = append(out, s)
		}
		must(rows.Err())
		return out
	}
	check := func(got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	checkPermissions := func(want []string) {
		t.Helper()
		check(values(`SELECT p.code FROM admin_role_permissions b JOIN admin_permissions p ON p.id=b.permission_id WHERE b.role_id=$1 ORDER BY p.code`, r1), want)
	}
	checkRoles := func(want []string) {
		t.Helper()
		check(values(`SELECT r.code FROM admin_user_role_bindings b JOIN admin_roles r ON r.id=b.role_id WHERE b.admin_user_id=$1 ORDER BY r.code`, u1), want)
	}
	must(repo.AssignPermissionsToRole(ctx, r2, []string{p1}))
	must(repo.AssignRolesToUser(ctx, u2, []string{r2}))

	t.Run("permissions replace clear and rollback", func(t *testing.T) {
		must(repo.AssignPermissionsToRole(ctx, r1, []string{p1, p2, p1}))
		checkPermissions([]string{"read", "write"})
		must(repo.AssignPermissionsToRole(ctx, r1, []string{p2}))
		checkPermissions([]string{"write"})
		if err := repo.AssignPermissionsToRole(ctx, r1, []string{p1, uuid.NewString()}); err == nil {
			t.Fatal("missing permission accepted")
		}
		checkPermissions([]string{"write"})
		must(repo.AssignPermissionsToRole(ctx, r1, []string{}))
		checkPermissions([]string{})
		must(repo.AssignPermissionsToRole(ctx, r1, nil))
		checkPermissions([]string{})
		if err := repo.AssignPermissionsToRole(ctx, uuid.NewString(), nil); err == nil {
			t.Fatal("missing role accepted")
		}
	})
	t.Run("roles replace clear and rollback", func(t *testing.T) {
		must(repo.AssignRolesToUser(ctx, u1, []string{r1, r2, r1}))
		checkRoles([]string{"r1", "r2"})
		must(repo.AssignRolesToUser(ctx, u1, []string{r2}))
		checkRoles([]string{"r2"})
		if err := repo.AssignRolesToUser(ctx, u1, []string{r1, uuid.NewString()}); err == nil {
			t.Fatal("missing role accepted")
		}
		checkRoles([]string{"r2"})
		must(repo.AssignRolesToUser(ctx, u1, []string{}))
		checkRoles([]string{})
		must(repo.AssignRolesToUser(ctx, u1, nil))
		checkRoles([]string{})
		if err := repo.AssignRolesToUser(ctx, uuid.NewString(), nil); err == nil {
			t.Fatal("missing user accepted")
		}
	})
	// 别人（r2/u2）的绑定全程没被这两轮整体替换碰到。
	check(values(`SELECT permission_id::text FROM admin_role_permissions WHERE role_id=$1`, r2), []string{p1})
	check(values(`SELECT role_id::text FROM admin_user_role_bindings WHERE admin_user_id=$1`, u2), []string{r2})
}

func mustExec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func column(t *testing.T, pool *pgxpool.Pool, q string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameColumn(t *testing.T, pool *pgxpool.Pool, q string, want []string, args ...any) {
	t.Helper()
	if got := column(t, pool, q, args...); !reflect.DeepEqual(got, want) {
		t.Fatalf("query %s: got %v want %v", q, got, want)
	}
}

// TestRoleCodeRename 覆盖角色改名的边界：换成一个已被占用的 code 要报冲突并回滚，
// 换成合法的新 code 则绑定全须全尾地留着。
//
// 这里曾经还要验证「改名要把平台域的策略标识从旧码迁到新码」——策略现在从 admin_roles
// 按 id 关联派生，绑定挂的是 role_id，改名根本碰不到它们，那类迁移代码已经删掉。
func TestRoleCodeRename(t *testing.T) {
	pool := bindingTestPool(t)
	ctx := context.Background()
	roles := NewAdminRoleRepository(pool, audit.NewRecorder())
	bindings := NewAdminBindingRepository(pool, audit.NewRecorder())
	roleA, roleB, perm, user, other := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()

	mustExec(t, pool, `INSERT INTO admin_permissions (id, code) VALUES ($1,'read')`, perm)
	mustExec(t, pool, `INSERT INTO admin_users (id) VALUES ($1),($2)`, user, other)
	mustExec(t, pool, `INSERT INTO admin_roles (id, code) VALUES ($1,'ops_b')`, roleB)
	if err := roles.Create(ctx, &model.AdminRole{ID: roleA, Code: "ops_old", Name: "只读", Description: "初始", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := bindings.AssignPermissionsToRole(ctx, roleA, []string{perm}); err != nil {
		t.Fatal(err)
	}
	if err := bindings.AssignRolesToUser(ctx, user, []string{roleA}); err != nil {
		t.Fatal(err)
	}

	rename := func(code string) error {
		t.Helper()
		role, err := roles.FindByID(ctx, roleA)
		if err != nil {
			t.Fatal(err)
		}
		role.Code = code
		return roles.Update(ctx, role)
	}
	checkRenamed := func(code string) {
		t.Helper()
		sameColumn(t, pool, `SELECT code FROM admin_roles WHERE id=$1`, []string{code}, roleA)
		// 角色 UUID 未变，基于 role_id 的绑定全部保留。
		sameColumn(t, pool, `SELECT permission_id::text FROM admin_role_permissions WHERE role_id=$1`, []string{perm}, roleA)
		sameColumn(t, pool, `SELECT role_id::text FROM admin_user_role_bindings WHERE admin_user_id=$1`, []string{roleA}, user)
	}

	t.Run("rename keeps the bindings", func(t *testing.T) {
		if err := rename("ops_new"); err != nil {
			t.Fatal(err)
		}
		checkRenamed("ops_new")
		// code 换了，但这条授权没变：判定仍要放行新码下的同一份权限。
		enforcer, err := casbin.New(pool)
		if err != nil {
			t.Fatal(err)
		}
		allowed, err := enforcer.Enforce(user, "", "read", "*")
		if err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Fatal("改名之后原有的授权不再放行")
		}
	})

	t.Run("existing role code conflicts and rolls back", func(t *testing.T) {
		err := rename("ops_b")
		if !errors.Is(err, model.ErrRoleCodeConflict) {
			t.Fatalf("rename to an existing code: got %v want ErrRoleCodeConflict", err)
		}
		checkRenamed("ops_new")
		sameColumn(t, pool, `SELECT code FROM admin_roles WHERE id=$1`, []string{"ops_b"}, roleB)
	})

	t.Run("reserved and malformed codes are refused", func(t *testing.T) {
		if err := roles.Create(ctx, &model.AdminRole{ID: uuid.NewString(), Code: "super_admin", Name: "伪装", CreatedAt: now, UpdatedAt: now}); !errors.Is(err, model.ErrReservedRoleCode) {
			t.Fatalf("create reserved code: got %v want ErrReservedRoleCode", err)
		}
		reservedID := uuid.NewString()
		mustExec(t, pool, `INSERT INTO admin_roles (id, code) VALUES ($1,'超级管理员')`, reservedID)
		if err := roles.Update(ctx, &model.AdminRole{ID: reservedID, Code: "ops_renamed", Name: "改名", UpdatedAt: now}); !errors.Is(err, model.ErrReservedRoleCode) {
			t.Fatalf("rename reserved code away: got %v want ErrReservedRoleCode", err)
		}
		sameColumn(t, pool, `SELECT code FROM admin_roles WHERE id=$1`, []string{"超级管理员"}, reservedID)
		for _, bad := range []string{"", " ops", "ops,read", "ops\"x", "ops\nread"} {
			if err := rename(bad); !errors.Is(err, model.ErrInvalidRoleCode) {
				t.Fatalf("rename to %q: got %v want ErrInvalidRoleCode", bad, err)
			}
		}
		checkRenamed("ops_new")
	})

	// 审计事件必须和业务写入同生共死。这是 outbox 模式存在的全部理由，
	// 所以两个方向都要断言：提交的那次留下事件，回滚的那次一条都不许留。
	t.Run("audit event is committed with the rename and rolled back without it", func(t *testing.T) {
		events := func() string {
			return column(t, pool, `SELECT count(*)::text FROM message_outbox WHERE event_type=$1`, audit.EventType)[0]
		}
		before := events()
		if err := rename("ops_audit"); err != nil {
			t.Fatal(err)
		}
		committed := events()
		if committed == before {
			t.Fatalf("committed rename wrote no audit event: %s -> %s", before, committed)
		}
		// 第二次必然失败（ops_b 已被占用），事务回滚，事件也必须一起消失。
		if err := rename("ops_b"); !errors.Is(err, model.ErrRoleCodeConflict) {
			t.Fatalf("rename to an existing code: got %v want ErrRoleCodeConflict", err)
		}
		if got := events(); got != committed {
			t.Fatalf("rolled-back rename left an audit event behind: %s -> %s", committed, got)
		}
	})

	t.Run("binding reads the code under lock during a concurrent rename", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SELECT code FROM admin_roles WHERE id=$1 FOR UPDATE`, roleA); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- bindings.AssignRolesToUser(ctx, other, []string{roleA}) }()
		time.Sleep(300 * time.Millisecond) // 让绑定先拿到用户锁并阻塞在角色锁上
		if _, err := tx.Exec(ctx, `UPDATE admin_roles SET code='ops_locked' WHERE id=$1`, roleA); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("binding did not finish after the rename committed")
		}
		// 绑定必须在锁内读到改名后的角色，不能拿着改名前的快照写库。
		sameColumn(t, pool, `SELECT r.code FROM admin_user_role_bindings b JOIN admin_roles r ON r.id=b.role_id WHERE b.admin_user_id=$1`, []string{"ops_locked"}, other)
	})

	// 残留规则不再参与判定，也就不该再挡住一个角色码：casbin_rule 里有一条 'ghost' 的
	// p 行，但 admin_roles 里没有这个码，改名过去必须成功。放在最后是因为这一步会把
	// roleA 的 code 改掉。
	t.Run("a stale policy row no longer occupies a role code", func(t *testing.T) {
		mustExec(t, pool, `INSERT INTO casbin_rule(ptype,v0,v1,v2,v3) VALUES ('p','ghost','','read','*')`)
		if err := rename("ghost"); err != nil {
			t.Fatalf("rename onto a stale policy row: %v", err)
		}
		checkRenamed("ghost")
	})
}

const (
	fixtureRoleCode = "ops"
	fixturePermCode = "brands:view"
)

// rbacFixture 建一套串起来的「用户-角色-权限」，供删除/改名路径断言策略是否跟着走。
type rbacFixture struct {
	pool     *pgxpool.Pool
	roles    AdminRoleRepository
	perms    AdminPermissionRepository
	bindings AdminBindingRepository
	roleID   string
	permID   string
	userID   string
	enforcer *casbin.Enforcer
}

func newRBACFixture(t *testing.T) *rbacFixture {
	t.Helper()
	pool := bindingTestPool(t)
	ctx := context.Background()
	now := time.Now()
	enforcer, err := casbin.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	f := &rbacFixture{
		pool:     pool,
		roles:    NewAdminRoleRepository(pool, audit.NewRecorder()),
		perms:    NewAdminPermissionRepository(pool, audit.NewRecorder()),
		bindings: NewAdminBindingRepository(pool, audit.NewRecorder()),
		roleID:   uuid.NewString(),
		permID:   uuid.NewString(),
		userID:   uuid.NewString(),
		enforcer: enforcer,
	}
	mustExec(t, pool, `INSERT INTO admin_users (id) VALUES ($1)`, f.userID)
	if err := f.perms.Create(ctx, &model.AdminPermission{ID: f.permID, Code: fixturePermCode, Name: "查看品牌", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.roles.Create(ctx, &model.AdminRole{ID: f.roleID, Code: fixtureRoleCode, Name: "运营", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := f.bindings.AssignPermissionsToRole(ctx, f.roleID, []string{f.permID}); err != nil {
		t.Fatal(err)
	}
	if err := f.bindings.AssignRolesToUser(ctx, f.userID, []string{f.roleID}); err != nil {
		t.Fatal(err)
	}
	// 夹具先自证立住了：下面几条断言的是「操作完之后不再放行」，
	// 若建的时候就没生效，那些断言会假通过。
	if !f.allows(t, fixturePermCode) {
		t.Fatal("夹具没立住：新绑定的权限在鉴权路径上不生效")
	}
	return f
}

// allows 重新加载策略后判断夹具里的用户是否还有该权限码。
//
// 仓储层只管写库，重载是 service + Broadcaster 的事，所以这里先 Reload——这正是
// 一次授权变更之后真正发生的事。断言走的是 enforcer 的实际判定，而不是「某张镜像表
// 被清干净了」：前者才是「撤销生效」的本体，后者只是它曾经的一种实现。
func (f *rbacFixture) allows(t *testing.T, object string) bool {
	t.Helper()
	if err := f.enforcer.Reload(); err != nil {
		t.Fatal(err)
	}
	ok, err := f.enforcer.Enforce(f.userID, "", object, "*")
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// 策略从 admin_roles / admin_role_permissions / admin_user_role_bindings 派生，删掉
// 角色行，它在两张绑定表里的行靠 ON DELETE CASCADE 一起消失，派生出来的规则也就不存在了。
// 这条用例钉住的是结果：被删角色的成员必须立刻通不过校验。
func TestRoleDeleteClearsPolicy(t *testing.T) {
	f := newRBACFixture(t)
	ctx := context.Background()

	if err := f.roles.Delete(ctx, f.roleID); err != nil {
		t.Fatal(err)
	}

	sameColumn(t, f.pool, `SELECT count(*)::text FROM admin_roles WHERE id=$1`, []string{"0"}, f.roleID)
	// 级联负责的两张表。
	sameColumn(t, f.pool, `SELECT count(*)::text FROM admin_role_permissions WHERE role_id=$1`, []string{"0"}, f.roleID)
	sameColumn(t, f.pool, `SELECT count(*)::text FROM admin_user_role_bindings WHERE role_id=$1`, []string{"0"}, f.roleID)
	if f.allows(t, fixturePermCode) {
		t.Fatal("角色已删，成员却仍然通过校验")
	}

	// 连带效果：同名 code 可以立刻重建。策略里不会有残留规则把它判成冲突。
	now := time.Now()
	if err := f.roles.Create(ctx, &model.AdminRole{
		ID: uuid.NewString(), Code: fixtureRoleCode, Name: "重建", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("recreating a deleted role's code must succeed, got %v", err)
	}
}

func TestPermissionDeleteClearsPolicy(t *testing.T) {
	f := newRBACFixture(t)
	ctx := context.Background()

	if err := f.perms.Delete(ctx, f.permID); err != nil {
		t.Fatal(err)
	}

	sameColumn(t, f.pool, `SELECT count(*)::text FROM admin_permissions WHERE id=$1`, []string{"0"}, f.permID)
	sameColumn(t, f.pool, `SELECT count(*)::text FROM admin_role_permissions WHERE permission_id=$1`, []string{"0"}, f.permID)
	// 权限码就是策略里的 obj。不跟着消失的话，每个曾拥有它的角色都还留着这条授权，
	// 而 RequirePermission 是按码匹配的——权限删了却还能用。
	if f.allows(t, fixturePermCode) {
		t.Fatal("权限已删，持有它的角色却仍然通过校验")
	}
}

// 权限改名之后判定要跟着新码走，且旧码立刻失效——两条一起断言，只看新码生效是不够的：
// 旧码还留着同样是有权限（而且更难发现）。
func TestPermissionRenameMigratesPolicy(t *testing.T) {
	f := newRBACFixture(t)
	ctx := context.Background()

	perm, err := f.perms.FindByID(ctx, f.permID)
	if err != nil {
		t.Fatal(err)
	}
	perm.Code = "brands:edit"
	if err := f.perms.Update(ctx, perm); err != nil {
		t.Fatal(err)
	}

	if !f.allows(t, "brands:edit") {
		t.Fatal("改名后的权限码没有生效")
	}
	if f.allows(t, fixturePermCode) {
		t.Fatal("旧权限码改名后仍然放行")
	}
	// 绑定表按 id 关联，改名不该动它。
	sameColumn(t, f.pool, `SELECT permission_id::text FROM admin_role_permissions WHERE role_id=$1`, []string{f.permID}, f.roleID)
}
