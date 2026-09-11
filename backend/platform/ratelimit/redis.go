package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
)

// slidingWindowScript 在 Redis 上做一次滑动窗口判定，读-改-写必须原子，
// 所以整段交给 EVAL：Lua 里的命令是连续执行的，多个副本并发打同一个键
// 也不会互相插队把计数读丢。
//
// ZSET 的 score 是请求到达的毫秒时间戳，member 每次唯一，于是「窗口内的请求数」
// 就是 ZCARD。老于窗口的成员先删掉，键也顺手续期——窗口内没有请求时它自己会过期，
// 不会留下长期占位的空 ZSET。
//
// 返回 1 放行、0 超限。
const slidingWindowScript = `
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
if redis.call('ZCARD', key) >= limit then
	return 0
end
redis.call('ZADD', key, now, ARGV[4])
redis.call('PEXPIRE', key, window)
return 1
`

// redisLimiter 是跨副本共享的滑动窗口。
type redisLimiter struct {
	runner cache.ScriptRunner
	window time.Duration
	limit  int
}

func newRedisLimiter(runner cache.ScriptRunner, budget Budget) *redisLimiter {
	return &redisLimiter{runner: runner, window: budget.Window, limit: budget.Limit}
}

func (l *redisLimiter) Allow(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, errEmptyKey
	}
	now := time.Now().UnixMilli()
	res, err := l.runner.Eval(ctx, slidingWindowScript, []string{l.redisKey(key)},
		now, l.window.Milliseconds(), l.limit, uniqueMember(now))
	if err != nil {
		return false, fmt.Errorf("ratelimit eval: %w", err)
	}
	allowed, err := scriptResult(res)
	if err != nil {
		return false, err
	}
	return allowed, nil
}

// redisKey 给键加前缀，免得和别的用途共享同一个 Redis 时撞名。
func (l *redisLimiter) redisKey(key string) string { return "panda:ratelimit:" + key }

// uniqueMember 生成 ZSET 成员。要求只是同一毫秒内不重复：重复的成员会被 ZADD
// 覆盖而不是新增，计数少一，限流就比配置的松一点。带随机后缀跨副本也安全。
func uniqueMember(nowMS int64) string {
	return strconv.FormatInt(nowMS, 10) + "-" + strconv.FormatUint(rand.Uint64(), 36)
}

// scriptResult 读 EVAL 的返回值。go-redis 通常给 int64，但也不保证永远是它，
// 所以两种形态都认，其它形态当作错误而不是「放行」。
func scriptResult(res any) (bool, error) {
	switch v := res.(type) {
	case int64:
		return v == 1, nil
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return false, fmt.Errorf("ratelimit script returned %q: %w", v, err)
		}
		return parsed == 1, nil
	default:
		return false, fmt.Errorf("ratelimit script returned unexpected type %T", res)
	}
}

// fallbackLimiter 在主判定失败时改用本进程判定。Redis 抖动不该让整站
// 要么全放要么全拒：退化成每副本一份额度虽然更松，但那仍是有限的，
// 比没限流更接近预期，也不会因为限流组件自己出问题而挡住正常流量。
type fallbackLimiter struct {
	primary  Limiter
	fallback Limiter
	onError  func(error)
	lastLog  atomic.Int64
}

// withFallback 的 onError 可以为 nil；为 nil 时用内置的节流日志——
// 限流失败往往是一段时间内持续失败，每条都记会把日志淹掉。
func withFallback(primary, fallback Limiter, onError func(error)) *fallbackLimiter {
	return &fallbackLimiter{primary: primary, fallback: fallback, onError: onError}
}

const fallbackLogInterval = 5 * time.Second

func (l *fallbackLimiter) Allow(ctx context.Context, key string) (bool, error) {
	allowed, err := l.primary.Allow(ctx, key)
	if err == nil {
		return allowed, nil
	}
	if l.onError != nil {
		l.onError(err)
	} else {
		l.logThrottled(err)
	}
	return l.fallback.Allow(ctx, key)
}

func (l *fallbackLimiter) logThrottled(err error) {
	now := time.Now().UnixNano()
	last := l.lastLog.Load()
	if now-last < int64(fallbackLogInterval) || !l.lastLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("rate limit fell back to in-process counting", "error", err)
}
