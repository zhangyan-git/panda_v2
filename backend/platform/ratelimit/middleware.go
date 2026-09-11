package ratelimit

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// KeyFunc 从一个请求上取出限流键。键要能区分「谁在打」和「打的哪一类接口」，
// 但不要用请求里的任何可伪造字段——限流键被伪造就等于没有限流。
type KeyFunc func(*http.Request) string

// Middleware 按 Budget 限流：超限返回 429 并带上 Retry-After。
//
// 判定本身失败时放行。限流是一层保护，不该在它自己出问题的时候变成
// 「把所有人挡在外面」；是否要更严格由调用方决定（platform 里 New 出来的
// 限流器自带进程内回退，正常路径上不会走到这里）。
func Middleware(limiter Limiter, budget Budget, key KeyFunc) func(http.Handler) http.Handler {
	retryAfter := int(math.Ceil(budget.Window.Seconds()))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			allowed, err := limiter.Allow(r.Context(), key(r))
			if err != nil || allowed {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false, "errorCode": "TOO_MANY_REQUESTS",
				"errorMessage": http.StatusText(http.StatusTooManyRequests),
			})
		})
	}
}

// ClientIP 从 RemoteAddr 取客户端地址，用于限流键。
//
// 刻意不看 X-Forwarded-For：那是请求头，客户端想写什么就写什么，用它当键
// 等于把限流的开关交到攻击者手里（每次换个值就换一个桶）。
//
// 网关前面有负载均衡时这个函数不够用（所有人都会被算成 LB 那一个地址），
// 那种拓扑用 ProxyTrust.ClientIP，它只在链路确实经过可信代理时才读 XFF。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// 没有端口（不该出现在服务端，但真出现时也不能把整个地址丢掉：
		// 退化成「所有这种请求共用一个桶」，比不限流更安全）。
		return r.RemoteAddr
	}
	return host
}

// maxForwardedHops 限制 XFF 链的解析长度。链有多长完全由上游决定，
// 可信代理背后的客户端也能往里塞任意多段——不设上限就等于把这一小段
// 解析成本交给对方控制。真实的代理层数远低于这个值。
const maxForwardedHops = 20

// ProxyTrust 决定「谁是真的客户端」。默认什么都不信：只有 RemoteAddr 落在
// 显式配置的可信网段内，才会去读 X-Forwarded-For。
//
// 为什么需要它：ClientIP 无视 XFF 在「客户端直连」下是对的，但一旦网关前面
// 有 L4 负载均衡，所有请求的 RemoteAddr 都是 LB 的地址——于是全站共用一个
// 限流桶，副本越多越确定，最后是所有人一起 429。
//
// 信任网段把这两件事分开：链路里确有代理时才读代理写的头，且只读到第一个
// 不可信的跳为止。客户端伪造的 XFF 在可信代理那里被追加在真实地址的**左边**，
// 从右往左走根本走不到，所以伪造依旧无效。
//
// 只认 X-Forwarded-For；X-Real-IP 之类的单值头不参与——多一跳代理就会互相覆盖，
// 值本身没有可验证的来源。
type ProxyTrust struct {
	nets []*net.IPNet
}

// NewProxyTrust 解析可信代理网段。空列表返回一个什么都不信的 ProxyTrust，
// 也就是 ClientIP 原来的行为——没配就是不变，安全性质不会因为升级而松动。
func NewProxyTrust(cidrs []string) (*ProxyTrust, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		cidr := strings.TrimSpace(raw)
		if cidr == "" {
			continue
		}
		// 也接受裸地址（10.0.0.1），按单主机处理。漏写掩码是这里最容易犯的错，
		// 而且错得隐蔽：那台代理会被判成不可信，限流悄悄退回全站一个桶。
		if !strings.Contains(cidr, "/") {
			ip := net.ParseIP(strings.Trim(cidr, "[]"))
			if ip == nil {
				return nil, fmt.Errorf("trusted proxy %q is neither an IP nor a CIDR", raw)
			}
			if ip.To4() != nil {
				cidr = ip.String() + "/32"
			} else {
				cidr = ip.String() + "/128"
			}
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", raw, err)
		}
		nets = append(nets, network)
	}
	return &ProxyTrust{nets: nets}, nil
}

// ClientIP 返回限流键要用的客户端地址。nil 接收者等同于「没有可信代理」，
// 这样没配的地方可以直接用，不必到处判空。
func (p *ProxyTrust) ClientIP(r *http.Request) string {
	peer := ClientIP(r)
	if p == nil || len(p.nets) == 0 {
		return peer
	}
	parsed := parseIP(peer)
	if parsed == nil || !p.trusted(parsed) {
		// 直连，或对端不在可信网段：此时 XFF 是客户端自己写的，一个字节都不信。
		return peer
	}

	chain := forwardedFor(r)
	// 从右往左走：右边是离本机最近的一跳。跳过可信代理，第一个不可信的就是
	// 真实客户端。往左超出预算还不确定，就退回 peer——那会退化成「按代理地址
	// 计数」这个共享桶，比信任一个验不过来的头安全。
	limit := max(0, len(chain)-1-maxForwardedHops)
	for i := len(chain) - 1; i >= limit; i-- {
		ip := parseIP(chain[i])
		if ip == nil {
			continue
		}
		if !p.trusted(ip) {
			return ip.String()
		}
	}
	return peer
}

func (p *ProxyTrust) trusted(ip net.IP) bool {
	for _, n := range p.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedFor 把一个请求上所有 X-Forwarded-For 头按出现顺序拼成一条链。
// HTTP 允许同一个头出现多次，语义上等同于用逗号连接，只看第一个会丢掉
// 前一段——链断在哪里，判定就会从哪里开始出错。
//
// 只在调用方已经确认对端可信之后才该读它。
func forwardedFor(r *http.Request) []string {
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return nil
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
	}
	return parts
}

// parseIP 解析链上的一跳。真实部署里的写法并不统一：裸地址、带端口、
// IPv6 带方括号都有，都要认得；认不出的一律返回 nil 由调用方跳过，
// 不能因为一个畸形项就把整条链当成不存在。
func parseIP(value string) net.IP {
	if ip := net.ParseIP(value); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip
		}
	}
	// 带方括号但没有端口：[::1]
	return net.ParseIP(strings.Trim(value, "[]"))
}
