package repository

import (
	"context"

	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// MenuRepository 菜单树与角色-菜单绑定的数据访问接口
type MenuRepository interface {
	FindAll(ctx context.Context) ([]*model.Menu, error)
	FindByID(ctx context.Context, id string) (*model.Menu, error)
	Create(ctx context.Context, m *model.Menu) error
	Update(ctx context.Context, m *model.Menu) error
	Delete(ctx context.Context, id string) error
	HasChildren(ctx context.Context, id string) (bool, error)
	FindMenuIDsByRoleID(ctx context.Context, roleID string) ([]string, error)
	AssignToRole(ctx context.Context, roleID string, menuIDs []string) error
}

// menuSnapshot 是写入审计 before_data / after_data 的形状。菜单决定前端可达的
// 页面，改菜单本身就是一次授权面变更，所以要留快照。
type menuSnapshot struct {
	ParentID string `json:"parent_id,omitempty"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Icon     string `json:"icon"`
	Sort     int    `json:"sort"`
}

func snapshotOfMenu(m *model.Menu) menuSnapshot {
	return menuSnapshot{ParentID: m.ParentID, Name: m.Name, Path: m.Path, Icon: m.Icon, Sort: m.Sort}
}

type pgMenuRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewMenuRepository(pool *pgxpool.Pool, recorder audit.Recorder) MenuRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgMenuRepo{pool: pool, audit: recorder}
}

// menuColumns 统一 SELECT 列表，parent_id 归一化为空字符串方便扫描
const menuColumns = `id, COALESCE(parent_id::text, ''), name, path, icon, sort, created_at, updated_at`

func (r *pgMenuRepo) FindAll(ctx context.Context) ([]*model.Menu, error) {
	q := `SELECT ` + menuColumns + ` FROM admin_menus ORDER BY sort, created_at`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.Menu
	for rows.Next() {
		m, err := scanMenu(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

func (r *pgMenuRepo) FindByID(ctx context.Context, id string) (*model.Menu, error) {
	q := `SELECT ` + menuColumns + ` FROM admin_menus WHERE id = $1 LIMIT 1`
	return scanMenu(r.pool.QueryRow(ctx, q, id))
}

func (r *pgMenuRepo) Create(ctx context.Context, m *model.Menu) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO admin_menus (id, parent_id, name, path, icon, sort, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := tx.Exec(ctx, q,
		m.ID, nullParent(m.ParentID), m.Name, m.Path, m.Icon, m.Sort,
		m.CreatedAt, m.UpdatedAt,
	); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "menus", Action: "create", Operation: "新增菜单",
		TargetType: "menu", TargetID: m.ID, TargetName: m.Name,
		After: audit.Snapshot(snapshotOfMenu(m)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgMenuRepo) Update(ctx context.Context, m *model.Menu) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMenuForUpdate(ctx, tx, m.ID)
	if err != nil {
		return err
	}
	const q = `
		UPDATE admin_menus
		SET parent_id = $2, name = $3, path = $4, icon = $5, sort = $6, updated_at = $7
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q,
		m.ID, nullParent(m.ParentID), m.Name, m.Path, m.Icon, m.Sort, m.UpdatedAt,
	); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "menus", Action: "update", Operation: "修改菜单",
		TargetType: "menu", TargetID: m.ID, TargetName: m.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(snapshotOfMenu(m)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgMenuRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMenuForUpdate(ctx, tx, id)
	if err != nil {
		// 删不存在的菜单保持原语义：不是错误，也就没有可记的审计。
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	const q = `DELETE FROM admin_menus WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "menus", Action: "delete", Operation: "删除菜单",
		TargetType: "menu", TargetID: id, TargetName: before.Name,
		Before: audit.Snapshot(before),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func selectMenuForUpdate(ctx context.Context, tx pgx.Tx, id string) (menuSnapshot, error) {
	const q = `
		SELECT COALESCE(parent_id::text, ''), name, path, icon, sort
		FROM admin_menus WHERE id = $1 FOR UPDATE`
	var snapshot menuSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(
		&snapshot.ParentID, &snapshot.Name, &snapshot.Path, &snapshot.Icon, &snapshot.Sort,
	)
	return snapshot, err
}

func (r *pgMenuRepo) HasChildren(ctx context.Context, id string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM admin_menus WHERE parent_id = $1)`
	var exists bool
	err := r.pool.QueryRow(ctx, q, id).Scan(&exists)
	return exists, err
}

func (r *pgMenuRepo) FindMenuIDsByRoleID(ctx context.Context, roleID string) ([]string, error) {
	const q = `SELECT menu_id FROM admin_role_menus WHERE role_id = $1`
	rows, err := r.pool.Query(ctx, q, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *pgMenuRepo) AssignToRole(ctx context.Context, roleID string, menuIDs []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var roleCode string
	if err := tx.QueryRow(ctx, `SELECT code FROM admin_roles WHERE id = $1 FOR UPDATE`, roleID).Scan(&roleCode); err != nil {
		return err
	}
	// 整体替换语义：before 必须在清空之前取。
	beforeIDs, err := menuIDsOfRole(ctx, tx, roleID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_role_menus WHERE role_id = $1`, roleID); err != nil {
		return err
	}
	for _, menuID := range menuIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_role_menus (role_id, menu_id) VALUES ($1, $2)
			 ON CONFLICT (role_id, menu_id) DO NOTHING`,
			roleID, menuID,
		); err != nil {
			return err
		}
	}
	afterIDs, err := menuIDsOfRole(ctx, tx, roleID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "roles", Action: "assign_menus", Operation: "配置角色菜单",
		TargetType: "role", TargetID: roleID, TargetName: roleCode,
		Before: audit.Snapshot(bindingSnapshot{TargetID: roleID, IDs: beforeIDs}),
		After:  audit.Snapshot(bindingSnapshot{TargetID: roleID, IDs: afterIDs}),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func menuIDsOfRole(ctx context.Context, tx pgx.Tx, roleID string) ([]string, error) {
	return idSetOf(ctx, tx,
		`SELECT menu_id::text FROM admin_role_menus WHERE role_id = $1 ORDER BY menu_id`, roleID)
}

func scanMenu(row pgx.Row) (*model.Menu, error) {
	m := &model.Menu{}
	err := row.Scan(&m.ID, &m.ParentID, &m.Name, &m.Path, &m.Icon, &m.Sort, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// nullParent 空字符串父 ID 在库中存 NULL
func nullParent(parentID string) any {
	if parentID == "" {
		return nil
	}
	return parentID
}
