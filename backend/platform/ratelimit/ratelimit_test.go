package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
)

func TestBudgetValidation(t *testing.T) {
	for name, budget := range map[string]Budget{
		"zero limit":     {Limit: 0, Window: time.Minute},
		"negative limit": {Limit: -1, Window: time.Minute},
		"zero window":    {Limit: 1, Window: 0},
	} {
		if _, err := New(cache.Noop{}, budget); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// REDIS_ADDR 为空时 New 必须给出可用的限流器：服务照常起，只是额度变成每副本一份。
func TestNewFallsBackToMemoryWithoutRedis(t *testing.T) {
	limiter, err := New(cache.Noop{}, Budget{Limit: 2, Window: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := limiter.(*memoryLimiter); !ok {
		t.Fatalf("limiter type = %T, want the in-process implementation", limiter)
	}
}

func TestMemoryLimiterConsumesBurstThenRefills(t *testing.T) {
	limiter := newMemoryLimiter(Budget{Limit: 2, Window: time.Minute})
	now := time.Now()
	limiter.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		allowed, err := limiter.Allow(ctx, "ip")
		if err != nil || !allowed {
			t.Fatalf("request %d: allowed=%v err=%v, want allowed", i, allowed, err)
		}
	}
	allowed, err := limiter.Allow(ctx, "ip")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("third request inside the window should have been rejected")
	}

	// 速率 = Limit/Window = 每 30 秒一个令牌，等 30 秒就够再放一个。
	now = now.Add(30 * time.Second)
	allowed, err = limiter.Allow(ctx, "ip")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("request after refill should have been allowed")
	}
}

func TestMemoryLimiterKeepsKeysIndependent(t *testing.T) {
	limiter := newMemoryLimiter(Budget{Limit: 1, Window: time.Minute})
	ctx := context.Background()

	if allowed, _ := limiter.Allow(ctx, "a"); !allowed {
		t.Fatal("first key should be allowed")
	}
	if allowed, _ := limiter.Allow(ctx, "a"); allowed {
		t.Fatal("second request on the same key should be rejected")
	}
	if allowed, _ := limiter.Allow(ctx, "b"); !allowed {
		t.Fatal("a different key must get its own budget")
	}
}

// 键来自公网时桶的个数会持续增长，闲置清理必须有；否则限流自己就是内存放大的入口。
func TestMemoryLimiterDropsIdleKeys(t *testing.T) {
	limiter := newMemoryLimiter(Budget{Limit: 1, Window: time.Minute})
	now := time.Now()
	limiter.now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := limiter.Allow(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if limiter.size() != 1 {
		t.Fatalf("buckets = %d, want 1", limiter.size())
	}
	now = now.Add(limiter.idleTTL + time.Minute)
	if _, err := limiter.Allow(ctx, "fresh"); err != nil {
		t.Fatal(err)
	}
	if limiter.size() != 1 {
		t.Fatalf("buckets = %d, want the idle key to be dropped", limiter.size())
	}
}

func TestMemoryLimiterRejectsEmptyKey(t *testing.T) {
	limiter := newMemoryLimiter(Budget{Limit: 1, Window: time.Minute})
	if _, err := limiter.Allow(context.Background(), ""); !errors.Is(err, errEmptyKey) {
		t.Fatalf("err = %v, want errEmptyKey", err)
	}
}

func TestFallbackLimiterUsesFallbackOnError(t *testing.T) {
	primary := &stubLimiter{err: errors.New("redis down"), allowed: true}
	fallback := newMemoryLimiter(Budget{Limit: 1, Window: time.Minute})
	var reported error
	limiter := withFallback(primary, fallback, func(err error) { reported = err })

	allowed, err := limiter.Allow(context.Background(), "ip")
	if err != nil || !allowed {
		t.Fatalf("allowed=%v err=%v, want the fallback to allow the first request", allowed, err)
	}
	if reported == nil {
		t.Fatal("the failure should have been reported")
	}
	// 第二次由回退实现判定，超限。
	if allowed, _ := limiter.Allow(context.Background(), "ip"); allowed {
		t.Fatal("the fallback should still enforce its own budget")
	}
}

func TestMiddlewareRejectsOverBudgetWith429(t *testing.T) {
	limiter := newMemoryLimiter(Budget{Limit: 1, Window: time.Minute})
	handler := Middleware(limiter, Budget{Limit: 1, Window: time.Minute}, ClientIP)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request("10.0.0.1:5000"))
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request("10.0.0.1:5001"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", second.Header().Get("Retry-After"))
	}
}

// 伪造 X-Forwarded-For 不能换到新额度：换得到就等于限流形同虚设。
func TestMiddlewareKeyIgnoresForwardedFor(t *testing.T) {
	limiter := newMemoryLimiter(Budget{Limit: 1, Window: time.Minute})
	handler := Middleware(limiter, Budget{Limit: 1, Window: time.Minute}, ClientIP)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	first := request("10.0.0.1:5000")
	handler.ServeHTTP(httptest.NewRecorder(), first)

	forged := request("10.0.0.1:5001")
	forged.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, forged)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: X-Forwarded-For must not grant a fresh budget", rec.Code)
	}
}

func TestMiddlewareFailsOpenWhenOnlyLimiterErrors(t *testing.T) {
	limiter := &stubLimiter{err: errors.New("no redis, no fallback")}
	handler := Middleware(limiter, Budget{Limit: 1, Window: time.Minute}, ClientIP)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request("10.0.0.1:5000"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a broken limiter must not block traffic", rec.Code)
	}
}

// 真 Redis 上的滑动窗口。用 REDIS_TEST_ADDR 显式打开，未配置就跳过——
// 跳过不算通过，阶段 5 的验收会在真实 Redis 上再跑一遍。
func TestRedisSlidingWindow(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("REDIS_TEST_ADDR is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := cache.New(ctx, cache.Options{Addr: addr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	window := 500 * time.Millisecond
	limiter, err := New(client, Budget{Limit: 2, Window: window})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := limiter.(*fallbackLimiter); !ok {
		t.Fatalf("limiter type = %T, want the Redis implementation with a fallback", limiter)
	}
	key := "test-" + time.Now().Format("150405.000000000")

	for i := 0; i < 2; i++ {
		allowed, err := limiter.Allow(ctx, key)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !allowed {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	allowed, err := limiter.Allow(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("third request inside the window should be rejected")
	}

	// 窗口滑过去之后额度自己回来，不需要显式重置。
	time.Sleep(window + 200*time.Millisecond)
	allowed, err = limiter.Allow(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("request after the window slid past should be allowed")
	}

	// 清理：别把测试键留在共享的 dev Redis 里。
	if runner, ok := client.(cache.ScriptRunner); ok {
		if _, err := runner.Eval(ctx, "return redis.call('DEL', KEYS[1])", []string{"panda:ratelimit:" + key}); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}
}

type stubLimiter struct {
	allowed bool
	err     error
}

func (s *stubLimiter) Allow(context.Context, string) (bool, error) { return s.allowed, s.err }

func request(remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	r.RemoteAddr = remoteAddr
	return r
}
