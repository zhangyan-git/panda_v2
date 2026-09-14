package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// memoryLimiter 是进程内令牌桶：每个键一桶，按 Window/Limit 的速率补充，
// 桶容量为 Limit。容量取满额而不是更小，是为了让「窗口内 Limit 次」这个语义
// 和 Redis 滑动窗口一致——否则限流一旦退化，行为会跟着变，排查时会误以为
// 是限流规则本身改了。
//
// 代价要写明：每个副本各限一份，N 个副本的实际额度是 Limit×N。
type memoryLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // 每秒补充的令牌数
	burst   float64
	idleTTL time.Duration
	lastGC  time.Time
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newMemoryLimiter(budget Budget) *memoryLimiter {
	return &memoryLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    float64(budget.Limit) / budget.Window.Seconds(),
		burst:   float64(budget.Limit),
		// 键可能来自公网（每个来源 IP 一个桶），不清理就是一条内存放大的路。
		// 留 10 个窗口的宽限，避免刚停一下的客户端被当成新键。
		idleTTL: max(10*budget.Window, time.Minute),
		now:     time.Now,
	}
}

func (m *memoryLimiter) Allow(_ context.Context, key string) (bool, error) {
	if key == "" {
		return false, errEmptyKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.collect(now)
	bucket, ok := m.buckets[key]
	if !ok {
		bucket = &tokenBucket{tokens: m.burst, last: now}
		m.buckets[key] = bucket
	}
	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens = math.Min(m.burst, bucket.tokens+elapsed*m.rate)
		bucket.last = now
	}
	if bucket.tokens < 1 {
		return false, nil
	}
	bucket.tokens--
	return true, nil
}

// collect 顺手清掉闲置的桶。挂在 Allow 里而不是起一个后台协程：
// 没有请求时也就没有键在增长，不需要另一个随生命周期启停的组件。
func (m *memoryLimiter) collect(now time.Time) {
	if now.Sub(m.lastGC) < m.idleTTL {
		return
	}
	m.lastGC = now
	for key, bucket := range m.buckets {
		if now.Sub(bucket.last) > m.idleTTL {
			delete(m.buckets, key)
		}
	}
}

func (m *memoryLimiter) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}
