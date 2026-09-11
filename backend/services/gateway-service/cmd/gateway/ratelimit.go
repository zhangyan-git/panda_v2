package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/ratelimit"
)

// 限流放在网关上：这里是全站唯一的入口，拦在边缘才不会让超量请求先穿过
// 代理、再被每个服务各自挡一遍。键用客户端 IP 加路由类别。
//
// 客户端 IP 默认取 RemoteAddr，不看 X-Forwarded-For——那是请求头，客户端
// 想写什么就写什么。网关前面挂负载均衡时（HA 的前提），RemoteAddr 会变成
// LB 的地址，全站共用一个桶，所以那时必须配 TRUSTED_PROXY_CIDRS 告诉网关
// 「哪些地址的话可以信」，仅对来自这些地址的请求解析 XFF。
//
// 代价是网关不再是零依赖的纯标准库模块：它现在引用 platform/cache 与
// platform/ratelimit，后者带来 go-redis。取的是「边缘限流 > 保持零依赖」。
const (
	// defaultRateLimit 是普通接口的额度：每分钟每 IP。
	defaultRateLimitEnv = "RATE_LIMIT_PER_MINUTE"
	// authRateLimit 是认证接口的额度。登录/刷新是撞库最集中的入口，单独收紧。
	authRateLimitEnv = "RATE_LIMIT_AUTH_PER_MINUTE"
	rateLimitEnabled = "RATE_LIMIT_ENABLED"
	// trustedProxyEnv 是可信代理网段，逗号分隔的 CIDR 列表。留空即「什么都不信」，
	// 也就是网关直连客户端时的正确取值——别为了让限流「看起来在工作」而填 *。
	trustedProxyEnv = "TRUSTED_PROXY_CIDRS"
)

const (
	defaultRateLimitPerMinute = 600
	authRateLimitPerMinute    = 30
	rateLimitWindow           = time.Minute
)

// throttle 按请求类别选一份预算。两类各自一个 limiter，共用一个 Redis 客户端
// （也就共用一条连接池），键里再带上类别，互不占用对方额度。
type throttle struct {
	enabled   bool
	defaultMW func(http.Handler) http.Handler
	authMW    func(http.Handler) http.Handler
}

// newThrottle 建限流中间件。REDIS_ADDR 为空时 cache.NewFromEnv 返回 Noop，
// 限流器退化成进程内令牌桶——服务照常起，只是额度按副本数放大，见 platform/ratelimit。
func newThrottle(ctx context.Context) (*throttle, error) {
	if !envBool(rateLimitEnabled, true) {
		log.Print("gateway-service: rate limiting is disabled")
		return &throttle{enabled: false}, nil
	}
	client, err := cache.NewFromEnv(ctx)
	if err != nil {
		return nil, err
	}
	limit, err := envInt(defaultRateLimitEnv, defaultRateLimitPerMinute)
	if err != nil {
		return nil, err
	}
	authLimit, err := envInt(authRateLimitEnv, authRateLimitPerMinute)
	if err != nil {
		return nil, err
	}
	// 配置错误直接起不来。把网段写错（比如漏了掩码又写了个域名）会让判定
	// 悄悄退回「全站一个桶」，那是配额被整片吃掉才发现的故障。
	trustedProxies := envList(trustedProxyEnv)
	trust, err := ratelimit.NewProxyTrust(trustedProxies)
	if err != nil {
		return nil, err
	}
	if len(trustedProxies) == 0 {
		log.Print("gateway-service: no trusted proxies configured; rate limiting keys on the immediate peer address")
	}

	defaultBudget := ratelimit.Budget{Limit: limit, Window: rateLimitWindow}
	authBudget := ratelimit.Budget{Limit: authLimit, Window: rateLimitWindow}
	defaultLimiter, err := ratelimit.New(client, defaultBudget)
	if err != nil {
		return nil, err
	}
	authLimiter, err := ratelimit.New(client, authBudget)
	if err != nil {
		return nil, err
	}
	return &throttle{
		enabled:   true,
		defaultMW: ratelimit.Middleware(defaultLimiter, defaultBudget, keyFor("api", trust)),
		authMW:    ratelimit.Middleware(authLimiter, authBudget, keyFor("auth", trust)),
	}, nil
}

// wrap 把限流套在网关的入口上。两条链都预先构造好，每个请求只是选中其中一条，
// 而不是现建闭包。
func (t *throttle) wrap(next http.Handler) http.Handler {
	if t == nil || !t.enabled {
		return next
	}
	authGuarded := t.authMW(next)
	defaultGuarded := t.defaultMW(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAuthPath(r.URL.Path) {
			authGuarded.ServeHTTP(w, r)
			return
		}
		defaultGuarded.ServeHTTP(w, r)
	})
}

// isAuthPath 认出认证类接口。按后缀判断而不是完整路径：网关已经把 /api
// 前缀剥掉了，但请求可能带尾斜杠，用后缀更经得起这些写法差异。
func isAuthPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return strings.HasSuffix(path, "/auth/login") || strings.HasSuffix(path, "/auth/refresh")
}

// keyFor 把类别写进键里：两类接口各自计数，认证接口的严格额度不会被
// 普通接口的流量提前耗掉。
func keyFor(category string, trust *ratelimit.ProxyTrust) ratelimit.KeyFunc {
	return func(r *http.Request) string {
		return category + "|" + trust.ClientIP(r)
	}
}

// envList 读一个逗号分隔的配置项。空值返回 nil，与「没配」同义。
func envList(name string) []string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func envInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, &configError{name: name, want: "a positive integer"}
	}
	return parsed, nil
}

func envBool(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

type configError struct {
	name string
	want string
}

func (e *configError) Error() string {
	return "gateway-service: " + e.name + " must be " + e.want
}
