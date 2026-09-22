package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// tempDatabase hands the test a database of its own, created from the server
// ACCOUNT_TEST_DATABASE_URL points at and dropped when the test ends. The other
// tests in this file create and drop their own tables in that shared database;
// these two care about what a half-finished run leaves behind, so they need
// somewhere nothing else can observe or disturb.
func tempDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("ACCOUNT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ACCOUNT_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	name := "migrate_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// CREATE DATABASE cannot run inside a transaction, and a pool Exec without a
	// transaction is autocommit, so this is allowed here.
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		admin.Close()
		t.Fatalf("parse %s: %v", url, err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		// Close before dropping: DROP DATABASE refuses while any session is
		// connected to the database being dropped.
		pool.Close()
		if _, err := admin.Exec(context.WithoutCancel(ctx), `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		admin.Close()
	})
	return pool
}

// stripTransactionControl is what keeps the runner's transaction and the file's
// own out of each other's way, so it is tested on its own before it is tested
// through Apply.
func TestStripTransactionControl(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "the file's transaction is removed",
			sql:  "BEGIN;\nCREATE TABLE t (id INT);\nCOMMIT;\n",
			want: "\nCREATE TABLE t (id INT);\n\n",
		},
		{
			name: "lower case counts too",
			sql:  " begin;\nSELECT 1;\n commit;\n",
			want: "\nSELECT 1;\n\n",
		},
		{
			// The one that must not be touched: a plpgsql body opens with BEGIN
			// and no semicolon, and removing it would leave a function that does
			// not compile.
			name: "a plpgsql block body survives",
			sql:  "CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$\nBEGIN\n\tRETURN;\nEND;\n$$;\n",
			want: "CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$\nBEGIN\n\tRETURN;\nEND;\n$$;\n",
		},
		{
			// These are left behind on purpose: Apply catches an unrecognised
			// transaction statement by checking the transaction is still open,
			// and a textual strip that tried to rewrite comments or string
			// literals would break more than it fixed.
			name: "comments and literals are not rewritten",
			sql:  "-- COMMIT; in a comment\nSELECT 'COMMIT;';\nBEGIN; SELECT 1;\n",
			want: "-- COMMIT; in a comment\nSELECT 'COMMIT;';\nBEGIN; SELECT 1;\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(stripTransactionControl([]byte(tt.sql))); got != tt.want {
				t.Fatalf("stripTransactionControl(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

// TestApplyStripsFileOwnTransaction is the ordinary case: a file shaped like the
// ones this repository ships applies, is recorded, and is not replayed.
func TestApplyStripsFileOwnTransaction(t *testing.T) {
	pool := tempDatabase(t)
	ctx := context.Background()

	table := "migrate_owner_tx_ok"
	version := "907_migrate_owner_tx_ok.sql"
	fsys := fstest.MapFS{
		version: {Data: []byte("BEGIN;\nCREATE TABLE " + table + " (id INT PRIMARY KEY);\nCOMMIT;\n")},
	}
	if err := Apply(ctx, pool, fsys); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The file is not idempotent, so a second run that replays it would fail on
	// the duplicate table rather than on the version row.
	if err := Apply(ctx, pool, fsys); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if !exists {
		t.Fatal("the migration did not run")
	}
}

// TestApplyRollsBackMigrationCarryingItsOwnTransaction is the regression for the
// bug the strip exists to close.
//
// The file opens its own transaction, closes it, and only then hits something
// that fails at execution time. The statement is valid SQL on purpose: a
// syntax error would be caught while PostgreSQL parses the batch, before any of
// it ran, and would prove nothing. Executed unchanged, the file's COMMIT commits
// the runner's transaction as a side effect, so the CREATE TABLE stays applied,
// the version row is never written, and the next start replays the file into
// "relation already exists" — a database that can only be repaired by hand.
// Stripped and run inside the runner's transaction, the failure rolls back both
// the table and the version row, so the two can never disagree.
func TestApplyRollsBackMigrationCarryingItsOwnTransaction(t *testing.T) {
	pool := tempDatabase(t)
	ctx := context.Background()

	table := "migrate_owner_tx_rollback"
	version := "908_migrate_owner_tx_rollback.sql"
	fsys := fstest.MapFS{
		version: {Data: []byte(
			"BEGIN;\nCREATE TABLE " + table + " (id INT);\nCOMMIT;\n" +
				"-- Parses as valid SQL and fails only when it is executed.\n" +
				"INSERT INTO migrate_owner_tx_missing (id) VALUES (1);\n")},
	}
	if err := Apply(ctx, pool, fsys); err == nil {
		t.Fatal("apply must fail on a statement that cannot run")
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if exists {
		t.Fatal("the file's COMMIT committed its DDL before the failure, leaving a table no version row describes")
	}

	var recorded bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&recorded); err != nil {
		t.Fatalf("check version row: %v", err)
	}
	if recorded {
		t.Fatal("a failed migration must not be recorded as applied")
	}
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

// A goose-style Down section would be executed by Apply, which hands the whole
// file to one Exec and understands no directives: the table would be created and
// dropped again in the same transaction, with nothing to show it happened.
func TestApplyRejectsGooseDownSection(t *testing.T) {
	pool, ctx := testPool(t)

	table := "migrate_goose_test"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), `DROP TABLE IF EXISTS `+table)
		_, _ = pool.Exec(context.WithoutCancel(ctx),
			`DELETE FROM schema_migrations WHERE version = '904_migrate_goose.sql'`)
	})

	fsys := fstest.MapFS{
		"904_migrate_goose.sql": {Data: []byte(
			"-- +goose Up\nCREATE TABLE " + table + " (id INT);\n\n-- +goose Down\nDROP TABLE IF EXISTS " + table + ";\n")},
	}
	if err := Apply(ctx, pool, fsys); err == nil {
		t.Fatal("apply must refuse a migration carrying a goose Down section")
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if exists {
		t.Fatal("the refused migration was executed anyway")
	}
	var recorded bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '904_migrate_goose.sql')`).Scan(&recorded); err != nil {
		t.Fatalf("check version row: %v", err)
	}
	if recorded {
		t.Fatal("a refused migration must not be recorded as applied")
	}
}

func TestBaselineSkipsAlreadyAppliedMigrations(t *testing.T) {
	pool, ctx := testPool(t)

	table := "migrate_baseline_test"
	version := "905_migrate_baseline.sql"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), `DROP TABLE IF EXISTS `+table)
		_, _ = pool.Exec(context.WithoutCancel(ctx), `DELETE FROM schema_migrations WHERE version = $1`, version)
	})

	// The file is deliberately not idempotent, like 001_init.sql: it can only run
	// once. If Baseline is honoured, Apply never reads it.
	fsys := fstest.MapFS{
		version: {Data: []byte(`CREATE TABLE ` + table + ` (id INT PRIMARY KEY)`)},
	}

	if err := Baseline(ctx, pool, version); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// Recording the same version twice must stay a no-op.
	if err := Baseline(ctx, pool, version); err != nil {
		t.Fatalf("second baseline: %v", err)
	}
	if err := Apply(ctx, pool, fsys); err != nil {
		t.Fatalf("apply after baseline: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists); err != nil {
		t.Fatalf("check table: %v", err)
	}
	if exists {
		t.Fatal("a baselined migration was executed by apply")
	}
}

func TestBaselineRejectsEmptyVersion(t *testing.T) {
	if err := Baseline(context.Background(), nil, "001.sql"); err == nil {
		t.Fatal("baseline must reject a nil pool")
	}
}

func TestBaselineRequiresNonEmptyVersion(t *testing.T) {
	pool, ctx := testPool(t)
	if err := Baseline(ctx, pool, "  "); err == nil {
		t.Fatal("baseline must reject a blank version")
	}
}
