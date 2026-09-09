package migrate

import (
	"context"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("ACCOUNT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ACCOUNT_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	return pool, ctx
}

func TestApplyIsIdempotentAndOrdered(t *testing.T) {
	pool, ctx := testPool(t)

	// PostgreSQL folds unquoted identifiers to lower case, so information_schema
	// only ever matches a lower-case name.
	table := "migrate_test_ordered"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), `DROP TABLE IF EXISTS `+table)
		_, _ = pool.Exec(context.WithoutCancel(ctx),
			`DELETE FROM schema_migrations WHERE version LIKE '9%_migrate_test%'`)
	})

	fsys := fstest.MapFS{
		// The second file depends on the first, so applying them out of order
		// fails loudly rather than silently skipping a column.
		"901_migrate_test_create.sql": {Data: []byte(`CREATE TABLE ` + table + ` (id INT PRIMARY KEY)`)},
		"902_migrate_test_alter.sql":  {Data: []byte(`ALTER TABLE ` + table + ` ADD COLUMN label TEXT`)},
		"ignored.txt":                 {Data: []byte("not sql")},
	}

	if err := Apply(ctx, pool, fsys); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// A second run must be a no-op; re-running CREATE TABLE would error.
	if err := Apply(ctx, pool, fsys); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	var columns int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = $1`, table).Scan(&columns); err != nil {
		t.Fatalf("count columns: %v", err)
	}
	if columns != 2 {
		t.Fatalf("columns = %d, want 2", columns)
	}

	var applied int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version LIKE '9%_migrate_test%'`).Scan(&applied); err != nil {
		t.Fatalf("count applied: %v", err)
	}
	if applied != 2 {
		t.Fatalf("applied versions = %d, want 2", applied)
	}
}

func TestApplyRollsBackFailedMigration(t *testing.T) {
	pool, ctx := testPool(t)

	table := "migrate_fail_test"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), `DROP TABLE IF EXISTS `+table)
		_, _ = pool.Exec(context.WithoutCancel(ctx),
			`DELETE FROM schema_migrations WHERE version = '903_migrate_fail.sql'`)
	})

	fsys := fstest.MapFS{
		"903_migrate_fail.sql": {Data: []byte(
			`CREATE TABLE ` + table + ` (id INT); THIS IS NOT SQL;`)},
	}
	if err := Apply(ctx, pool, fsys); err == nil {
		t.Fatal("apply must fail on invalid SQL")
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if exists {
		t.Fatal("the failed migration left a partial schema behind")
	}

	var recorded bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '903_migrate_fail.sql')`).Scan(&recorded); err != nil {
		t.Fatalf("check version row: %v", err)
	}
	if recorded {
		t.Fatal("a failed migration must not be recorded as applied")
	}
}

func TestApplyRequiresPool(t *testing.T) {
	fsys := fstest.MapFS{"001.sql": {Data: []byte("SELECT 1")}}
	if err := Apply(context.Background(), nil, fsys); err == nil {
		t.Fatal("apply must reject a nil pool")
	}
}
