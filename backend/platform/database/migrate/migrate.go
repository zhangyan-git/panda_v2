// Package migrate applies ordered SQL migration files to PostgreSQL.
//
// The caller supplies the files as an fs.FS rather than the package embedding
// them itself: the same runner serves the legacy single-database set and one set
// per service. Both kinds of caller exist: the services that own a set run Apply
// on start behind DB_MIGRATE_ON_START, and cmd/panda-migrate runs it — with
// Baseline, for the adopt steps — over the sets the migrations module embeds.
package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockID guards the whole migration run. Replicas starting simultaneously would
// otherwise race to create the same tables; the loser crashes on a duplicate.
const lockID int64 = 8070626418431

const createVersionTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version     TEXT PRIMARY KEY,
	applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Apply runs every *.sql file in fsys that has not been applied yet, in
// filename order. It is safe to call on every service start.
func Apply(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	if pool == nil {
		return errors.New("migrate: pool is required")
	}
	files, err := migrationFiles(fsys)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	return withMigrationLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		applied, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}
		for _, name := range files {
			if _, done := applied[name]; done {
				continue
			}
			statements, err := fs.ReadFile(fsys, name)
			if err != nil {
				return fmt.Errorf("migrate: read %s: %w", name, err)
			}
			// Apply hands the whole file to a single Exec and knows nothing about
			// goose directives, so a file carrying a Down section would run it too
			// — creating a table and then dropping it in the same transaction,
			// with no error to show for it. Fail loudly instead.
			if bytes.Contains(statements, []byte("-- +goose Down")) {
				return fmt.Errorf("migrate: apply %s: file contains a %q section and would execute it; remove the section or run it with goose",
					name, "-- +goose Down")
			}
			// Each migration is one transaction: a failure half-way leaves no
			// partial schema and no version row claiming it succeeded.
			//
			// The file's own BEGIN;/COMMIT; is stripped first. Most of the set
			// predates this runner and opens and closes a transaction of its own;
			// pgx sends a no-argument Exec over the simple protocol, so the file's
			// statements run inside the transaction opened below, and its COMMIT
			// then commits that transaction early. The version row would land
			// after it, on its own, and a crash in between leaves the DDL applied
			// with nothing recorded — the next start replays the file and dies on
			// "already exists". Stripping in code rather than editing the files
			// keeps an applied migration byte-identical to what the databases
			// hold; the version row is a name, not a checksum, so this is safe
			// only as long as that stays true.
			//
			// The cost: everything now runs in one transaction, so a migration
			// cannot use a statement that refuses to run inside a transaction
			// block (CREATE INDEX CONCURRENTLY and friends). The note under
			// "上生产的注意：事务里建索引" at the head of
			// migrations/payment/001_payment.sql spells out the workaround.
			// Strip only the lines that are nothing but the statement, so a
			// BEGIN inside a dollar-quoted function body is left alone.
			statements = stripTransactionControl(statements)
			tx, err := conn.Begin(ctx)
			if err != nil {
				return fmt.Errorf("migrate: begin %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, string(statements)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migrate: apply %s: %w", name, err)
			}
			// Belt and braces over the strip: if the file still ended this
			// transaction (a transaction statement written in a shape the strip
			// does not recognise), the DDL may already be committed on its own
			// and writing the version row now would be the very split this
			// package is trying to close. Refuse the file loudly rather than
			// paper over it.
			if status := tx.Conn().PgConn().TxStatus(); status != txStatusInProgress {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migrate: apply %s: the file is no longer inside the migration transaction (server status %q); "+
					"it commits or rolls back on its own, so its DDL and its version row cannot be applied together", name, status)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migrate: record %s: %w", name, err)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("migrate: commit %s: %w", name, err)
			}
			slog.Info("applied migration", "version", name)
		}
		return nil
	})
}

// txStatusInProgress is the ReadyForQuery status a PostgreSQL server reports
// while a transaction is open. pgconn hands out the raw byte without naming the
// values, so the one this package tests against is named here.
const txStatusInProgress = 'T'

// stripTransactionControl removes the lines that are nothing but a BEGIN; or
// COMMIT; statement, which is how the migrations in this repository open and
// close a transaction of their own.
//
// The match is deliberately narrow. It has to leave the BEGIN of a dollar-quoted
// plpgsql body alone (that one carries no semicolon on its own line), and it has
// to leave a statement that merely mentions a transaction — a comment, or a
// string containing the word — alone as well. A transaction statement written in
// any other shape therefore survives the strip; Apply detects that after the
// fact by checking that the transaction is still open, and refuses the file
// rather than recording a version row written outside it.
//
// Matched lines are blanked rather than dropped so that a syntax error reported
// by the server still points at the line the file has.
func stripTransactionControl(sql []byte) []byte {
	lines := bytes.Split(sql, []byte("\n"))
	for i, line := range lines {
		switch strings.ToUpper(strings.TrimSpace(string(line))) {
		case "BEGIN;", "COMMIT;":
			lines[i] = nil
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// Baseline records versions as already applied, without running any DDL.
//
// It exists because this repository's early migrations ran outside any runner:
// schema_migrations stays empty while the schema is fully present, and those
// migrations use bare CREATE TABLE without IF NOT EXISTS. A first Apply would
// therefore replay them and fail on the first statement. Recording what is
// already on the database first turns that first Apply into a no-op for the
// past and a normal incremental run for everything after it.
//
// It performs no verification of its own: only name versions you have confirmed
// are really applied. Re-recording an existing version is a no-op, so calling
// it on every start is harmless.
func Baseline(ctx context.Context, pool *pgxpool.Pool, versions ...string) error {
	if pool == nil {
		return errors.New("migrate: pool is required")
	}
	cleaned := make([]string, 0, len(versions))
	for _, version := range versions {
		version = strings.TrimSpace(version)
		if version == "" {
			return errors.New("migrate: baseline version must not be empty")
		}
		cleaned = append(cleaned, version)
	}
	if len(cleaned) == 0 {
		return nil
	}
	return withMigrationLock(ctx, pool, func(ctx context.Context, conn *pgxpool.Conn) error {
		// ON CONFLICT keeps a migration that really did run through Apply from
		// being re-dated by a baseline call.
		if _, err := conn.Exec(ctx,
			`INSERT INTO schema_migrations (version) SELECT unnest($1::text[]) ON CONFLICT (version) DO NOTHING`,
			cleaned); err != nil {
			return fmt.Errorf("migrate: baseline versions: %w", err)
		}
		slog.Info("baselined migrations", "count", len(cleaned))
		return nil
	})
}

// withMigrationLock runs fn while holding the advisory lock and after ensuring
// the version table exists. Every writer of schema_migrations goes through it so
// two replicas starting together cannot interleave.
func withMigrationLock(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, *pgxpool.Conn) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockID); err != nil {
			slog.Error("migrate: release advisory lock", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, createVersionTable); err != nil {
		return fmt.Errorf("migrate: create version table: %w", err)
	}
	return fn(ctx, conn)
}

func migrationFiles(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migration dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func appliedVersions(ctx context.Context, conn interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (map[string]struct{}, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read applied versions: %w", err)
	}
	defer rows.Close()
	applied := map[string]struct{}{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("migrate: scan version: %w", err)
		}
		applied[version] = struct{}{}
	}
	return applied, rows.Err()
}
