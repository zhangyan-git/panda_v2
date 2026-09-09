package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
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

type pgAdminRoleRepo struct{ pool *pgxpool.Pool }

func NewAdminRoleRepository(pool *pgxpool.Pool) AdminRoleRepository {
	return &pgAdminRoleRepo{pool: pool}
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
	const q = `
		INSERT INTO admin_roles (id, code, name, description, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := r.pool.Exec(ctx, q,
		role.ID, role.Code, role.Name, role.Description, role.CreatedAt, role.UpdatedAt,
	)
	return err
}

func (r *pgAdminRoleRepo) Update(ctx context.Context, role *model.AdminRole) error {
	const q = `
		UPDATE admin_roles
		SET name = $1, description = $2, updated_at = $3
		WHERE id = $4`
	_, err := r.pool.Exec(ctx, q,
		role.Name, role.Description, role.UpdatedAt, role.ID,
	)
	return err
}

func (r *pgAdminRoleRepo) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM admin_roles WHERE id = $1`, id)
	return err
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

type pgAdminPermRepo struct{ pool *pgxpool.Pool }

func NewAdminPermissionRepository(pool *pgxpool.Pool) AdminPermissionRepository {
	return &pgAdminPermRepo{pool: pool}
}

func (r *pgAdminPermRepo) FindAll(ctx context.Context) ([]*model.AdminPermission, error) {
	const q = `
		SELECT id, code, name, description, perm_group, created_at
		FROM admin_permissions
		ORDER BY perm_group, code`
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
		ORDER BY p.perm_group, p.code`
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
	const q = `
		INSERT INTO admin_permissions (id, code, name, description, perm_group, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := r.pool.Exec(ctx, q,
		p.ID, p.Code, p.Name, p.Description, p.PermGroup, p.CreatedAt,
	)
	return err
}

func (r *pgAdminPermRepo) Update(ctx context.Context, p *model.AdminPermission) error {
	const q = `
		UPDATE admin_permissions
		SET code = $1, name = $2, description = $3, perm_group = $4
		WHERE id = $5`
	_, err := r.pool.Exec(ctx, q,
		p.Code, p.Name, p.Description, p.PermGroup, p.ID,
	)
	return err
}

func (r *pgAdminPermRepo) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM admin_permissions WHERE id = $1`, id)
	return err
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

type pgAdminBindingRepo struct{ pool *pgxpool.Pool }

func NewAdminBindingRepository(pool *pgxpool.Pool) AdminBindingRepository {
	return &pgAdminBindingRepo{pool: pool}
}

func (r *pgAdminBindingRepo) AssignPermissionsToRole(ctx context.Context, roleID string, permissionIDs []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, pid := range permissionIDs {
		// 获取 permission code 用于写 casbin_rule
		var code string
		if err := tx.QueryRow(ctx, `SELECT code FROM admin_permissions WHERE id = $1`, pid).Scan(&code); err != nil {
			return fmt.Errorf("permission %s not found: %w", pid, err)
		}
		// 获取 role name 用于写 casbin_rule
		var roleCode string
		if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1`, roleID).Scan(&roleCode); err != nil {
			return fmt.Errorf("role %s not found: %w", roleID, err)
		}

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
	return tx.Commit(ctx)
}

func (r *pgAdminBindingRepo) RemovePermissionFromRole(ctx context.Context, roleID, permissionID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var roleCode, code string
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1`, roleID).Scan(&roleCode); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_permissions WHERE id = $1`, permissionID).Scan(&code); err != nil {
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
	return tx.Commit(ctx)
}

func (r *pgAdminBindingRepo) AssignRolesToUser(ctx context.Context, userID string, roleIDs []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, rid := range roleIDs {
		var roleCode string
		if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1`, rid).Scan(&roleCode); err != nil {
			return fmt.Errorf("role %s not found: %w", rid, err)
		}
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
	return tx.Commit(ctx)
}

func (r *pgAdminBindingRepo) RemoveRoleFromUser(ctx context.Context, userID, roleID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var roleCode string
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1`, roleID).Scan(&roleCode); err != nil {
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
