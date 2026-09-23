package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// MerchantUserRepository 商户员工数据访问接口
type MerchantUserRepository interface {
	// FindPage 返回某商户下的一页账号（全量取用会随账号数增长而变慢，
	// 后台列表也不再需要一次拉全），总数走 Count，两者的 merchantID 必须一致。
	FindPage(ctx context.Context, merchantID string, limit, offset int) ([]*model.MerchantUser, error)
	Count(ctx context.Context, merchantID string) (int64, error)
	FindByUsername(ctx context.Context, username string) (*model.MerchantUser, error)
	FindByID(ctx context.Context, id string) (*model.MerchantUser, error)
	Create(ctx context.Context, u *model.MerchantUser) error
	UpdateStatus(ctx context.Context, id, status string) error
	UpdateScope(ctx context.Context, id, scopeType string, scopeIDs []string, isAdmin bool) error
	ResetScopeByTarget(ctx context.Context, scopeType, scopeID string) error
	HasUsers(ctx context.Context, merchantID string) (bool, error)
	TouchLogin(ctx context.Context, id, ip string) error
	Delete(ctx context.Context, id string) error
}

// merchantUserSnapshot 同样不含 PasswordHash，理由见 adminUserSnapshot。
// 数据范围（scope_type/scope_ids）必须记：它决定这个账号能看哪些门店。
type merchantUserSnapshot struct {
	MerchantID string   `json:"merchant_id"`
	Username   string   `json:"username"`
	Name       string   `json:"name"`
	Email      string   `json:"email,omitempty"`
	Phone      string   `json:"phone,omitempty"`
	Status     string   `json:"status"`
	IsAdmin    bool     `json:"is_admin"`
	ScopeType  string   `json:"scope_type"`
	ScopeIDs   []string `json:"scope_ids,omitempty"`
}

func snapshotOfMerchantUser(u *model.MerchantUser) merchantUserSnapshot {
	return merchantUserSnapshot{
		MerchantID: u.MerchantID, Username: u.Username, Name: u.Name,
		Email: u.Email, Phone: u.Phone, Status: u.Status,
		IsAdmin: u.IsAdmin, ScopeType: u.ScopeType, ScopeIDs: scopeIDsParameter(u.ScopeIDs),
	}
}

// selectMerchantUserForUpdate 读锁定行并组装快照，供 UpdateStatus/UpdateScope/Delete 共用。
func selectMerchantUserForUpdate(ctx context.Context, tx pgx.Tx, id string) (merchantUserSnapshot, error) {
	const q = `
		SELECT merchant_id, username, name, COALESCE(email, ''), COALESCE(phone, ''),
			status, is_admin, scope_type, scope_ids::text[]
		FROM merchant_users WHERE id = $1 FOR UPDATE`
	var s merchantUserSnapshot
	err := tx.QueryRow(ctx, q, id).Scan(
		&s.MerchantID, &s.Username, &s.Name, &s.Email, &s.Phone,
		&s.Status, &s.IsAdmin, &s.ScopeType, &s.ScopeIDs,
	)
	if s.ScopeIDs == nil {
		// 与 scanMerchantUser 同一条不变式，理由见那里。
		s.ScopeIDs = []string{}
	}
	return s, err
}

// scopeIDsParameter 保证交给 uuid[] 列的切片不是 nil。
//
// nil 在那个参数位置上编码成 SQL NULL，而 scope_ids 是 NOT NULL 列；更要紧的是空数组与
// NULL 在展开处读法相反——空数组是「一个点位都没授权」，NULL 是「不过滤」。写入口的
// 兜底放在这里，和读出口的 scanMerchantUser、平台层的 auth.WithStoreScope 是同一件事
// 的三道：任何一道漏了，这个区别就会在某条路径上被抹平。
func scopeIDsParameter(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
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
// scope_ids 转成 text[] 之后再扫：列是 uuid[]，扫进 []string 要么靠 pgx 的数组编解码
// 逐元素转，要么像这里一样让服务端先转好——同一条 SQL 里 COALESCE 那一列就是这么做的。
//
// 末尾的 scope_names 是展示用的范围名称：brands/stores 属于商户库，身份库这边
// 联不到，这里只占位，由 service 层经 gRPC 批量解析后填上。
const merchantUserColumns = `u.id, u.merchant_id, u.username, u.password_hash, u.name,
	COALESCE(u.email, ''), COALESCE(u.phone, ''), u.status,
	u.is_admin, u.scope_type, u.scope_ids::text[], COALESCE(u.avatar, ''),
	u.last_login_at, COALESCE(u.last_login_ip, ''), u.login_count,
	u.created_at, u.updated_at,
	'{}'::text[]`

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

// FindPage 以 id 作排序决胜位，理由同 adminUserRepo.FindPage。
func (r *pgMerchantUserRepo) FindPage(ctx context.Context, merchantID string, limit, offset int) ([]*model.MerchantUser, error) {
	q := `SELECT ` + merchantUserColumns + `
		FROM merchant_users u
		WHERE u.merchant_id = $1
		ORDER BY u.created_at, u.id
		LIMIT $2 OFFSET $3`
	rows, err := r.pool.Query(ctx, q, merchantID, limit, offset)
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

func (r *pgMerchantUserRepo) Count(ctx context.Context, merchantID string) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM merchant_users WHERE merchant_id = $1`, merchantID).Scan(&n)
	return n, err
}

func (r *pgMerchantUserRepo) Create(ctx context.Context, u *model.MerchantUser) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	const q = `
		INSERT INTO merchant_users (id, merchant_id, username, password_hash, name, email, phone, status,
			is_admin, scope_type, scope_ids, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::text[]::uuid[], $12, $13)`
	if _, err := tx.Exec(ctx, q,
		u.ID, u.MerchantID, u.Username, u.PasswordHash,
		u.Name, u.Email, u.Phone, u.Status,
		u.IsAdmin, u.ScopeType, scopeIDsParameter(u.ScopeIDs),
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

// UpdateScope 更新账号数据范围与管理员标记；空范围存空数组而**不是** NULL——两者在
// 展开处读法相反，见 scopeIDsParameter。
func (r *pgMerchantUserRepo) UpdateScope(ctx context.Context, id, scopeType string, scopeIDs []string, isAdmin bool) error {
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
		SET scope_type = $2, scope_ids = $3::text[]::uuid[], is_admin = $4, updated_at = NOW()
		WHERE id = $1`
	if _, err := tx.Exec(ctx, q, id, scopeType, scopeIDsParameter(scopeIDs), isAdmin); err != nil {
		return err
	}
	after := before
	after.ScopeType, after.ScopeIDs, after.IsAdmin = scopeType, scopeIDsParameter(scopeIDs), isAdmin
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

// ResetScopeByTarget 品牌/门店删除时收回它：把那个 id 从各账号的范围里摘掉。
//
// 摘完范围为空（这个账号原本只指向它）的账号回落成商户档——这正是范围只能有一个目标
// 时每一个被回收的账号都会走到的结果（004 的产品口径：回收为 scope_type=merchant）。
// 还有别的目标的账号保持原档位，只是少了一个目标。
//
// 一条语句做完：先摘后判空必须看到**同一个**数组，分两条的话第二条读的是已经更新过的行，
// 判断会跟第一条的实际结果错开。
func (r *pgMerchantUserRepo) ResetScopeByTarget(ctx context.Context, scopeType, scopeID string) error {
	const q = `
		UPDATE merchant_users
		SET scope_ids = array_remove(scope_ids, $2::uuid),
			scope_type = CASE
				WHEN cardinality(array_remove(scope_ids, $2::uuid)) = 0 THEN 'merchant'
				ELSE scope_type
			END,
			updated_at = NOW()
		WHERE scope_type = $1 AND $2::uuid = ANY(scope_ids)`
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
		&u.IsAdmin, &u.ScopeType, &u.ScopeIDs, &u.Avatar,
		&u.LastLoginAt, &u.LastLoginIP, &u.LoginCount,
		&u.CreatedAt, &u.UpdatedAt,
		&u.ScopeNames,
	)
	if err != nil {
		return nil, err
	}
	if u.ScopeIDs == nil {
		// 列是 NOT NULL，正常读不出 nil；真读出来了也归一成空切片——它一旦流到边界上，
		// nil 就是「不过滤」，而这件事只该在 WithStoreScope 那一处收口，不该靠调用方自觉。
		u.ScopeIDs = []string{}
	}
	return u, nil
}
