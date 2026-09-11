package database

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// withAppName 给 DSN 加一个 application_name，好让服务端能把这些连接从
// 共享的 dev 栈里单独数出来。DSN 可能已经带了查询串，所以分隔符要判一下。
func withAppName(rawURL, name string) string {
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	return rawURL + separator + "application_name=" + name
}

func TestNewWithoutURLReturnsNoop(t *testing.T) {
	pool, err := New(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.(Noop); !ok {
		t.Fatalf("pool type = %T, want Noop", pool)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool.Close()
}

const testDSN = "postgres://user:pw@localhost:5432/panda?sslmode=disable"

// 没配任何东西时必须落到固定默认值，而不是 pgx 那个跟 CPU 走的 max(4, NumCPU)。
// 连接数是容量规划的一部分：同一份配置不该在核多的机器上悄悄多要几倍连接。
func TestPoolMaxConnsDefaultsToAPinnedValue(t *testing.T) {
	t.Setenv("DB_POOL_MAX_CONNS", "")
	got, err := poolMaxConns(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultPoolMaxConns {
		t.Fatalf("max conns = %d, want %d", got, defaultPoolMaxConns)
	}
}

func TestPoolMaxConnsEnvOverridesDSN(t *testing.T) {
	t.Setenv("DB_POOL_MAX_CONNS", "3")
	got, err := poolMaxConns(testDSN + "&pool_max_conns=25")
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("max conns = %d, want 3 from the environment", got)
	}
}

// DSN 里写下的也是运维明确表达的意图，不该被默认值盖掉。
func TestPoolMaxConnsFromDSN(t *testing.T) {
	t.Setenv("DB_POOL_MAX_CONNS", "")
	got, err := poolMaxConns(testDSN + "&pool_max_conns=25")
	if err != nil {
		t.Fatal(err)
	}
	if got != 25 {
		t.Fatalf("max conns = %d, want 25 from the DSN", got)
	}
}

// 配坏了要报错，不能静默退回默认值：带着一个没人预期的连接数跑着，
// 只会在压力上来的时候才暴露。
func TestPoolMaxConnsRejectsInvalidEnv(t *testing.T) {
	for _, value := range []string{"0", "-5", "many", "  "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DB_POOL_MAX_CONNS", value)
			got, err := poolMaxConns(testDSN)
			if value == "  " {
				// 只有空白等于没配，走默认值。
				if err != nil || got != defaultPoolMaxConns {
					t.Fatalf("blank value = (%d, %v), want the default", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("poolMaxConns(%q) = %d, want an error", value, got)
			}
		})
	}
}

// DSN 里写坏了不报错——pgx 自己会在 ParseConfig 上拒绝，这里只要不崩、
// 退回默认值即可。
func TestPoolMaxConnsIgnoresGarbageInDSN(t *testing.T) {
	t.Setenv("DB_POOL_MAX_CONNS", "")
	for _, dsn := range []string{testDSN + "&pool_max_conns=nope", testDSN + "&pool_max_conns=0"} {
		got, err := poolMaxConns(dsn)
		if err != nil {
			t.Fatalf("poolMaxConns(%q): %v", dsn, err)
		}
		if got != defaultPoolMaxConns {
			t.Fatalf("max conns = %d, want the default %d", got, defaultPoolMaxConns)
		}
	}
}

// 上限必须真的落到 pgx 的配置上，而不只是算出来一个数。
func TestNewAppliesPoolLimit(t *testing.T) {
	t.Setenv("DB_POOL_MAX_CONNS", "7")
	pool, err := New(context.Background(), testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pgx, ok := pool.(*PGXPool)
	if !ok {
		t.Fatalf("pool type = %T, want *PGXPool", pool)
	}
	if got := pgx.Pool().Config().MaxConns; got != 7 {
		t.Fatalf("pool MaxConns = %d, want 7", got)
	}
	// 上限不该拖着一个空闲连接池：pgxpool 默认延迟建连，没有查询就不占连接。
	if got := pgx.Pool().Stat().TotalConns(); got != 0 {
		t.Fatalf("a freshly built pool holds %d connections, want 0", got)
	}
}

// 上限要在并发压力下真的成立，而不只是配置里的一个数。
//
// 这条是「副本翻倍 → too many clients → 三个服务一起挂」的直接对照：并发的
// 请求必须是排队等连接，而不是各自开一条新的。服务端数一遍才作数——只看
// 客户端自己的 Stat() 会漏掉「配置写对了但连接照样泄漏」这种情况。
func TestPoolLimitHoldsUnderConcurrency(t *testing.T) {
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping the PostgreSQL integration test")
	}
	const appName = "panda-pool-limit-verify"
	const limit = 3
	ctx := context.Background()

	t.Setenv("DB_POOL_MAX_CONNS", "3")
	pool, err := New(ctx, withAppName(baseURL, appName))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer pool.Close()
	pgx := pool.(*PGXPool)

	observer, err := New(ctx, withAppName(baseURL, appName+"-observer"))
	if err != nil {
		t.Fatalf("New observer: %v", err)
	}
	defer observer.Close()
	countConnections := func() int {
		t.Helper()
		var count int
		query := `select count(*) from pg_stat_activity where application_name = $1`
		if err := observer.(*PGXPool).Pool().QueryRow(ctx, query, appName).Scan(&count); err != nil {
			t.Fatalf("count connections: %v", err)
		}
		return count
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		maxSeen int
		errs    []error
	)
	stop := make(chan struct{})
	sampler := make(chan struct{})
	go func() {
		defer close(sampler)
		for {
			select {
			case <-stop:
				return
			default:
			}
			seen := countConnections()
			mu.Lock()
			if seen > maxSeen {
				maxSeen = seen
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// 每个查询占住连接 200ms，24 个并发一起上：池子要是不够限流，
	// 服务端会看到 24 条连接。
	for range 24 {
		wg.Go(func() {
			var one int
			if err := pgx.Pool().QueryRow(ctx, "select 1 from pg_sleep(0.2)").Scan(&one); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(stop)
	<-sampler

	mu.Lock()
	defer mu.Unlock()
	if len(errs) > 0 {
		t.Fatalf("%d queries failed while waiting for a connection (they must queue, not fail): %v", len(errs), errs[0])
	}
	if maxSeen > limit {
		t.Fatalf("server saw %d connections, want at most %d", maxSeen, limit)
	}
	// 少于 2 说明根本没并发起来，那么上面的断言什么都没证明。
	if maxSeen < 2 {
		t.Fatalf("server saw at most %d connections; the load never exercised the pool", maxSeen)
	}
	t.Logf("peak server-side connections: %d (limit %d)", maxSeen, limit)
}
