package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/token"
)

type refreshTokenQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type refreshTokenExecutor func(context.Context, string, ...any) (pgconn.CommandTag, error)

type PostgreSQLRefreshTokenStore struct {
	pool refreshTokenQuerier
	exec refreshTokenExecutor
}

func NewPostgreSQLRefreshTokenStore(pool *pgxpool.Pool) *PostgreSQLRefreshTokenStore {
	return &PostgreSQLRefreshTokenStore{pool: pool, exec: pool.Exec}
}

func (s *PostgreSQLRefreshTokenStore) Register(ctx context.Context, record token.TokenRecord) error {
	if s.exec == nil {
		return errors.New("refresh token store executor is nil")
	}
	_, err := s.exec(ctx, `INSERT INTO refresh_tokens (jti, account_id, user_id, expires_at) VALUES ($1, NULLIF($2, '')::uuid, NULLIF($3, ''), $4) ON CONFLICT (jti) DO UPDATE SET account_id = EXCLUDED.account_id, user_id = EXCLUDED.user_id, expires_at = EXCLUDED.expires_at, revoked = FALSE, consumed = FALSE`, record.JTI, record.AccountID, record.UserID, record.ExpiresAt)
	return err
}

func (s *PostgreSQLRefreshTokenStore) Consume(ctx context.Context, jti string, now time.Time) (bool, error) {
	if s.pool == nil {
		return false, errors.New("refresh token store pool is nil")
	}
	var found string
	err := s.pool.QueryRow(ctx, `UPDATE refresh_tokens SET consumed = TRUE WHERE jti = $1 AND revoked = FALSE AND consumed = FALSE AND expires_at > $2 RETURNING jti`, jti, now).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *PostgreSQLRefreshTokenStore) Rotate(ctx context.Context, oldJTI string, record token.TokenRecord, now time.Time) (bool, error) {
	if s.pool == nil {
		return false, errors.New("refresh token store pool is nil")
	}
	var found string
	err := s.pool.QueryRow(ctx, `WITH consumed AS (
		UPDATE refresh_tokens SET consumed = TRUE
		WHERE jti = $1 AND revoked = FALSE AND consumed = FALSE AND expires_at > $2
		RETURNING jti
	)
	INSERT INTO refresh_tokens (jti, account_id, user_id, expires_at)
	SELECT $3, NULLIF($4, '')::uuid, NULLIF($5, ''), $6 FROM consumed
	RETURNING jti`, oldJTI, now, record.JTI, record.AccountID, record.UserID, record.ExpiresAt).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *PostgreSQLRefreshTokenStore) Revoke(ctx context.Context, jti string, now time.Time) (bool, error) {
	if s.pool == nil {
		return false, errors.New("refresh token store pool is nil")
	}
	var found string
	err := s.pool.QueryRow(ctx, `UPDATE refresh_tokens SET revoked = TRUE WHERE jti = $1 AND revoked = FALSE AND consumed = FALSE AND expires_at > $2 RETURNING jti`, jti, now).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// CleanupExpired removes expired records and returns the number deleted.
func (s *PostgreSQLRefreshTokenStore) CleanupExpired(ctx context.Context, before time.Time) (int64, error) {
	if s.exec == nil {
		return 0, errors.New("refresh token store executor is nil")
	}
	tag, err := s.exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at <= $1`, before)
	return tag.RowsAffected(), err
}

var _ token.Store = (*PostgreSQLRefreshTokenStore)(nil)
