package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

type PostgreSQL struct{ pool *pgxpool.Pool }

func NewPostgreSQL(pool *pgxpool.Pool) *PostgreSQL { return &PostgreSQL{pool: pool} }

const userColumns = `id, name, nickname, avatar_url, email, gender, birthday, occupation, hobbies, region_code, region_name, status, created_at, updated_at`

func scanUser(row pgx.Row) (model.User, error) {
	var user model.User
	err := row.Scan(&user.ID, &user.Name, &user.Nickname, &user.AvatarURL, &user.Email, &user.Gender, &user.Birthday, &user.Occupation, &user.Hobbies, &user.RegionCode, &user.RegionName, &user.Status, &user.CreatedAt, &user.UpdatedAt)
	return user, err
}

func (r *PostgreSQL) Create(ctx context.Context, name string) (model.User, error) {
	return scanUser(r.pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING `+userColumns, name))
}

func (r *PostgreSQL) GetByID(ctx context.Context, id int64) (model.User, error) {
	user, err := scanUser(r.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, model.ErrNotFound
	}
	return user, err
}

func (r *PostgreSQL) Update(ctx context.Context, id int64, update model.UserUpdate) (model.User, error) {
	user, err := scanUser(r.pool.QueryRow(ctx, `UPDATE users SET
		nickname = COALESCE($2, nickname), avatar_url = COALESCE($3, avatar_url), email = COALESCE($4, email),
		gender = COALESCE($5, gender), birthday = COALESCE($6, birthday), occupation = COALESCE($7, occupation),
		hobbies = COALESCE($8, hobbies), region_code = COALESCE($9, region_code), region_name = COALESCE($10, region_name),
		updated_at = NOW()
		WHERE id = $1 RETURNING `+userColumns,
		id, update.Nickname, update.AvatarURL, update.Email, update.Gender, update.Birthday, update.Occupation,
		update.Hobbies, update.RegionCode, update.RegionName))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.User{}, model.ErrNotFound
	}
	return user, err
}
