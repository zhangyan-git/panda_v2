// Command panda-migrate applies this repository's migration sets.
//
// Usage:
//
//	panda-migrate -database URL apply <identity|merchant|coupon|coffee_machine|order|legacy>
//	panda-migrate -database URL adopt <identity|merchant|coupon|coffee_machine|order|legacy> THROUGH
//
// apply runs every migration in the set that the database has not recorded yet,
// and is safe to re-run. Services do the same thing on start when
// DB_MIGRATE_ON_START is enabled; this command exists for the steps that must
// stay an explicit operator decision.
//
// adopt is one of those steps. A database built before the split already has the
// schema the set describes — for the part of its history that predates the
// split — but has no rows in schema_migrations naming those files, and the early
// migrations use bare CREATE TABLE that cannot be replayed. adopt records every
// version up to and including THROUGH as applied without running it, then runs
// whatever follows. It is the only way to bring a pre-split database (or, for the
// identity set, the freshly created merchant database) under the runner.
//
// THROUGH must be a file name from the set, e.g.
//
//	panda-migrate -database "$USER_DATABASE_URL" adopt identity 003_message_outbox_inbox.sql
//
// -baseline-only stops after recording the baseline and applies nothing, for the
// case where what follows THROUGH must not run yet. The split has exactly one
// such step: identity/004_split_cleanup.sql drops the merchant tables, so it may
// only run once merchant-service is confirmed to be serving from its own
// database. The operator records the baseline ahead of the switchover, and runs
// `apply identity` afterwards. Recorded versions do not expire, so splitting
// adopt in two is the same operation with a checkpoint in the middle.
//
// -database is required and is deliberately not read from the environment: this
// command runs DDL, and there is no safe default for which database that hits.
// The URL a service will actually use is USER_DATABASE_URL /
// MERCHANT_DATABASE_URL, falling back to DATABASE_URL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/platform/database/migrate"
	"github.com/panda-dev/panda-v2/migrations"
)

const connectTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "panda-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	database := flag.String("database", "", "PostgreSQL URL to migrate (required)")
	baselineOnly := flag.Bool("baseline-only", false, "with adopt: record the baseline and apply nothing")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if *database == "" {
		flag.Usage()
		return errors.New("-database is required")
	}
	if len(args) < 1 || len(args) > 3 {
		flag.Usage()
		return errors.New("want: <apply|adopt> <set> [THROUGH]")
	}
	command, setName := args[0], args[1]
	set, err := setByName(setName)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, *database)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	// The advisory lock migrate takes serialises concurrent runs, so there is no
	// bound on how long applying a set may take.
	ctx = context.WithoutCancel(ctx)
	switch command {
	case "apply":
		if len(args) != 2 {
			flag.Usage()
			return errors.New("apply takes no THROUGH argument")
		}
		return apply(ctx, pool, set)
	case "adopt":
		if len(args) != 3 {
			flag.Usage()
			return errors.New("adopt needs the version it adopts through, e.g. 003_message_outbox_inbox.sql")
		}
		return adopt(ctx, pool, set, args[2], *baselineOnly)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q, want apply or adopt", command)
	}
}

func apply(ctx context.Context, pool *pgxpool.Pool, set fs.FS) error {
	return migrate.Apply(ctx, pool, set)
}

func adopt(ctx context.Context, pool *pgxpool.Pool, set fs.FS, through string, baselineOnly bool) error {
	versions, err := migrations.Versions(set)
	if err != nil {
		return err
	}
	index := -1
	for i, version := range versions {
		if version == through {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("%q is not a migration in this set (have %v)", through, versions)
	}
	// Baseline everything the database already satisfies, then let Apply run the
	// remainder — the part of the set that only a post-split database reaches.
	if err := migrate.Baseline(ctx, pool, versions[:index+1]...); err != nil {
		return err
	}
	if baselineOnly {
		return nil
	}
	return migrate.Apply(ctx, pool, set)
}

func setByName(name string) (fs.FS, error) {
	switch name {
	case "identity":
		return migrations.Identity, nil
	case "merchant":
		return migrations.Merchant, nil
	case "coupon":
		return migrations.Coupon, nil
	case "coffee_machine":
		return migrations.CoffeeMachine, nil
	case "order":
		return migrations.Order, nil
	case "legacy":
		return migrations.Legacy, nil
	default:
		return nil, fmt.Errorf("unknown set %q, want identity, merchant, coupon, coffee_machine, order, or legacy", name)
	}
}

func usage() {
	fmt.Fprint(flag.CommandLine.Output(), `panda-migrate applies the repository's migration sets.

  panda-migrate -database URL apply <identity|merchant|coupon|coffee_machine|order|legacy>
  panda-migrate -database URL adopt <identity|merchant|coupon|coffee_machine|order|legacy> THROUGH
  panda-migrate -database URL -baseline-only adopt <identity|merchant|coupon|coffee_machine|order|legacy> THROUGH

apply          run every migration the database has not recorded yet (safe to re-run)
adopt          record everything up to and including THROUGH as applied, then run the rest;
               for a database built before the identity/merchant split
-baseline-only stop after recording the baseline, applying nothing; run apply later
`)
}
