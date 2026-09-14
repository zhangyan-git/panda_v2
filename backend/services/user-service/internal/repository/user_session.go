package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// UserSessionRepository 是 Refresh Token 会话的数据访问接口。
//
// 撤销用「置 revoked_at」而不是删行：一枚已经轮换掉的 token 再被使用时，那一行
// 还在才能识别出「盗用」这件事。行删了，攻击者拿着旧 token 来换，系统看到的只
// 是一个查不到的哈希，和有人的 token 打错字完全没法区分。
type UserSessionRepository interface {
	CreateSession(ctx context.Context, s *model.UserSession) error
	// FindSessionByHash 按 refresh token 的确定性哈希反查。用哈希而不是原文，
	// 是因为表里根本不存原文；返回的行可能已撤销或已过期，判断留给调用方。
	FindSessionByHash(ctx context.Context, hash string) (*model.UserSession, error)
	// RevokeSession 撤销一枚会话。只对当前有效的行生效：已经撤销过的行会返回
	// pgx.ErrNoRows，而不是把新的撤销原因覆盖到旧的上面——首次撤销的原因才是
	// 事实（rotated 就是 rotated，不该被后来的 reuse_detected 改写）。
	//
	// 返回 ErrNoRows 同时还是一个乐观锁信号：并发刷新同一枚 token 时，两条
	// 请求都会先读到「有效」，但只有一条能把 revoked_at 从 NULL 改掉，另一条
	// 拿到 0 行。刷新链路据此判定为盗用并踢掉整条会话链，见 service。
	RevokeSession(ctx context.Context, id, reason string) error
	// RevokeUserSessions 撤销某用户当前全部有效会话，用于退出登录、改绑手机号、
	// 账号被禁用/注销，以及检测到 token 盗用。已经撤销过的行不动，所以
	// reuse_detected 这个原因只落在「盗用发生时還活着的那些」会话上，
	// 一批会话在同一时刻被标成它，本身就是盗用发生过的证据。
	//
	// 注意它走连接池、自带事务，所以后台「禁用并踢下线」那条路径不能在它自己的
	// 事务里调它——状态与撤销必须同事务提交，见 repository/admin_miniapp_user.go。
	RevokeUserSessions(ctx context.Context, userID, reason string) error
	// CountActiveByUser 当前仍然有效的会话数（未撤销且未过期），后台详情页用。
	// 过期但未撤销的行不算：它们换不出新令牌，把它们算进去等于让页面显示一个
	// 比实际更大的数字，而管理员正是靠这个数字判断「禁用后还有没有人挂在线上」。
	CountActiveByUser(ctx context.Context, userID string) (int64, error)
	// TouchSessionUsed 记录这枚 refresh token 最近一次被使用的时间，尽力而为。
	TouchSessionUsed(ctx context.Context, id string) error
}

type pgUserSessionRepo struct {
	pool *pgxpool.Pool
}

// NewUserSessionRepository 构造会话仓库。
func NewUserSessionRepository(pool *pgxpool.Pool) UserSessionRepository {
	return &pgUserSessionRepo{pool: pool}
}

// userSessionColumns 顺序必须与 scanUserSession 对齐。revoked_at / last_used_at
// 保持指针语义：扫描进 *time.Time 时 NULL 就是 nil，不需要 COALESCE——
// 「没撤销」和「撤销于零时刻」是两件事，用空值顶替会把这个区别抹掉。
const userSessionColumns = `id, user_id, refresh_token_hash, issued_at, expires_at,
	revoked_at, revoke_reason, last_used_at, COALESCE(ip, ''), COALESCE(user_agent, ''),
	created_at, updated_at`

func (r *pgUserSessionRepo) CreateSession(ctx context.Context, s *model.UserSession) error {
	const q = `
		INSERT INTO user_sessions (id, user_id, refresh_token_hash, issued_at, expires_at,
			revoked_at, revoke_reason, last_used_at, ip, user_agent, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	_, err := r.pool.Exec(ctx, q,
		s.ID, s.UserID, s.RefreshTokenHash, s.IssuedAt, s.ExpiresAt,
		s.RevokedAt, s.RevokeReason, s.LastUsedAt, s.IP, s.UserAgent,
		s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (r *pgUserSessionRepo) FindSessionByHash(ctx context.Context, hash string) (*model.UserSession, error) {
	q := `SELECT ` + userSessionColumns + `
		FROM user_sessions
		WHERE refresh_token_hash = $1
		LIMIT 1`
	s := &model.UserSession{}
	err := r.pool.QueryRow(ctx, q, hash).Scan(
		&s.ID, &s.UserID, &s.RefreshTokenHash, &s.IssuedAt, &s.ExpiresAt,
		&s.RevokedAt, &s.RevokeReason, &s.LastUsedAt, &s.IP, &s.UserAgent,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r *pgUserSessionRepo) RevokeSession(ctx context.Context, id, reason string) error {
	// revoked_at IS NULL 这个条件是整条语句的关键，不是优化：
	// 它让「撤销」变成一次原子的抢占，并发刷新里只有一个调用方能成功。
	const q = `
		UPDATE user_sessions
		SET revoked_at = NOW(), revoke_reason = $2, updated_at = NOW()
		WHERE id = $1 AND revoked_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, id, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *pgUserSessionRepo) RevokeUserSessions(ctx context.Context, userID, reason string) error {
	const q = `
		UPDATE user_sessions
		SET revoked_at = NOW(), revoke_reason = $2, updated_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := r.pool.Exec(ctx, q, userID, reason)
	return err
}

func (r *pgUserSessionRepo) CountActiveByUser(ctx context.Context, userID string) (int64, error) {
	const q = `
		SELECT count(*)
		FROM user_sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()`
	var n int64
	err := r.pool.QueryRow(ctx, q, userID).Scan(&n)
	return n, err
}

func (r *pgUserSessionRepo) TouchSessionUsed(ctx context.Context, id string) error {
	const q = `UPDATE user_sessions SET last_used_at = NOW(), updated_at = NOW() WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id)
	return err
}
