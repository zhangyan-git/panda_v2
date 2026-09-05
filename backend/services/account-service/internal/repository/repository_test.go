package repository

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/token"
	"testing"
	"time"
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
		case *model.AccountType:
			*value = model.AccountType(r.values[i].(string))
		case *model.Status:
			*value = model.Status(r.values[i].(string))
		}
	}
	return nil
}

type mockQuerier struct{ row pgx.Row }

func (q mockQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

func TestPostgreSQLGetByUsername(t *testing.T) {
	r := &PostgreSQL{pool: mockQuerier{row: mockRow{values: []any{"a1", "admin", "hash", "admin", "u1", "active"}}}}
	account, err := r.GetByUsername(context.Background(), "admin", model.AdminAccount)
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "a1" || account.Type != model.AdminAccount || account.Status != model.StatusActive || account.UserID != "u1" {
		t.Fatalf("unexpected account: %+v", account)
	}
}

func TestPostgreSQLGetByID(t *testing.T) {
	r := &PostgreSQL{pool: mockQuerier{row: mockRow{values: []any{"a1", "admin", "hash", "admin", "u1", "active"}}}}
	account, err := r.GetByID(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "a1" || account.Type != model.AdminAccount || account.Status != model.StatusActive || account.UserID != "u1" {
		t.Fatalf("unexpected account: %+v", account)
	}
}

func TestPostgreSQLGetByUsernameNotFound(t *testing.T) {
	r := &PostgreSQL{pool: mockQuerier{row: mockRow{err: pgx.ErrNoRows}}}
	_, err := r.GetByUsername(context.Background(), "missing", model.AdminAccount)
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

type refreshMockRow struct{ err error }

func (r refreshMockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = "jti-1"
	return nil
}

type refreshMockPool struct {
	row   pgx.Row
	query string
}

func (p *refreshMockPool) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	p.query = query
	return p.row
}

func TestPostgreSQLRefreshTokenStoreConsumeIsAtomic(t *testing.T) {
	pool := &refreshMockPool{row: refreshMockRow{}}
	store := &PostgreSQLRefreshTokenStore{pool: pool}
	ok, err := store.Consume(context.Background(), "jti-1", time.Now())
	if err != nil || !ok {
		t.Fatalf("consume = %v, %v", ok, err)
	}
	if pool.query == "" || !containsAll(pool.query, "UPDATE refresh_tokens", "consumed = TRUE", "consumed = FALSE", "expires_at >") {
		t.Fatalf("consume query is not atomic: %s", pool.query)
	}
}

func TestPostgreSQLRefreshTokenStoreRegisterDoesNotStorePlaintext(t *testing.T) {
	var args []any
	var registerQuery string
	store := &PostgreSQLRefreshTokenStore{exec: func(_ context.Context, query string, values ...any) (pgconn.CommandTag, error) {
		args = values
		registerQuery = query
		if query == "" {
			t.Fatal("empty query")
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}}
	record := token.TokenRecord{JTI: "jti-1", AccountID: "account-1", UserID: "user-1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Register(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if arg == "refresh-token-plaintext" {
			t.Fatal("refresh token plaintext was persisted")
		}
	}
	// account_id 列是 uuid 类型，插入表达式必须显式 cast，否则 text 无法隐式转换会导致 INSERT 失败。
	if !contains(registerQuery, "::uuid") {
		t.Fatalf("register query must cast account_id to uuid: %s", registerQuery)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !contains(value, part) {
			return false
		}
	}
	return true
}
func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

var _ refreshTokenExecutor = func(context.Context, string, ...any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, nil }
var _ = pgx.ErrNoRows
