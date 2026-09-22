package ingress

import (
	"sync"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/ratelimit"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
)

// DefaultRateLimitPerMinute 是密钥行上没给额度时的兜底。
//
// 库上那一列是 NOT NULL DEFAULT 60 且有 CHECK (> 0)，所以「没给」在数据里不存在；这个常量
// 服务的是**构造 APIKey 结构体的代码**（测试、以及将来某条只读路径），它保证额度不会被零值
// 解释成「不限流」——那是最不该出现的默认值。
//
// 它就是 model 里那一个（签发的写路径要用同一个数把「没填」归一掉），这里再导出一次只是
// 让本包的调用点不必 import model 去拿一个限流语义的值。**不要再抄第二个 60**。
const DefaultRateLimitPerMinute = model.DefaultRateLimitPerMinute

// rateWindow 是限流的窗口。一分钟，与列名 rate_limit_per_minute 逐字对应。
//
// 不做成可配置的：列名已经把单位写死了，让窗口可变会造出「这个字段叫每分钟但实际是每 30 秒」
// 这种只有读代码才知道的偏差，而限流额度是要跟合作方写在协议里的。
const rateWindow = time.Minute

// Limiters 按额度值缓存 ratelimit.Limiter。
//
// # 为什么要缓存
//
// platform/ratelimit 的 New 接受一份**静态** Budget，而本服务的额度是**每把密钥一行**
// （partner_api_keys.rate_limit_per_minute，运营可以给大客户调高）。每来一个请求就 New
// 一次的话：Redis 那条路每次重建一个 limiter（无所谓），而**进程内回退**那条路每次重建一个
// 空的令牌桶——那是「每个请求都从满额开始」，等于没有限流，而且不会有任何报错。缓存把这两条
// 路都钉在「一把密钥一组桶」上。
//
// 键是额度值而不是密钥 id：额度相同的密钥共享一个 limiter 实例，而 limiter 内部本来就是按
// 「传进来的键」分桶的（见 Allow 的调用点），所以共享实例不会让两把密钥共用一个桶。
// 额度取值空间很小（运营会用的就是几十到几万这几档），这个 map 不会长起来。
//
// 并发安全：用一把互斥锁而不是 sync.Map——这里的读写是「每个请求一次」的量级，锁上没有
// 竞争；而 sync.Map 会让人以为这里有热路径。
type Limiters struct {
	mu       sync.Mutex
	client   cache.Client
	byBudget map[int]ratelimit.Limiter
}

// NewLimiters 建一个缓存。client 可以是 cache.Noop（没配 REDIS_ADDR）——那时 ratelimit.New
// 会退化成进程内令牌桶，也就是每个副本各限一份。
//
// 本服务在生产上不会走到那条回退：REDIS_ADDR 为空时 NewNonceStore 直接让进程起不来（见
// nonce.go），而防重放的失败关闭是硬要求。留着这条回退是因为它由 platform 决定，在这里再
// 判一次只会多一处「两边的判断哪天不一致」。
func NewLimiters(client cache.Client) *Limiters {
	return &Limiters{client: client, byBudget: make(map[int]ratelimit.Limiter)}
}

// For 取（或建）这个额度对应的限流器。额度非正时用 DefaultRateLimitPerMinute。
func (l *Limiters) For(perMinute int) (ratelimit.Limiter, error) {
	if perMinute <= 0 {
		perMinute = DefaultRateLimitPerMinute
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if limiter, ok := l.byBudget[perMinute]; ok {
		return limiter, nil
	}
	limiter, err := ratelimit.New(l.client, ratelimit.Budget{Limit: perMinute, Window: rateWindow})
	if err != nil {
		// Budget 的取值在上一行已经保证合法，所以这里只可能是 platform 内部换了实现。
		// 不缓存失败的构造：下一次请求重试一次比缓存一个 nil 好——缓存 nil 会让这一档额度
		// 永久失效，而它的起因可能只是一次瞬时的失败。
		return nil, err
	}
	l.byBudget[perMinute] = limiter
	return limiter, nil
}

// rateLimitKey 拼限流键：**按密钥**，不按来源 IP（与 migrations/partner/001 上那一列的注释
// 同一条理由）。一把密钥发给一家合作方，它们背后可能是一整个机房——按 IP 限会把一家正常
// 公司挡在门外，而按密钥限算的正是「你答应的调用量」。
//
// 前缀 partner: 是为了与别的用途分开：platform 的 redis limiter 还会在这前面再加一层
// panda:ratelimit:，两段拼起来才是最终的 Redis 键。
func rateLimitKey(apiKeyID string) string { return "partner:" + apiKeyID }
