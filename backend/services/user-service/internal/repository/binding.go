package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

type AdminBindingRepository interface {
	AssignPermissionsToRole(ctx context.Context, roleID string, permissionIDs []string) error
	RemovePermissionFromRole(ctx context.Context, roleID, permissionID string) error
	AssignRolesToUser(ctx context.Context, userID string, roleIDs []string) error
	RemoveRoleFromUser(ctx context.Context, userID, roleID string) error
	FindRolesByUser(ctx context.Context, userID string) ([]*model.AdminRole, error)
	FindPermissionCodesByUser(ctx context.Context, userID string) ([]string, error)
}

// bindingSnapshot 记录绑定关系的整体替换结果。这四个接口都是「整体替换」语义
// （先清空再写入），只记被增删的那一个 id 会丢掉「其余保持不变」这个事实，
// 事后无法判断一次保存到底是改了一项还是清掉了一片。
type bindingSnapshot struct {
	TargetID string   `json:"target_id"`
	IDs      []string `json:"ids"`
}

type pgAdminBindingRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewAdminBindingRepository(pool *pgxpool.Pool, recorder audit.Recorder) AdminBindingRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgAdminBindingRepo{pool: pool, audit: recorder}
}

// idSetOf 读出某个绑定集合的当前成员，用来记 before 快照。必须在删除语句之前调用。
func idSetOf(ctx context.Context, tx pgx.Tx, query, key string) ([]string, error) {
	rows, err := tx.Query(ctx, query, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, 8)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ensurePermissionsExist 校验这批权限 id 都在 admin_permissions 里，缺一个就报错。
// 绑定行本身有外键，缺 id 要到 INSERT 时才以 23503 报出来——那种错误看不出是哪个 id，
// 也分不清是调用方传错还是并发删除。这里一次查完，错误里能指名道姓。
func ensurePermissionsExist(ctx context.Context, tx pgx.Tx, permissionIDs []string) error {
	if len(permissionIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM admin_permissions WHERE id = ANY($1::uuid[])`, permissionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := make(map[string]struct{}, len(permissionIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, pid := range permissionIDs {
		if _, ok := found[pid]; !ok {
			return fmt.Errorf("permission %s not found: %w", pid, pgx.ErrNoRows)
		}
	}
	return nil
}

func permissionIDsOfRole(ctx context.Context, tx pgx.Tx, roleID string) ([]string, error) {
	return idSetOf(ctx, tx,
		`SELECT permission_id::text FROM admin_role_permissions WHERE role_id = $1 ORDER BY permission_id`, roleID)
}

func roleIDsOfUser(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	return idSetOf(ctx, tx,
		`SELECT role_id::text FROM admin_user_role_bindings WHERE admin_user_id = $1 ORDER BY role_id`, userID)
}

func (r *pgAdminBindingRepo) AssignPermissionsToRole(ctx context.Context, roleID string, permissionIDs []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 分配接口整体替换；锁住目标，避免并发保存将两个集合合并。
	var roleCode string
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1 FOR UPDATE`, roleID).Scan(&roleCode); err != nil {
		return fmt.Errorf("role %s not found: %w", roleID, err)
	}
	beforeIDs, err := permissionIDsOfRole(ctx, tx, roleID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_role_permissions WHERE role_id = $1`, roleID); err != nil {
		return err
	}
	if err := ensurePermissionsExist(ctx, tx, permissionIDs); err != nil {
		return err
	}

	afterIDs := make([]string, 0, len(permissionIDs))
	for _, pid := range permissionIDs {
		afterIDs = append(afterIDs, pid)
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_role_permissions (role_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			roleID, pid,
		); err != nil {
			return err
		}
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "assign_permissions", Operation: "配置角色权限",
		TargetType: "role", TargetID: roleID, TargetName: roleCode,
		Before: audit.Snapshot(bindingSnapshot{TargetID: roleID, IDs: beforeIDs}),
		After:  audit.Snapshot(bindingSnapshot{TargetID: roleID, IDs: afterIDs}),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminBindingRepo) RemovePermissionFromRole(ctx context.Context, roleID, permissionID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var roleCode string
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1 FOR UPDATE`, roleID).Scan(&roleCode); err != nil {
		return err
	}
	beforeIDs, err := permissionIDsOfRole(ctx, tx, roleID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM admin_role_permissions WHERE role_id = $1 AND permission_id = $2`,
		roleID, permissionID,
	); err != nil {
		return err
	}
	// after 直接从库里重读，而不是在内存里把 permissionID 从 beforeIDs 减掉：
	// 调用方传进来的 id 大小写/格式未必与库里一致，减法会漏掉或不匹配。
	afterIDs, err := permissionIDsOfRole(ctx, tx, roleID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "remove_permission", Operation: "移除角色权限",
		TargetType: "role", TargetID: roleID, TargetName: roleCode,
		Before: audit.Snapshot(bindingSnapshot{TargetID: roleID, IDs: beforeIDs}),
		After:  audit.Snapshot(bindingSnapshot{TargetID: roleID, IDs: afterIDs}),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminBindingRepo) AssignRolesToUser(ctx context.Context, userID string, roleIDs []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var username string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(username, '') FROM admin_users WHERE id = $1 FOR UPDATE`, userID).Scan(&username); err != nil {
		return fmt.Errorf("user %s not found: %w", userID, err)
	}
	// 用户锁先于角色锁；先锁全部旧/新角色，随后才删除绑定或规则。
	ids := make([]string, len(roleIDs))
	for i, id := range roleIDs {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return fmt.Errorf("invalid role ID: %w", err)
		}
		ids[i] = parsed.String()
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM admin_roles
		WHERE id = ANY($1::uuid[]) OR id IN (
			SELECT role_id FROM admin_user_role_bindings WHERE admin_user_id = $2
		) ORDER BY id FOR SHARE`, ids, userID)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		known[id] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("role %s not found: %w", id, pgx.ErrNoRows)
		}
	}
	beforeIDs, err := roleIDsOfUser(ctx, tx, userID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_user_role_bindings WHERE admin_user_id = $1`, userID); err != nil {
		return err
	}

	for _, rid := range ids {
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_user_role_bindings (admin_user_id, role_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			userID, rid,
		); err != nil {
			return err
		}
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "users", Action: "assign_roles", Operation: "配置用户角色",
		TargetType: "admin_user", TargetID: userID, TargetName: username,
		Before: audit.Snapshot(bindingSnapshot{TargetID: userID, IDs: beforeIDs}),
		After:  audit.Snapshot(bindingSnapshot{TargetID: userID, IDs: ids}),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminBindingRepo) RemoveRoleFromUser(ctx context.Context, userID, roleID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var username string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(username, '') FROM admin_users WHERE id = $1 FOR UPDATE`, userID).Scan(&username); err != nil {
		return err
	}
	// 这个 id 取出来没有别的用途，保留的是「角色不存在就报 ErrNoRows」与 FOR SHARE
	// 的加锁次序（用户锁先于角色锁，与 AssignRolesToUser 一致）。
	var lockedRoleID string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM admin_roles WHERE id = $1 FOR SHARE`, roleID).Scan(&lockedRoleID); err != nil {
		return err
	}
	beforeIDs, err := roleIDsOfUser(ctx, tx, userID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM admin_user_role_bindings WHERE admin_user_id = $1 AND role_id = $2`,
		userID, roleID,
	); err != nil {
		return err
	}
	afterIDs, err := roleIDsOfUser(ctx, tx, userID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "users", Action: "remove_role", Operation: "移除用户角色",
		TargetType: "admin_user", TargetID: userID, TargetName: username,
		Before: audit.Snapshot(bindingSnapshot{TargetID: userID, IDs: beforeIDs}),
		After:  audit.Snapshot(bindingSnapshot{TargetID: userID, IDs: afterIDs}),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminBindingRepo) FindRolesByUser(ctx context.Context, userID string) ([]*model.AdminRole, error) {
	// description 可空：这里读出的角色直接进鉴权路径（超管判定看 code），
	// 一个 NULL 不该让整条鉴权链断在 Scan 上。
	const q = `
		SELECT r.id, r.code, r.name, COALESCE(r.description, ''), r.created_at, r.updated_at
		FROM admin_roles r
		JOIN admin_user_role_bindings b ON b.role_id = r.id
		WHERE b.admin_user_id = $1
		ORDER BY r.name`
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.AdminRole
	for rows.Next() {
		role := &model.AdminRole{}
		if err := rows.Scan(&role.ID, &role.Code, &role.Name, &role.Description, &role.CreatedAt, &role.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, role)
	}
	return list, rows.Err()
}

func (r *pgAdminBindingRepo) FindPermissionCodesByUser(ctx context.Context, userID string) ([]string, error) {
	const q = `
		SELECT DISTINCT p.code
		FROM admin_permissions p
		JOIN admin_role_permissions rp ON rp.permission_id = p.id
		JOIN admin_user_role_bindings b ON b.role_id = rp.role_id
		WHERE b.admin_user_id = $1
		ORDER BY p.code`
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, rows.Err()
}
