package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type PostgreSQL struct {
	pool rowQuerier
	exec func(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func NewPostgreSQL(pool *pgxpool.Pool) *PostgreSQL {
	return &PostgreSQL{pool: pool, exec: pool.Exec}
}

const accountColumns = `id, username, password_hash, type, user_id, status`

func scanAccount(row pgx.Row) (model.Account, error) {
	var account model.Account
	var userID *string
	err := row.Scan(&account.ID, &account.Username, &account.PasswordHash, &account.Type, &userID, &account.Status)
	if userID != nil {
		account.UserID = *userID
	}
	return account, err
}

func (r *PostgreSQL) GetByUsername(ctx context.Context, username string, accountType model.AccountType) (model.Account, error) {
	account, err := scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE username = $1 AND type = $2`, username, accountType))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Account{}, ErrNotFound
	}
	return account, err
}

func (r *PostgreSQL) GetByID(ctx context.Context, id string) (model.Account, error) {
	account, err := scanAccount(r.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Account{}, ErrNotFound
	}
	return account, err
}

func (r *PostgreSQL) Upsert(ctx context.Context, account model.Account) error {
	if r.exec == nil {
		return errors.New("account repository executor is nil")
	}
	_, err := r.exec(ctx, `INSERT INTO accounts (username, type, password_hash, status) VALUES ($1, $2, $3, $4) ON CONFLICT (username, type) DO UPDATE SET password_hash = EXCLUDED.password_hash, status = EXCLUDED.status, updated_at = NOW()`, account.Username, account.Type, account.PasswordHash, account.Status)
	return err
}

var _ model.Repository = (*PostgreSQL)(nil)
