// Package ratelimit 按「键」限流：配了 Redis 就用跨副本共享的滑动窗口，
// 没有 Redis 就退化成进程内令牌桶。
//
// 它只管「这个键此刻该不该放行」，键怎么取由调用方决定——网关用的是
// 客户端 IP 加路由类别，见 services/gateway-service/cmd/gateway/ratelimit.go。
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
)

// errEmptyKey 表示调用方没给出限流键。这里返回错误而不是放行：空键会让所有
// 请求挤进同一个桶，静默放行等于限流整体失效，而且是看不出来的那种失效。
var errEmptyKey = errors.New("ratelimit key must not be empty")

// Budget 是一份限流预算：Window 之内最多 Limit 次。
type Budget struct {
	Limit  int
	Window time.Duration
}

func (b Budget) validate() error {
	if b.Limit <= 0 {
		return fmt.Errorf("ratelimit budget limit must be positive, got %d", b.Limit)
	}
	if b.Window <= 0 {
		return fmt.Errorf("ratelimit budget window must be positive, got %s", b.Window)
	}
	return nil
}

// Limiter 判定某个键此刻是否放行。
type Limiter interface {
	// Allow 返回 false 表示超限。返回 error 表示判定本身失败（例如 Redis 不可达），
	// 调用方必须自己决定这种时候怎么办，不要把它当成「拒绝」。
	Allow(ctx context.Context, key string) (bool, error)
}

// New 按客户端能力挑实现：能跑 Redis 脚本就用共享滑动窗口，额度是全部副本合起来
// 算的；否则用进程内令牌桶，额度会按副本数放大。两种实现「窗口内 Limit 次」的
// 语义一致，所以退化时不需要调用方改代码，只是保护强度下降。
//
// REDIS_ADDR 为空时 cache.New 返回的 Noop 不是 cache.ScriptRunner，正好落到
// 进程内实现——这就是「没配 Redis 也照常跑」的那条路径。
func New(client cache.Client, budget Budget) (Limiter, error) {
	if err := budget.validate(); err != nil {
		return nil, err
	}
	local := newMemoryLimiter(budget)
	runner, ok := client.(cache.ScriptRunner)
	if !ok {
		return local, nil
	}
	return withFallback(newRedisLimiter(runner, budget), local, nil), nil
}
