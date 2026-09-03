package cache

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// Client is the lifecycle contract used by platform components.
type Client interface {
	Ping(context.Context) error
	Close() error
}

// Noop keeps services runnable when REDIS_ADDR is not configured.
type Noop struct{}

func (Noop) Ping(context.Context) error { return nil }
func (Noop) Close() error               { return nil }

// NewFromEnv creates a Redis client using REDIS_ADDR, REDIS_PASSWORD, and
// REDIS_DB. An empty REDIS_ADDR returns a no-op adapter.
func NewFromEnv(ctx context.Context) (Client, error) {
	value := os.Getenv("REDIS_DB")
	db := 0
	if value != "" {
		var err error
		db, err = strconv.Atoi(value)
		if err != nil {
			return nil, errors.New("invalid REDIS_DB")
		}
	}
	return New(ctx, Options{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       db,
	})
}

type Options struct {
	Addr     string
	Password string
	DB       int
}

// New creates a Redis 7 compatible go-redis v9 client. It does not ping during
// construction; callers can explicitly Ping as part of startup.
func New(_ context.Context, options Options) (Client, error) {
	if options.Addr == "" {
		return Noop{}, nil
	}
	if options.DB < 0 {
		return nil, errors.New("redis database must not be negative")
	}
	return &Redis{client: redis.NewClient(&redis.Options{
		Addr:     options.Addr,
		Password: options.Password,
		DB:       options.DB,
	})}, nil
}

// Redis adapts go-redis to the platform lifecycle contract.
type Redis struct{ client *redis.Client }

func (r *Redis) Ping(ctx context.Context) error {
	if r == nil || r.client == nil {
		return errors.New("redis client is nil")
	}
	return r.client.Ping(ctx).Err()
}

func (r *Redis) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

var _ Client = (*Redis)(nil)
