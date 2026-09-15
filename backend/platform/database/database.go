package database

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultPoolMaxConns 是每个服务实例对 Postgres 的连接上限。
//
// 定成固定值而不是沿用 pgx 的 max(4, NumCPU)，是为了让这件事算得出来：
// 需要的连接数 = 这个值 × 有库的服务数 × 副本数，直接和 Postgres 的
// max_connections 对账即可。跟着 CPU 走的话，同一份配置在大机器上会悄悄
// 多要几倍的连接，而「副本翻倍」正好是最后一个还没被算进去的因子——
// 撞上 too many clients 时三个服务会一起报错。
const defaultPoolMaxConns = 10

// Pool is the lifecycle contract used by platform components.
type Pool interface {
	Ping(context.Context) error
	Close()
}

// Noop keeps services runnable when DATABASE_URL is not configured.
type Noop struct{}

func (Noop) Ping(context.Context) error { return nil }
func (Noop) Close()                     {}

// New creates a PostgreSQL pgx v5 pool, or a no-op adapter for an empty URL.
func New(ctx context.Context, rawURL string) (Pool, error) {
	if rawURL == "" {
		return Noop{}, nil
	}
	cfg, err := pgxpool.ParseConfig(rawURL)
	if err != nil {
		return nil, err
	}
	maxConns, err := poolMaxConns(rawURL)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	// 下限大于上限时 pgx 会直接拒绝配置。默认上限可能比运维写的 pool_min_conns
	// 还小，这时把上限抬到下限：下限是他们明确要的，上限只是个没被覆盖的默认值。
	if cfg.MinConns > cfg.MaxConns {
		cfg.MaxConns = cfg.MinConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &PGXPool{pool: pool}, nil
}

// poolMaxConns 决定池子上限，优先级从高到低：
// DB_POOL_MAX_CONNS → DSN 里的 pool_max_conns → defaultPoolMaxConns。
//
// 环境变量优先于 DSN，与仓库里其余配置一致；DSN 排在中间是因为它也是运维
// 明确写下的，不该被一个默认值盖掉。
func poolMaxConns(rawURL string) (int32, error) {
	if value := strings.TrimSpace(os.Getenv("DB_POOL_MAX_CONNS")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			// 不静默退回默认值：配错了就是配错了，让服务起不来比让它带着一个
			// 没人预期的连接数跑着好——后者只会在压力上来的时候才暴露。
			return 0, fmt.Errorf("DB_POOL_MAX_CONNS must be a positive integer, got %q", value)
		}
		if parsed > math.MaxInt32 {
			return 0, fmt.Errorf("DB_POOL_MAX_CONNS %d is out of range", parsed)
		}
		return int32(parsed), nil
	}
	if parsed, ok := dsnPoolMaxConns(rawURL); ok {
		return parsed, nil
	}
	return defaultPoolMaxConns, nil
}

// dsnPoolMaxConns 读 DSN 查询串里的 pool_max_conns。读不出来（没写、写坏了）
// 就返回 false 交给上层兜底；写坏的那种 pgx 自己会在 ParseConfig 上报错。
func dsnPoolMaxConns(rawURL string) (int32, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return 0, false
	}
	value := parsed.Query().Get("pool_max_conns")
	if value == "" {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return int32(n), true
}

// PGXPool adapts pgxpool.Pool to the platform lifecycle contract.
type PGXPool struct{ pool *pgxpool.Pool }

// Pool exposes the underlying pgx pool to repositories that need SQL access.
func (p *PGXPool) Pool() *pgxpool.Pool {
	if p == nil {
		return nil
	}
	return p.pool
}

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
