package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// AdminUserRepository 平台管理员数据访问接口
type AdminUserRepository interface {
	// FindPage 返回一页管理员；总数走 Count，两个方法都不带筛选条件，
	// 所以它们必须始终对同一批行生效——将来给列表加筛选时，两边要一起改。
	FindPage(ctx context.Context, limit, offset int) ([]*model.AdminUser, error)
	Count(ctx context.Context) (int64, error)
	FindByUsername(ctx context.Context, username string) (*model.AdminUser, error)
	FindByID(ctx context.Context, id string) (*model.AdminUser, error)
	Create(ctx context.Context, u *model.AdminUser) error
	UpdateStatus(ctx context.Context, id, status string) error
}

// adminUserSnapshot 刻意不含 PasswordHash。model.AdminUser 是跟着 admin_users
// 整行走的，直接序列化它会把口令散列写进审计表——一张长期留存、还要被后台翻看的表。
// 审计只记「谁、改了什么状态」，口令散列不是审计信息，是凭据。
type adminUserSnapshot struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Status   string `json:"status"`
}

func snapshotOfAdminUser(u *model.AdminUser) adminUserSnapshot {
	return adminUserSnapshot{Username: u.Username, Name: u.Name, Email: u.Email, Status: u.Status}
}

type pgAdminUserRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewAdminUserRepository(pool *pgxpool.Pool, recorder audit.Recorder) AdminUserRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgAdminUserRepo{pool: pool, audit: recorder}
}

// FindPage 排序里带上 id 作为决胜位：created_at 相同的行（同一次批量导入很常见）
// 单靠时间排序没有稳定次序，翻页时同一行可能出现两次、另一行一次都不出现。
func (r *pgAdminUserRepo) FindPage(ctx context.Context, limit, offset int) ([]*model.AdminUser, error) {
	const q = `
		SELECT id, username, password_hash, name, email, status, created_at, updated_at
		FROM admin_users
		ORDER BY created_at, id
		LIMIT $1 OFFSET $2`
	rows, err := r.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.AdminUser
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

func (r *pgAdminUserRepo) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&n)
	return n, err
}

func (r *pgAdminUserRepo) FindByUsername(ctx context.Context, username string) (*model.AdminUser, error) {
	const q = `
		SELECT id, username, password_hash, name, email, status, created_at, updated_at
		FROM admin_users
		WHERE username = $1 AND status = 'active'
		LIMIT 1`
	return scanAdminUser(r.pool.QueryRow(ctx, q, username))
}

func (r *pgAdminUserRepo) FindByID(ctx context.Context, id string) (*model.AdminUser, error) {
	const q = `
		SELECT id, username, password_hash, name, email, status, created_at, updated_at
		FROM admin_users
		WHERE id = $1
		LIMIT 1`
	return scanAdminUser(r.pool.QueryRow(ctx, q, id))
}

func (r *pgAdminUserRepo) Create(ctx context.Context, u *model.AdminUser) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO admin_users (id, username, password_hash, name, email, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := tx.Exec(ctx, q,
		u.ID, u.Username, u.PasswordHash,
		u.Name, u.Email, u.Status,
		u.CreatedAt, u.UpdatedAt,
	); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "users", Action: "create", Operation: "新增管理员",
		TargetType: "admin_user", TargetID: u.ID, TargetName: u.Username,
		After: audit.Snapshot(snapshotOfAdminUser(u)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminUserRepo) UpdateStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 停用/启用是权限面的开关，before 与 after 的差异正是这次操作的全部内容。
	const selectSQL = `
		SELECT username, name, email, status FROM admin_users WHERE id = $1 FOR UPDATE`
	var before adminUserSnapshot
	if err := tx.QueryRow(ctx, selectSQL, id).Scan(
		&before.Username, &before.Name, &before.Email, &before.Status,
	); err != nil {
		return err
	}
	const q = `UPDATE admin_users SET status = $1 WHERE id = $2`
	if _, err := tx.Exec(ctx, q, status, id); err != nil {
		return err
	}
	after := before
	after.Status = status
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "users", Action: "update_status", Operation: "修改管理员状态",
		TargetType: "admin_user", TargetID: id, TargetName: before.Username,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func scanAdminUser(row pgx.Row) (*model.AdminUser, error) {
	u := &model.AdminUser{}
	err := row.Scan(
		&u.ID, &u.Username, &u.PasswordHash,
		&u.Name, &u.Email, &u.Status,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return u, nil
}
