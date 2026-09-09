package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// AdminUserRepository 平台管理员数据访问接口
type AdminUserRepository interface {
	FindAll(ctx context.Context) ([]*model.AdminUser, error)
	FindByUsername(ctx context.Context, username string) (*model.AdminUser, error)
	FindByID(ctx context.Context, id string) (*model.AdminUser, error)
	Create(ctx context.Context, u *model.AdminUser) error
	UpdateStatus(ctx context.Context, id, status string) error
}

type pgAdminUserRepo struct {
	pool *pgxpool.Pool
}

func NewAdminUserRepository(pool *pgxpool.Pool) AdminUserRepository {
	return &pgAdminUserRepo{pool: pool}
}

func (r *pgAdminUserRepo) FindAll(ctx context.Context) ([]*model.AdminUser, error) {
	const q = `
		SELECT id, username, password_hash, name, email, status, created_at, updated_at
		FROM admin_users
		ORDER BY created_at`
	rows, err := r.pool.Query(ctx, q)
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
	const q = `
		INSERT INTO admin_users (id, username, password_hash, name, email, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := r.pool.Exec(ctx, q,
		u.ID, u.Username, u.PasswordHash,
		u.Name, u.Email, u.Status,
		u.CreatedAt, u.UpdatedAt,
	)
	return err
}

func (r *pgAdminUserRepo) UpdateStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE admin_users SET status = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, q, status, id)
	return err
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

// MerchantUserRepository 商户员工数据访问接口
type MerchantUserRepository interface {
	FindByUsername(ctx context.Context, username string) (*model.MerchantUser, error)
	FindByID(ctx context.Context, id string) (*model.MerchantUser, error)
	FindByMerchant(ctx context.Context, merchantID string) ([]*model.MerchantUser, error)
	Create(ctx context.Context, u *model.MerchantUser) error
	UpdateStatus(ctx context.Context, id, status string) error
	UpdateScope(ctx context.Context, id, scopeType, scopeID string, isAdmin bool) error
	ResetScopeByTarget(ctx context.Context, scopeType, scopeID string) error
	TouchLogin(ctx context.Context, id, ip string) error
	Delete(ctx context.Context, id string) error
}

type pgMerchantUserRepo struct {
	pool *pgxpool.Pool
}

func NewMerchantUserRepository(pool *pgxpool.Pool) MerchantUserRepository {
	return &pgMerchantUserRepo{pool: pool}
}

// merchantUserColumns 统一 SELECT 列表；可空列归一化为空字符串方便扫描，
// scope_name 为联表计算列（范围品牌/门店名称），仅展示用
const merchantUserColumns = `u.id, u.merchant_id, u.username, u.password_hash, u.name,
	COALESCE(u.email, ''), COALESCE(u.phone, ''), u.status,
	u.is_admin, u.scope_type, COALESCE(u.scope_id::text, ''), COALESCE(u.avatar, ''),
	u.last_login_at, COALESCE(u.last_login_ip, ''), u.login_count,
	u.created_at, u.updated_at,
	COALESCE(b.name, s.name, '')`

const merchantUserJoins = `
	LEFT JOIN brands b ON u.scope_type = 'brand' AND u.scope_id = b.id
	LEFT JOIN stores s ON u.scope_type = 'store' AND u.scope_id = s.id`

// FindByUsername 按全局唯一 username 查询（003 迁移加约束）；
// 不过滤 status，登录链路需要区分「账号已禁用」和「账号不存在」
func (r *pgMerchantUserRepo) FindByUsername(ctx context.Context, username string) (*model.MerchantUser, error) {
	q := `SELECT ` + merchantUserColumns + `
		FROM merchant_users u` + merchantUserJoins + `
		WHERE u.username = $1
		LIMIT 1`
	return scanMerchantUser(r.pool.QueryRow(ctx, q, username))
}

func (r *pgMerchantUserRepo) FindByID(ctx context.Context, id string) (*model.MerchantUser, error) {
	q := `SELECT ` + merchantUserColumns + `
		FROM merchant_users u` + merchantUserJoins + `
		WHERE u.id = $1
		LIMIT 1`
	return scanMerchantUser(r.pool.QueryRow(ctx, q, id))
}

func (r *pgMerchantUserRepo) FindByMerchant(ctx context.Context, merchantID string) ([]*model.MerchantUser, error) {
	q := `SELECT ` + merchantUserColumns + `
		FROM merchant_users u` + merchantUserJoins + `
		WHERE u.merchant_id = $1
		ORDER BY u.created_at`
	rows, err := r.pool.Query(ctx, q, merchantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []*model.MerchantUser
	for rows.Next() {
		u, err := scanMerchantUser(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

func (r *pgMerchantUserRepo) Create(ctx context.Context, u *model.MerchantUser) error {
	const q = `
		INSERT INTO merchant_users (id, merchant_id, username, password_hash, name, email, phone, status,
			is_admin, scope_type, scope_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	_, err := r.pool.Exec(ctx, q,
		u.ID, u.MerchantID, u.Username, u.PasswordHash,
		u.Name, u.Email, u.Phone, u.Status,
		u.IsAdmin, u.ScopeType, nullEmpty(u.ScopeID),
		u.CreatedAt, u.UpdatedAt,
	)
	return err
}

func (r *pgMerchantUserRepo) UpdateStatus(ctx context.Context, id, status string) error {
	const q = `UPDATE merchant_users SET status = $1 WHERE id = $2`
	_, err := r.pool.Exec(ctx, q, status, id)
	return err
}

func (r *pgMerchantUserRepo) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM merchant_users WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id)
	return err
}

// UpdateScope 更新账号数据范围与管理员标记；scopeID 为空时在库中存 NULL
func (r *pgMerchantUserRepo) UpdateScope(ctx context.Context, id, scopeType, scopeID string, isAdmin bool) error {
	const q = `
		UPDATE merchant_users
		SET scope_type = $2, scope_id = $3, is_admin = $4, updated_at = NOW()
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id, scopeType, nullEmpty(scopeID), isAdmin)
	return err
}

// ResetScopeByTarget 品牌/门店删除时，把指向它的账号回收为商户级范围
func (r *pgMerchantUserRepo) ResetScopeByTarget(ctx context.Context, scopeType, scopeID string) error {
	const q = `
		UPDATE merchant_users
		SET scope_type = 'merchant', scope_id = NULL, updated_at = NOW()
		WHERE scope_id = $2 AND scope_type = $1`
	_, err := r.pool.Exec(ctx, q, scopeType, scopeID)
	return err
}

// TouchLogin 登录成功后更新最后登录信息（尽力而为，失败不影响登录）
func (r *pgMerchantUserRepo) TouchLogin(ctx context.Context, id, ip string) error {
	const q = `
		UPDATE merchant_users
		SET last_login_at = NOW(), last_login_ip = $2, login_count = login_count + 1
		WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id, ip)
	return err
}

func scanMerchantUser(row pgx.Row) (*model.MerchantUser, error) {
	u := &model.MerchantUser{}
	err := row.Scan(
		&u.ID, &u.MerchantID, &u.Username, &u.PasswordHash,
		&u.Name, &u.Email, &u.Phone, &u.Status,
		&u.IsAdmin, &u.ScopeType, &u.ScopeID, &u.Avatar,
		&u.LastLoginAt, &u.LastLoginIP, &u.LoginCount,
		&u.CreatedAt, &u.UpdatedAt,
		&u.ScopeName,
	)
	if err != nil {
		return nil, err
	}
	return u, nil
}
