package repository

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/domain"
)

type mockRow struct {
	values []any
	err    error
}

func (r mockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch value := dest[i].(type) {
		case *string:
			*value = r.values[i].(string)
		case **string:
			text := r.values[i].(string)
			*value = &text
		case *domain.AccountType:
			*value = domain.AccountType(r.values[i].(string))
		case *domain.Status:
			*value = domain.Status(r.values[i].(string))
		}
	}
	return nil
}

type mockQuerier struct{ row pgx.Row }

func (q mockQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

func TestPostgreSQLGetByUsername(t *testing.T) {
	r := &PostgreSQL{pool: mockQuerier{row: mockRow{values: []any{"a1", "admin", "hash", "admin", "u1", "active"}}}}
	account, err := r.GetByUsername(context.Background(), "admin", domain.AdminAccount)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "a1" || account.Type != domain.AdminAccount || account.Status != domain.StatusActive || account.UserID != "u1" {
		t.Fatalf("unexpected account: %+v", account)
	}
}

func TestPostgreSQLGetByID(t *testing.T) {
	r := &PostgreSQL{pool: mockQuerier{row: mockRow{values: []any{"a1", "admin", "hash", "admin", "u1", "active"}}}}
	account, err := r.GetByID(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "a1" || account.Type != domain.AdminAccount || account.Status != domain.StatusActive || account.UserID != "u1" {
		t.Fatalf("unexpected account: %+v", account)
	}
}

func TestPostgreSQLGetByUsernameNotFound(t *testing.T) {
	r := &PostgreSQL{pool: mockQuerier{row: mockRow{err: pgx.ErrNoRows}}}
	_, err := r.GetByUsername(context.Background(), "missing", domain.AdminAccount)
	if err != ErrNotFound {
		t.Fatalf("error = %v, want %v", err, ErrNotFound)
	}
}

func TestPostgreSQLGetByIDNotFound(t *testing.T) {
	r := &PostgreSQL{pool: mockQuerier{row: mockRow{err: pgx.ErrNoRows}}}
	_, err := r.GetByID(context.Background(), "missing")
	if err != ErrNotFound {
		t.Fatalf("error = %v, want %v", err, ErrNotFound)
	}
}
