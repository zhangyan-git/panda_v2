package database

import (
	"context"
	"errors"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the lifecycle contract used by platform components.
type Pool interface {
	Ping(context.Context) error
	Close()
}

// Noop keeps services runnable when DATABASE_URL is not configured.
type Noop struct{}

func (Noop) Ping(context.Context) error { return nil }
func (Noop) Close()                     {}

// NewFromEnv creates a PostgreSQL pool when DATABASE_URL is configured.
// An empty URL returns a no-op adapter so local services need no database.
func NewFromEnv(ctx context.Context) (Pool, error) {
	return New(ctx, os.Getenv("DATABASE_URL"))
}

// New creates a PostgreSQL pgx v5 pool, or a no-op adapter for an empty URL.
func New(ctx context.Context, url string) (Pool, error) {
	if url == "" {
		return Noop{}, nil
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &PGXPool{pool: pool}, nil
}

// PGXPool adapts pgxpool.Pool to the platform lifecycle contract.
type PGXPool struct{ pool *pgxpool.Pool }

func (p *PGXPool) Ping(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return errors.New("database pool is nil")
	}
	return p.pool.Ping(ctx)
}

func (p *PGXPool) Close() {
	if p != nil && p.pool != nil {
		p.pool.Close()
	}
}

var _ Pool = (*PGXPool)(nil)
