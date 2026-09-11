package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// AdminRoleRepository 平台角色数据访问接口
type AdminRoleRepository interface {
	FindAll(ctx context.Context) ([]*model.AdminRole, error)
	FindByID(ctx context.Context, id string) (*model.AdminRole, error)
	Create(ctx context.Context, r *model.AdminRole) error
	Update(ctx context.Context, r *model.AdminRole) error
	Delete(ctx context.Context, id string) error
}

// roleSnapshot 是写入审计 before_data / after_data 的形状。
//
// 显式列字段而不是序列化 model.AdminRole，有两条理由：模型的字段只有 db tag，
// 直接序列化写出的是 Go 字段名；而且同一个包里的 AdminUser 带着 password_hash——
// 审计快照一旦习惯性照抄模型，迟早会把口令散列写进日志表。
type roleSnapshot struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func snapshotOfRole(role *model.AdminRole) roleSnapshot {
	return roleSnapshot{Code: role.Code, Name: role.Name, Description: role.Description}
}

type pgAdminRoleRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewAdminRoleRepository(pool *pgxpool.Pool, recorder audit.Recorder) AdminRoleRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgAdminRoleRepo{pool: pool, audit: recorder}
}

func (r *pgAdminRoleRepo) FindAll(ctx context.Context) ([]*model.AdminRole, error) {
	const q = `
		SELECT id, code, name, description, created_at, updated_at
		FROM admin_roles
		ORDER BY created_at`
	rows, err := r.pool.Query(ctx, q)
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

func (r *pgAdminRoleRepo) FindByID(ctx context.Context, id string) (*model.AdminRole, error) {
	const q = `
		SELECT id, code, name, description, created_at, updated_at
		FROM admin_roles WHERE id = $1`
	role := &model.AdminRole{}
	err := r.pool.QueryRow(ctx, q, id).Scan(
		&role.ID, &role.Code, &role.Name, &role.Description, &role.CreatedAt, &role.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return role, nil
}

func (r *pgAdminRoleRepo) Create(ctx context.Context, role *model.AdminRole) error {
	if err := model.ValidateRoleCodeChange("", role.Code); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO admin_roles (id, code, name, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(ctx, q,
		role.ID, role.Code, role.Name, role.Description, role.CreatedAt, role.UpdatedAt,
	); err != nil {
		return roleCodeError(err)
	}
	// 先占用唯一 code，再用新快照检查残留规则，不能合并旧授权。
	if err := checkRolePolicyConflict(ctx, tx, role.Code); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "create", Operation: "新增角色",
		TargetType: "role", TargetID: role.ID, TargetName: role.Name,
		After: audit.Snapshot(snapshotOfRole(role)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminRoleRepo) Update(ctx context.Context, role *model.AdminRole) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// 读整行而不只是 code：锁已经拿到了，审计的 before 快照顺带取出来，
	// 不必在审计时再查一次（那时数据已被本次更新覆盖，也就取不到了）。
	before := &model.AdminRole{}
	if err := tx.QueryRow(ctx,
		`SELECT id, code, name, description, created_at, updated_at FROM admin_roles WHERE id = $1 FOR UPDATE`,
		role.ID,
	).Scan(&before.ID, &before.Code, &before.Name, &before.Description, &before.CreatedAt, &before.UpdatedAt); err != nil {
		return err
	}
	oldCode := before.Code
	if err := model.ValidateRoleCodeChange(oldCode, role.Code); err != nil {
		return err
	}
	const q = `
		UPDATE admin_roles
		SET code = $1, name = $2, description = $3, updated_at = $4
		WHERE id = $5`
	if _, err := tx.Exec(ctx, q, role.Code, role.Name, role.Description, role.UpdatedAt, role.ID); err != nil {
		return roleCodeError(err)
	}
	if oldCode != role.Code {
		if err := checkRolePolicyConflict(ctx, tx, role.Code); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE casbin_rule SET v0 = $1 WHERE ptype = 'p' AND v0 = $2 AND v1 = ''`, role.Code, oldCode); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE casbin_rule SET v1 = $1 WHERE ptype = 'g' AND v1 = $2 AND v2 = ''`, role.Code, oldCode); err != nil {
			return err
		}
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "update", Operation: "修改角色",
		TargetType: "role", TargetID: role.ID, TargetName: role.Name,
		Before: audit.Snapshot(snapshotOfRole(before)), After: audit.Snapshot(snapshotOfRole(role)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func roleCodeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "admin_roles_code_key" {
		return fmt.Errorf("%w: %w", model.ErrRoleCodeConflict, err)
	}
	return err
}

func checkRolePolicyConflict(ctx context.Context, tx pgx.Tx, code string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM casbin_rule WHERE (ptype = 'p' AND v0 = $1 AND v1 = '')
		OR (ptype = 'g' AND v1 = $1 AND v2 = '')
	)`, code).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return model.ErrRoleCodeConflict
	}
	return nil
}

func (r *pgAdminRoleRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 删除之后行就没了，before 快照只能在删之前取。取不到说明这个角色本来就不存在：
	// 保持原来的语义（删不存在的角色不算错误），只是这次没有可记的操作。
	before := &model.AdminRole{}
	err = tx.QueryRow(ctx,
		`SELECT id, code, name, description, created_at, updated_at FROM admin_roles WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&before.ID, &before.Code, &before.Name, &before.Description, &before.CreatedAt, &before.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_roles WHERE id = $1`, id); err != nil {
		return err
	}
	// 绑定表靠 ON DELETE CASCADE 跟着走，casbin_rule 不会——它没有外键，
	// 而 enforcer 恰恰只从这张表加载策略。不清就是「角色已删，权限还在」：
	// 持有该角色的人继续通过校验，而且残留的 p 规则会让同名 code 再也建不回来
	// （Create 的 checkRolePolicyConflict 会判成冲突）。
	// 谓词与 Update 里改名用的完全一致，只动平台域（v1='' / v2=''）。
	if _, err := tx.Exec(ctx, `DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = $1 AND v1 = ''`, before.Code); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM casbin_rule WHERE ptype = 'g' AND v1 = $1 AND v2 = ''`, before.Code); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "delete", Operation: "删除角色",
		TargetType: "role", TargetID: id, TargetName: before.Name,
		Before: audit.Snapshot(snapshotOfRole(before)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AdminPermissionRepository 平台权限数据访问接口
type AdminPermissionRepository interface {
	FindAll(ctx context.Context) ([]*model.AdminPermission, error)
	FindByID(ctx context.Context, id string) (*model.AdminPermission, error)
	FindByRole(ctx context.Context, roleID string) ([]*model.AdminPermission, error)
	Create(ctx context.Context, p *model.AdminPermission) error
	Update(ctx context.Context, p *model.AdminPermission) error
	Delete(ctx context.Context, id string) error
}

// permissionSnapshot 是写入审计 before_data / after_data 的形状，理由同
// roleSnapshot：模型只有 db tag，照抄模型迟早把不该记的字段带进去。
type permissionSnapshot struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PermGroup   string `json:"perm_group"`
}

func snapshotOfPermission(p *model.AdminPermission) permissionSnapshot {
	return permissionSnapshot{Code: p.Code, Name: p.Name, Description: p.Description, PermGroup: p.PermGroup}
}

type pgAdminPermRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewAdminPermissionRepository(pool *pgxpool.Pool, recorder audit.Recorder) AdminPermissionRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgAdminPermRepo{pool: pool, audit: recorder}
}

func (r *pgAdminPermRepo) FindAll(ctx context.Context) ([]*model.AdminPermission, error) {
	const q = `
		SELECT id, code, name, description, perm_group, created_at
		FROM admin_permissions
		ORDER BY code`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.AdminPermission
	for rows.Next() {
		p := &model.AdminPermission{}
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.Description, &p.PermGroup, &p.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (r *pgAdminPermRepo) FindByID(ctx context.Context, id string) (*model.AdminPermission, error) {
	const q = `
		SELECT id, code, name, description, perm_group, created_at
		FROM admin_permissions WHERE id = $1`
	p := &model.AdminPermission{}
	err := r.pool.QueryRow(ctx, q, id).Scan(
		&p.ID, &p.Code, &p.Name, &p.Description, &p.PermGroup, &p.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *pgAdminPermRepo) FindByRole(ctx context.Context, roleID string) ([]*model.AdminPermission, error) {
	const q = `
		SELECT p.id, p.code, p.name, p.description, p.perm_group, p.created_at
		FROM admin_permissions p
		JOIN admin_role_permissions rp ON rp.permission_id = p.id
		WHERE rp.role_id = $1
		ORDER BY p.code`
	rows, err := r.pool.Query(ctx, q, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.AdminPermission
	for rows.Next() {
		p := &model.AdminPermission{}
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.Description, &p.PermGroup, &p.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (r *pgAdminPermRepo) Create(ctx context.Context, p *model.AdminPermission) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO admin_permissions (id, code, name, description, perm_group, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(ctx, q,
		p.ID, p.Code, p.Name, p.Description, p.PermGroup, p.CreatedAt,
	); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "permissions", Action: "create", Operation: "新增权限",
		TargetType: "permission", TargetID: p.ID, TargetName: p.Name,
		After: audit.Snapshot(snapshotOfPermission(p)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminPermRepo) Update(ctx context.Context, p *model.AdminPermission) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 先读出旧值再改：审计的意义就在于留下「改成了什么」，只有 after 等于没记。
	before, err := selectPermissionForUpdate(ctx, tx, p.ID)
	if err != nil {
		return err
	}
	const q = `
		UPDATE admin_permissions
		SET code = $1, name = $2, description = $3, perm_group = $4
		WHERE id = $5`
	if _, err := tx.Exec(ctx, q,
		p.Code, p.Name, p.Description, p.PermGroup, p.ID,
	); err != nil {
		return err
	}
	// 权限码就是策略里的 obj（p 规则的 v2），改了名必须同期迁移，
	// 否则旧码的授权留在 casbin_rule 里、新码一条也没有——改名看起来生效了，
	// 实际是「旧权限还在、新权限不生效」。角色改名同理，见 Update 里的两行。
	if before.Code != p.Code {
		if _, err := tx.Exec(ctx, `UPDATE casbin_rule SET v2 = $1 WHERE ptype = 'p' AND v1 = '' AND v2 = $2`, p.Code, before.Code); err != nil {
			return err
		}
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "permissions", Action: "update", Operation: "修改权限",
		TargetType: "permission", TargetID: p.ID, TargetName: p.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(snapshotOfPermission(p)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminPermRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectPermissionForUpdate(ctx, tx, id)
	if err != nil {
		// 删不存在的权限保持原语义：不是错误，也就没有可记的审计。
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_permissions WHERE id = $1`, id); err != nil {
		return err
	}
	// 同角色删除：admin_role_permissions 有级联，casbin_rule 没有。
	// 不清的话，删掉的权限码仍然挂在每个曾经拥有它的角色下面，
	// 而 RequirePermission 是按码匹配的——等于权限删了还能用。
	if _, err := tx.Exec(ctx, `DELETE FROM casbin_rule WHERE ptype = 'p' AND v1 = '' AND v2 = $1`, before.Code); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "permissions", Action: "delete", Operation: "删除权限",
		TargetType: "permission", TargetID: id, TargetName: before.Name,
		Before: audit.Snapshot(before),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func selectPermissionForUpdate(ctx context.Context, tx pgx.Tx, id string) (permissionSnapshot, error) {
	const q = `
		SELECT code, name, description, perm_group
		FROM admin_permissions WHERE id = $1 FOR UPDATE`
	var snapshot permissionSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(&snapshot.Code, &snapshot.Name, &snapshot.Description, &snapshot.PermGroup)
	return snapshot, err
}

// AdminBindingRepository 角色-权限、用户-角色绑定
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
	if _, err := tx.Exec(ctx, `DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = $1 AND v1 = ''`, roleCode); err != nil {
		return err
	}

	afterIDs := make([]string, 0, len(permissionIDs))
	for _, pid := range permissionIDs {
		// 获取 permission code 用于写 casbin_rule
		var code string
		if err := tx.QueryRow(ctx, `SELECT code FROM admin_permissions WHERE id = $1`, pid).Scan(&code); err != nil {
			return fmt.Errorf("permission %s not found: %w", pid, err)
		}
		afterIDs = append(afterIDs, pid)
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_role_permissions (role_id, permission_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			roleID, pid,
		); err != nil {
			return err
		}
		// 同步写 casbin_rule: p, roleCode, "", code, "*"
		// obj=code, act="*" 表示该角色拥有此权限码，具体 obj/act 检查由 handler.RequirePermission 用 code 匹配
		if _, err := tx.Exec(ctx,
			`INSERT INTO casbin_rule (ptype, v0, v1, v2, v3) VALUES ('p', $1, '', $2, '*')
			 ON CONFLICT DO NOTHING`,
			roleCode, code,
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

	var roleCode, code string
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1 FOR UPDATE`, roleID).Scan(&roleCode); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_permissions WHERE id = $1`, permissionID).Scan(&code); err != nil {
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
	if _, err := tx.Exec(ctx,
		`DELETE FROM casbin_rule WHERE ptype='p' AND v0=$1 AND v1='' AND v2=$2`,
		roleCode, code,
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
	rows, err := tx.Query(ctx, `SELECT id, code FROM admin_roles
		WHERE id = ANY($1::uuid[]) OR id IN (
			SELECT role_id FROM admin_user_role_bindings WHERE admin_user_id = $2
		) ORDER BY id FOR SHARE`, ids, userID)
	if err != nil {
		return err
	}
	codes := make(map[string]string)
	for rows.Next() {
		var id, code string
		if err := rows.Scan(&id, &code); err != nil {
			rows.Close()
			return err
		}
		codes[id] = code
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := codes[id]; !ok {
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
	if _, err := tx.Exec(ctx, `DELETE FROM casbin_rule WHERE ptype = 'g' AND v0 = $1 AND v2 = ''`, userID); err != nil {
		return err
	}

	for _, rid := range ids {
		roleCode := codes[rid]
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_user_role_bindings (admin_user_id, role_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			userID, rid,
		); err != nil {
			return err
		}
		// 同步写 casbin_rule: g, userID, roleCode, "" (domain)
		if _, err := tx.Exec(ctx,
			`INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ('g', $1, $2, '')
			 ON CONFLICT DO NOTHING`,
			userID, roleCode,
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
	var roleCode string
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1 FOR SHARE`, roleID).Scan(&roleCode); err != nil {
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
	if _, err := tx.Exec(ctx,
		`DELETE FROM casbin_rule WHERE ptype='g' AND v0=$1 AND v1=$2 AND v2=''`,
		userID, roleCode,
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
	const q = `
		SELECT r.id, r.code, r.name, r.description, r.created_at, r.updated_at
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
