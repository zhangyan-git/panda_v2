package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
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

// MerchantUserRepository 商户员工数据访问接口
type MerchantUserRepository interface {
	FindByUsername(ctx context.Context, username string) (*model.MerchantUser, error)
	FindByID(ctx context.Context, id string) (*model.MerchantUser, error)
	FindByMerchant(ctx context.Context, merchantID string) ([]*model.MerchantUser, error)
	Create(ctx context.Context, u *model.MerchantUser) error
	UpdateStatus(ctx context.Context, id, status string) error
	UpdateScope(ctx context.Context, id, scopeType, scopeID string, isAdmin bool) error
	ResetScopeByTarget(ctx context.Context, scopeType, scopeID string) error
	HasUsers(ctx context.Context, merchantID string) (bool, error)
	TouchLogin(ctx context.Context, id, ip string) error
	Delete(ctx context.Context, id string) error
}

// merchantUserSnapshot 同样不含 PasswordHash，理由见 adminUserSnapshot。
// 数据范围（scope_type/scope_id）必须记：它决定这个账号能看哪些门店。
type merchantUserSnapshot struct {
	MerchantID string `json:"merchant_id"`
	Username   string `json:"username"`
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Phone      string `json:"phone,omitempty"`
	Status     string `json:"status"`
	IsAdmin    bool   `json:"is_admin"`
	ScopeType  string `json:"scope_type"`
	ScopeID    string `json:"scope_id,omitempty"`
}

func snapshotOfMerchantUser(u *model.MerchantUser) merchantUserSnapshot {
	return merchantUserSnapshot{
		MerchantID: u.MerchantID, Username: u.Username, Name: u.Name,
		Email: u.Email, Phone: u.Phone, Status: u.Status,
		IsAdmin: u.IsAdmin, ScopeType: u.ScopeType, ScopeID: u.ScopeID,
	}
}

// selectMerchantUserForUpdate 读锁定行并组装快照，供 UpdateStatus/UpdateScope/Delete 共用。
func selectMerchantUserForUpdate(ctx context.Context, tx pgx.Tx, id string) (merchantUserSnapshot, error) {
	const q = `
		SELECT merchant_id, username, name, COALESCE(email, ''), COALESCE(phone, ''),
			status, is_admin, scope_type, COALESCE(scope_id::text, '')
		FROM merchant_users WHERE id = $1 FOR UPDATE`
	var s merchantUserSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(
		&s.MerchantID, &s.Username, &s.Name, &s.Email, &s.Phone,
		&s.Status, &s.IsAdmin, &s.ScopeType, &s.ScopeID,
	)
	return s, err
}

type pgMerchantUserRepo struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

func NewMerchantUserRepository(pool *pgxpool.Pool, recorder audit.Recorder) MerchantUserRepository {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgMerchantUserRepo{pool: pool, audit: recorder}
}

// merchantUserColumns 统一 SELECT 列表；可空列归一化为空字符串方便扫描。
// 末尾的 scope_name 是展示用的范围名称：brands/stores 属于商户库，身份库这边
// 联不到，这里只占位，由 service 层经 gRPC 批量解析后填上。
const merchantUserColumns = `u.id, u.merchant_id, u.username, u.password_hash, u.name,
	COALESCE(u.email, ''), COALESCE(u.phone, ''), u.status,
	u.is_admin, u.scope_type, COALESCE(u.scope_id::text, ''), COALESCE(u.avatar, ''),
	u.last_login_at, COALESCE(u.last_login_ip, ''), u.login_count,
	u.created_at, u.updated_at,
	''`

// FindByUsername 按全局唯一 username 查询（003 迁移加约束）；
// 不过滤 status，登录链路需要区分「账号已禁用」和「账号不存在」
func (r *pgMerchantUserRepo) FindByUsername(ctx context.Context, username string) (*model.MerchantUser, error) {
	q := `SELECT ` + merchantUserColumns + `
		FROM merchant_users u
		WHERE u.username = $1
		LIMIT 1`
	return scanMerchantUser(r.pool.QueryRow(ctx, q, username))
}

func (r *pgMerchantUserRepo) FindByID(ctx context.Context, id string) (*model.MerchantUser, error) {
	q := `SELECT ` + merchantUserColumns + `
		FROM merchant_users u
		WHERE u.id = $1
		LIMIT 1`
	return scanMerchantUser(r.pool.QueryRow(ctx, q, id))
}

func (r *pgMerchantUserRepo) FindByMerchant(ctx context.Context, merchantID string) ([]*model.MerchantUser, error) {
	q := `SELECT ` + merchantUserColumns + `
		FROM merchant_users u
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
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO merchant_users (id, merchant_id, username, password_hash, name, email, phone, status,
			is_admin, scope_type, scope_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	if _, err := tx.Exec(ctx, q,
		u.ID, u.MerchantID, u.Username, u.PasswordHash,
		u.Name, u.Email, u.Phone, u.Status,
		u.IsAdmin, u.ScopeType, nullEmpty(u.ScopeID),
		u.CreatedAt, u.UpdatedAt,
	); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchant_users", Action: "create", Operation: "新增商户账号",
		TargetType: "merchant_user", TargetID: u.ID, TargetName: u.Username,
		MerchantID: u.MerchantID,
		After:      audit.Snapshot(snapshotOfMerchantUser(u)),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgMerchantUserRepo) UpdateStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMerchantUserForUpdate(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `UPDATE merchant_users SET status = $1 WHERE id = $2`
	if _, err := tx.Exec(ctx, q, status, id); err != nil {
		return err
	}
	after := before
	after.Status = status
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchant_users", Action: "update_status", Operation: "修改商户账号状态",
		TargetType: "merchant_user", TargetID: id, TargetName: before.Username,
		MerchantID: before.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgMerchantUserRepo) Delete(ctx context.Context, id string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMerchantUserForUpdate(ctx, tx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	const q = `DELETE FROM merchant_users WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id); err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchant_users", Action: "delete", Operation: "删除商户账号",
		TargetType: "merchant_user", TargetID: id, TargetName: before.Username,
		MerchantID: before.MerchantID,
		Before:     audit.Snapshot(before),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateScope 更新账号数据范围与管理员标记；scopeID 为空时在库中存 NULL
func (r *pgMerchantUserRepo) UpdateScope(ctx context.Context, id, scopeType, scopeID string, isAdmin bool) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectMerchantUserForUpdate(ctx, tx, id)
	if err != nil {
		return err
	}
	const q = `
		UPDATE merchant_users
		SET scope_type = $2, scope_id = $3, is_admin = $4, updated_at = NOW()
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id, scopeType, nullEmpty(scopeID), isAdmin); err != nil {
		return err
	}
	after := before
	after.ScopeType, after.ScopeID, after.IsAdmin = scopeType, scopeID, isAdmin
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "merchant_users", Action: "update_scope", Operation: "调整商户账号范围",
		TargetType: "merchant_user", TargetID: id, TargetName: before.Username,
		MerchantID: before.MerchantID,
		Before:     audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

// HasUsers 商户是否还挂着账号；merchant-service 删除商户前据此拒绝级联删除
func (r *pgMerchantUserRepo) HasUsers(ctx context.Context, merchantID string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM merchant_users WHERE merchant_id = $1)`
	var exists bool
	err := r.pool.QueryRow(ctx, q, merchantID).Scan(&exists)
	return exists, err
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
