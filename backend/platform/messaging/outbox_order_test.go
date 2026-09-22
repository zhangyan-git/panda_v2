package messaging

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// outboxDDL 是 message_outbox 的最小表结构，抄自 migrations/identity/003。
//
// 测试自己建表：仓库里没有任何一条测试会替它跑迁移，CI 给那块库是空的。抄一份
// 而不是引 migrations 模块（那是另一个 go module）的代价是可能漂移，但漂移不会
// 静默——少一列，下面那条 SQL 自己会报错。
const outboxDDL = `CREATE TABLE IF NOT EXISTS message_outbox (
    event_id TEXT PRIMARY KEY CHECK (char_length(trim(event_id)) > 0),
    event_type TEXT NOT NULL DEFAULT '',
    event_version TEXT NOT NULL DEFAULT '',
    trace_id TEXT NOT NULL DEFAULT '',
    payload BYTEA NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at TIMESTAMPTZ,
    lease_owner TEXT,
    lease_token TEXT,
    lease_until TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// 同一个事务里追加的几条事件必须**按追加顺序**投出去。
//
// 认领与列举都是 `ORDER BY created_at,event_id`，而 created_at 的列默认值是 NOW()
// ——Postgres 里它返回的是**事务开始时刻**，同一个事务写进去的每一条都一样。排序
// 于是落到 event_id 上，那是随机 uuid：谁先谁后由抽签决定。
//
// 这件事真的会发生：membership-service 的店铺码领取在一个事务里连写两条
// （membership.activated 与 membership.campaign.claimed，见 campaign.go）。
// 今天两条的顺序恰好不影响 coupon-service 的判断，但「同一笔业务的两条事件按随机
// 顺序到达」本身就不是下游能依赖的东西。修法在 Append 里：created_at 显式写
// clock_timestamp()。
//
// 条数取 8 不是随手写的：真出事时两条恰好排对的概率是 1/2，八条全对是 1/40320。
// 红得起来，这条测试才在守东西。
func TestPostgresOutboxKeepsInsertionOrderWithinATransaction(t *testing.T) {
	pool := tempOutboxDatabase(t)
	repo := NewPostgreSQL(pool)
	ctx := context.Background()

	const count = 8
	want := make([]string, 0, count)
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		txRepo := NewPostgreSQLWithQuerier(tx)
		for range count {
			id := uuid.NewString()
			want = append(want, id)
			if err := txRepo.Append(ctx, Envelope{
				EventID: id, EventType: "verify.event", Payload: []byte(`{"n":1}`),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("append in one transaction: %v", err)
	}

	// 列举这条路径（Outbox.Pending）与认领那条共用同一个排序，两条都看。
	pending, err := repo.Pending(ctx, count)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	got := make([]string, 0, len(pending))
	for _, event := range pending {
		got = append(got, event.EventID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Pending order = %v\nwant insertion order  = %v", got, want)
	}

	claimed, err := repo.ClaimPending(ctx, count, "verify-order", time.Minute)
	if err != nil {
		t.Fatalf("ClaimPending: %v", err)
	}
	got = got[:0]
	for _, event := range claimed {
		got = append(got, event.EventID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ClaimPending order = %v\nwant insertion order       = %v", got, want)
	}

	// 认领会把 attempts 加一，而这个值必须真的传得出来：relay 的退避是拿它算的，
	// 传不出来每条事件就永远按「第一次失败」退避。
	for i, event := range claimed {
		if event.Attempts != 1 {
			t.Fatalf("claimed[%d].Attempts = %d, want 1", i, event.Attempts)
		}
	}
}

// tempOutboxDatabase 给这个测试一块自己的库，用完丢掉——与 database/migrate 里
// 那个 tempDatabase 同一个写法、同一个理由。
//
// 不能直接用 TEST_DATABASE_URL 指向的那个库：本机它是身份库，而 user-service 的
// outbox relay 正跑在上面，认领条件与这里完全一样——那几条测试事件会先被它抢走
// （lease 挂上），这里再认领就是 0 行，测试随机地红。CI 里没人抢，但测试不该只在
// CI 里成立。
func tempOutboxDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping the PostgreSQL integration test")
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

	name := "messaging_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// CREATE DATABASE 不能跑在事务里，而池上的 Exec 是自动提交的，所以这里可以。
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
		// 先关再删：还有会话连着的时候 DROP DATABASE 会拒绝。
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
		admin.Close()
	})

	if _, err := pool.Exec(ctx, outboxDDL); err != nil {
		t.Fatalf("create message_outbox: %v", err)
	}
	return pool
}
