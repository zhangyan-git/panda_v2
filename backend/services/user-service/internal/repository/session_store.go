package repository

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

// sessionStore 是两张**同形**会话表（user_sessions / admin_sessions）的共用实现。
//
// 为什么要共用：这两张表的列、索引、撤销语义逐字相同（见 migrations/identity），
// 差别只有表名和外键指向哪张账号表。抄两遍的话，「撤销必须是原子抢占」这类关键
// 条件只要有一边被改松，另一边就会静默地弱下去——而它俩谁弱了都不会有测试变红。
//
// table 只由本文件的构造函数用字面量塞进来，不接受任何外部输入，所以拼进 SQL 是安全的。
// 这不是「参数化查询的反面教材」：表名在 PostgreSQL 里**不能**用占位符，只能是这一种做法。
type sessionStore struct {
	pool  *pgxpool.Pool
	table string
}

// sessionColumns 顺序必须与 scanSession 对齐。revoked_at / last_used_at
// 保持指针语义：扫描进 *time.Time 时 NULL 就是 nil，不需要 COALESCE——
// 「没撤销」和「撤销于零时刻」是两件事，用空值顶替会把这个区别抹掉。
const sessionColumns = `id, user_id, refresh_token_hash, issued_at, expires_at,
	revoked_at, revoke_reason, last_used_at, COALESCE(ip, ''), COALESCE(user_agent, ''),
	created_at, updated_at`

func (r *sessionStore) CreateSession(ctx context.Context, s *model.UserSession) error {
	q := `
		INSERT INTO ` + r.table + ` (id, user_id, refresh_token_hash, issued_at, expires_at,
			revoked_at, revoke_reason, last_used_at, ip, user_agent, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	_, err := r.pool.Exec(ctx, q,
		s.ID, s.UserID, s.RefreshTokenHash, s.IssuedAt, s.ExpiresAt,
		s.RevokedAt, s.RevokeReason, s.LastUsedAt, s.IP, s.UserAgent,
		s.CreatedAt, s.UpdatedAt,
	)
	return err
}

func (r *sessionStore) FindSessionByHash(ctx context.Context, hash string) (*model.UserSession, error) {
	q := `SELECT ` + sessionColumns + `
		FROM ` + r.table + `
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

func (r *sessionStore) RevokeSession(ctx context.Context, id, reason string) error {
	// revoked_at IS NULL 这个条件是整条语句的关键，不是优化：
	// 它让「撤销」变成一次原子的抢占，并发刷新里只有一个调用方能成功。
	q := `
		UPDATE ` + r.table + `
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

func (r *sessionStore) RevokeUserSessions(ctx context.Context, userID, reason string) error {
	q := `
		UPDATE ` + r.table + `
		SET revoked_at = NOW(), revoke_reason = $2, updated_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := r.pool.Exec(ctx, q, userID, reason)
	return err
}

func (r *sessionStore) CountActiveByUser(ctx context.Context, userID string) (int64, error) {
	q := `
		SELECT count(*)
		FROM ` + r.table + `
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > NOW()`
	var n int64
	err := r.pool.QueryRow(ctx, q, userID).Scan(&n)
	return n, err
}

func (r *sessionStore) TouchSessionUsed(ctx context.Context, id string) error {
	q := `UPDATE ` + r.table + ` SET last_used_at = NOW(), updated_at = NOW() WHERE id = $1`
	_, err := r.pool.Exec(ctx, q, id)
	return err
}
