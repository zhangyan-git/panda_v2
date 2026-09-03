package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/domain"
)

type PostgreSQL struct{ pool *pgxpool.Pool }

func NewPostgreSQL(pool *pgxpool.Pool) *PostgreSQL { return &PostgreSQL{pool: pool} }

func (r *PostgreSQL) Create(ctx context.Context, name string) (domain.User, error) {
	var user domain.User
	err := r.pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id, name, created_at`, name).Scan(&user.ID, &user.Name, &user.CreatedAt)
	return user, err
}

func (r *PostgreSQL) GetByID(ctx context.Context, id int64) (domain.User, error) {
	var user domain.User
	err := r.pool.QueryRow(ctx, `SELECT id, name, created_at FROM users WHERE id = $1`, id).Scan(&user.ID, &user.Name, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrNotFound
	}
	return user, err
}
