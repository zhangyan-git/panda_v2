package ingress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
)

// ErrCredentialNotFound：X-API-Key 在库里查不到。
//
// 它由**装配处**从仓储的 ErrAPIKeyNotFound 翻译过来（见 cmd/main.go 的那个闭包）。为什么
// 要翻一次：这一层不该 import 仓储——它今天只被 HTTP 路由用，但「验签中间件」与「表长什么
// 样」是两件事。翻译点放在装配处，是因为那里本来就知道两边。
var ErrCredentialNotFound = errors.New("partner credential not found")

// CredentialLookup 按 X-API-Key 取回密钥行与它所属的合作方。
//
// 一次调用取两行（一条 JOIN），不是两次查询：这两件事**必须来自同一个快照**——分开查时
// 「合作方刚被禁用」与「密钥刚被签发」可能落在两次查询之间，于是判定用的是两个不同时刻的
// 事实。合成一条 SQL 让这个时间差消失。
//
// 查不到时返回 ErrCredentialNotFound；任何其它错误都是基础设施故障，两者在中间件里的处置
// 相同（都拒），但在调用日志里分开写。
type CredentialLookup func(ctx context.Context, apiKey string) (*model.APIKey, *model.PartnerAccount, error)

// Caller 是验签通过之后，这个请求的身份。它进请求上下文，供后面的 handler 与日志使用。
//
// 它**不是**授权凭据：能拿到 Caller 就意味着验签、时间窗、nonce、启停、过期、白名单、限流
// 七道全过了（见 middleware.go）。所以它才敢被放进上下文——一个「可能还没验签」的身份放进
// 上下文，迟早会被某个 handler 当成已验签来用。
type Caller struct {
	PartnerID   string
	PartnerCode string
	APIKeyID    string
	APIKeyMask  string
}

type callerContextKey struct{}

// WithCaller 把身份放进上下文。
func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, caller)
}

// CallerFrom 取回身份。第二个返回值为 false 表示这个请求不是从开放接口那一层进来的。
//
// handler 拿到 false 时应当**拒绝**而不是当成匿名调用：这些路由只挂在开放接口那一棵树上，
// 取不到身份只可能是装配错了（比如有人把 handler 挂到了另一条路由上），而「装配错了」的
// 表现必须是拒绝，不能是放行。
func CallerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerContextKey{}).(Caller)
	return caller, ok
}

// IPAllowList 是合作方密钥上的白名单，已解析。
//
// nil 接收者表示**不限制**（空数组的语义，见 migrations/partner）。这一条与「全拒」的
// 区别是有意的：新签发的密钥默认不限制来源，否则运营不填白名单就调不通，而报出来的是 403
// ——他会先怀疑签名算错了。
type IPAllowList struct{ nets []*net.IPNet }

// ParseIPAllowList 解析库里的 TEXT[]。
//
// 接受 CIDR 与裸地址两种写法（裸 IPv4 按 /32、裸 IPv6 按 /128），与
// ratelimit.NewProxyTrust 的规则一致——同一个仓库里两份「可信网段」的解析不该有两种语法。
//
// **有一条不合法就整份失败**，不跳过那一项：白名单写错一项的后果是那个来源被静默拒掉
// （或更糟，被静默放行），而两种都只在生产上表现为「某台机器突然调不通」。让它在这里报错，
// 装配处能把它变成一次启动失败或一次写入拒绝。
func ParseIPAllowList(entries []string) (*IPAllowList, error) {
	list := &IPAllowList{}
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		cidr := entry
		if !strings.Contains(cidr, "/") {
			ip := net.ParseIP(strings.Trim(cidr, "[]"))
			if ip == nil {
				return nil, fmt.Errorf("ip whitelist entry %q is neither an IP nor a CIDR", raw)
			}
			if ip.To4() != nil {
				cidr = ip.String() + "/32"
			} else {
				cidr = ip.String() + "/128"
			}
		}
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("ip whitelist entry %q: %w", raw, err)
		}
		list.nets = append(list.nets, network)
	}
	if len(list.nets) == 0 {
		// 空列表归一成 nil：nil 接收者的 Allows 直接放行，省掉调用方一个判断。
		return nil, nil
	}
	return list, nil
}

// Allows 判一个地址在不在白名单里。nil 接收者（没配白名单）放行一切。
//
// 地址解析不出来时**拒绝**：resolveClientIP 交给我们的串只有两种来路——RemoteAddr 解析
// 出来的 host，或可信代理写的 XFF 里的某一跳。一个解析不出来的值说明链路里有人在写畸形
// 地址，那种情况下「放行」等于把白名单交给对方决定。
func (l *IPAllowList) Allows(address string) bool {
	if l == nil {
		return true
	}
	ip := parseIP(address)
	if ip == nil {
		return false
	}
	for _, network := range l.nets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// parseIP 认裸地址、带端口的地址与带方括号的 IPv6。规则与 platform/ratelimit 的同名函数
// 一致（那里是包内私有，这里是本包私有）。
func parseIP(value string) net.IP {
	if ip := net.ParseIP(value); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip
		}
	}
	return net.ParseIP(strings.Trim(value, "[]"))
}
