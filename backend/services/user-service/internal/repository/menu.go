package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

type pgMenuRepo struct {
	pool *pgxpool.Pool
}

func NewMenuRepository(pool *pgxpool.Pool) MenuRepository {
	return &pgMenuRepo{pool: pool}
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
	const q = `
		INSERT INTO admin_menus (id, parent_id, name, path, icon, sort, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := r.pool.Exec(ctx, q,
		m.ID, nullParent(m.ParentID), m.Name, m.Path, m.Icon, m.Sort,
		m.CreatedAt, m.UpdatedAt,
	)
	return err
}

func (r *pgMenuRepo) Update(ctx context.Context, m *model.Menu) error {
	const q = `
		UPDATE admin_menus
		SET parent_id = $2, name = $3, path = $4, icon = $5, sort = $6, updated_at = $7
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q,
		m.ID, nullParent(m.ParentID), m.Name, m.Path, m.Icon, m.Sort, m.UpdatedAt,
	)
	return err
}

func (r *pgMenuRepo) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM admin_menus WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id)
	return err
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
	return tx.Commit(ctx)
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
