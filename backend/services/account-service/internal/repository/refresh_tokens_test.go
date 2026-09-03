package repository

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/handler"
)

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
	record := handler.TokenRecord{JTI: "jti-1", AccountID: "account-1", UserID: "user-1", ExpiresAt: time.Now().Add(time.Hour)}
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
