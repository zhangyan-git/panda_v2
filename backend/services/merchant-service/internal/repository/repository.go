package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
)

var ErrUnavailable = errors.New("merchant repository is unavailable")

type MerchantUserRepository interface {
	ResetScopeByTarget(context.Context, string, string) error
}

type unavailableScope struct{}

func NewUnavailableScope() MerchantUserRepository { return unavailableScope{} }

func (unavailableScope) ResetScopeByTarget(context.Context, string, string) error { return nil }

type Repository interface {
	Ping(context.Context) error
	ListMerchants(context.Context) ([]model.Merchant, error)
}

type unavailable struct{}

func NewUnavailable() Repository               { return unavailable{} }
func (unavailable) Ping(context.Context) error { return nil }
func (unavailable) ListMerchants(context.Context) ([]model.Merchant, error) {
	return nil, ErrUnavailable
}

type LegacyPostgres struct{ *pgMerchantRepo }

func NewLegacyPostgres(pool *pgxpool.Pool) Repository {
	return &LegacyPostgres{pgMerchantRepo: &pgMerchantRepo{pool: pool}}
}
func (r *LegacyPostgres) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }
func (r *LegacyPostgres) ListMerchants(ctx context.Context) ([]model.Merchant, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, name, status, COALESCE(contact_name, ''), COALESCE(contact_phone, ''), COALESCE(contact_email, ''), created_at, updated_at FROM merchants ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Merchant, 0)
	for rows.Next() {
		var m model.Merchant
		if err := rows.Scan(&m.ID, &m.Name, &m.Status, &m.ContactName, &m.ContactPhone, &m.ContactEmail, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
