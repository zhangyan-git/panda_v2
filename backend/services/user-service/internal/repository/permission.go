package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// AdminPermissionRepository 平台权限数据访问接口
type AdminPermissionRepository interface {
	// FindPage 返回一页权限；总数走 Count。FindByRole 不分页——它返回的是
	// 「某角色已绑定的权限全集」，少一条就等于少一项授权，不是展示问题。
	FindPage(ctx context.Context, limit, offset int) ([]*model.AdminPermission, error)
	Count(ctx context.Context) (int64, error)
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

// FindPage 以 code 排序（唯一）+ id 兜底，翻页次序稳定。
func (r *pgAdminPermRepo) FindPage(ctx context.Context, limit, offset int) ([]*model.AdminPermission, error) {
	const q = `
		SELECT id, code, name, COALESCE(description, ''), perm_group, created_at
		FROM admin_permissions
		ORDER BY code, id
		LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, limit, offset)
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

func (r *pgAdminPermRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM admin_permissions`).Scan(&n)
	return n, err
}

func (r *pgAdminPermRepo) FindByID(ctx context.Context, id string) (*model.AdminPermission, error) {
	const q = `
		SELECT id, code, name, COALESCE(description, ''), perm_group, created_at
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
		SELECT p.id, p.code, p.name, COALESCE(p.description, ''), p.perm_group, p.created_at
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
	// 权限码就是策略里的 obj，改了名不需要迁移别的东西：策略按 role_id/permission_id
	// 关联到这张表，派生的规则自然跟着新码走。
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
	// 同角色删除：admin_role_permissions 有 ON DELETE CASCADE，派生出来的策略规则
	// 跟着绑定行一起消失，不需要另外清理。
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
	// 同 admin_roles：description 可空，审计快照里要的是空串而不是一个 Scan 错误。
	const q = `
		SELECT code, name, COALESCE(description, ''), perm_group
		FROM admin_permissions WHERE id = $1 FOR UPDATE`
	var snapshot permissionSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(&snapshot.Code, &snapshot.Name, &snapshot.Description, &snapshot.PermGroup)
	return snapshot, err
}

// AdminBindingRepository 角色-权限、用户-角色绑定
