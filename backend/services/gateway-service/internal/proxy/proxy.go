package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	defaultRequestTimeout = 10 * time.Second
	// defaultUploadTimeout 是上传路径的预算。它比其余路由大一个量级，因为它覆盖
	// 的是「客户端上行 + 上游写进 OSS」整段，而不只是一次服务端处理：10MB 走
	// 1Mbps 上行约 80 秒，给 60 秒会稳定产出 504。
	defaultUploadTimeout = 120 * time.Second
)

// Config contains the upstream addresses used by the compatibility facade.
//
// The gateway holds no service token: internal calls moved to gRPC, where the
// caller authenticates itself with metadata, so the facade no longer vouches for
// anything it forwards.
type Config struct {
	MerchantServiceURL string
	UserServiceURL     string
	// CouponServiceURL 可留空：优惠券服务尚未上线时，/v1/admin/coupons 保持
	// 404，与「没有这个上游」的语义一致。填了才注册该路由。
	CouponServiceURL string
	// CoffeeMachineServiceURL 同上：可留空，留空时 /v1/admin/coffee-machines 保持
	// 404。设备域是后加的服务，不填不该让网关起不来。
	CoffeeMachineServiceURL string
	// OrderServiceURL 同上：可留空，留空时订单域的四条路径都保持 404（订单域是后加的
	// 服务）。注意它接的是订单与售后两个资源、两端各一棵：/v1/admin/orders、
	// /v1/admin/after-sales、/v1/miniapp/orders、/v1/miniapp/after-sales，后两个落在
	// user-service 的 /v1/miniapp 前缀里——顺序和「留空时不许落到 user-service」
	// 这两件事都在 NewHandler 的那条 case 上，见那里的注释。
	OrderServiceURL string
	// PaymentServiceURL 同上：可留空，留空时 /v1/payments 保持 404。
	//
	// 支付域与上面几个不一样的地方是：它这条路径**必须**能从公网到达。渠道的回调是从公网
	// 打进来的（它签得了名，但进不了内网），所以这一条不是「给前端用的转发」，而是支付域
	// 唯一的对外入口。发起支付走的是另一条路（order-service 同步 gRPC 调支付域），过不了
	// 网关，也不需要过。
	PaymentServiceURL string
	// AccountServiceURL 同上：可留空，留空时资产账户域的路径保持 404（资产账户域是
	// 最后加的服务）。它接的是福卡与咖啡豆两棵树：/v1/admin/{fortune-cards,coffee-beans}
	// 与 /v1/miniapp/{fortune-cards,coffee-beans}——后两条落在 user-service 的
	// /v1/miniapp 前缀里，顺序见 NewHandler 的那条 case。
	AccountServiceURL string
	RequestTimeout    time.Duration
	// UploadTimeout replaces RequestTimeout for the upload path only, so one slow
	// route does not buy every other route a two-minute hang.
	UploadTimeout time.Duration
	HTTPClient    *http.Client
}

// NewHandler returns a thin HTTP facade for browser API paths.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.RequestTimeout < 0 {
		return nil, fmt.Errorf("request timeout must not be negative")
	}
	if cfg.UploadTimeout == 0 {
		cfg.UploadTimeout = defaultUploadTimeout
	}
	if cfg.UploadTimeout < 0 {
		return nil, fmt.Errorf("upload timeout must not be negative")
	}
	merchant, err := newProxy(cfg.MerchantServiceURL, cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("merchant service URL: %w", err)
	}
	user, err := newProxy(cfg.UserServiceURL, cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("user service URL: %w", err)
	}
	// 只有配置了上游才构造，未配置时 coupons 走 default 分支的 404。
	var coupon http.Handler
	if strings.TrimSpace(cfg.CouponServiceURL) != "" {
		coupon, err = newProxy(cfg.CouponServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("coupon service URL: %w", err)
		}
	}
	// 同上，设备域也是一个后加的服务。
	var coffeeMachine http.Handler
	if strings.TrimSpace(cfg.CoffeeMachineServiceURL) != "" {
		coffeeMachine, err = newProxy(cfg.CoffeeMachineServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("coffee machine service URL: %w", err)
		}
	}
	// 订单域同样后加。
	var order http.Handler
	if strings.TrimSpace(cfg.OrderServiceURL) != "" {
		order, err = newProxy(cfg.OrderServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("order service URL: %w", err)
		}
	}
	// 支付域同样后加。它挂在 /v1/payments 这个**独立前缀**上，与订单域刻意分开：渠道回调
	// 不该长在小程序的路径树下（那个前缀已经整个转给订单域与 user-service 了）。
	var payment http.Handler
	if strings.TrimSpace(cfg.PaymentServiceURL) != "" {
		payment, err = newProxy(cfg.PaymentServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("payment service URL: %w", err)
		}
	}
	// 资产账户域（福卡 + 咖啡豆两棵树）同样后加。它接的两条 /v1/miniapp/* 都落在
	// user-service 的 /v1/miniapp 前缀里，所以构造完之后还要看下面那条 case 的位置。
	var account http.Handler
	if strings.TrimSpace(cfg.AccountServiceURL) != "" {
		account, err = newProxy(cfg.AccountServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("account service URL: %w", err)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := normalizePath(r.URL.Path)
		r.URL.Path = path
		// RawPath is intentionally retained (after the same /api removal) so
		// encoded path segments are not decoded and re-escaped differently.
		r.URL.RawPath = normalizeRawPath(r.URL.RawPath)
		var upstream http.Handler
		timeout := cfg.RequestTimeout
		switch {
		// 资产账户域的两棵树的四条路径：福卡与咖啡豆。它们必须排在下面那条
		// /v1/miniapp → user-service 之前，理由与订单域一样：/v1/miniapp/fortune-cards
		// 与 /v1/miniapp/coffee-beans 都落在那个前缀里，写在它后面就永远轮不到，
		// 小程序拿到的会是一句「用户服务没有这个接口」。
		//
		// 咖啡豆那两条是后补的，补之前它们分别落在这里的两处坏地方：/v1/admin/coffee-beans
		// 落到 default 是网关自己的 404，/v1/miniapp/coffee-beans 被转给 user-service。
		// 两者在界面上都是「接口不存在」，而真正的原因是**网关不认识这条路径**——
		// account-service 里那几条路由一直是好的。
		//
		// 两条后台路径也与 user-service 的 /v1/admin/* 都不冲突，放同一条 case 只是因为
		// 它们属于同一个上游。写前缀而不是写整条路径：一棵树上有多条子路径
		// （/coffee-beans/entries、/coffee-beans/{userId}/adjustments），逐个列举漏一个
		// 就是一个静默的 404。
		//
		// 与 order/coupon 那几条一样，这里**不把 account == nil 合进 case 条件**：
		// 没配上游时落到 /v1/miniapp，同样会被转给 user-service，而真正的原因是账户
		// 服务没接上。写在这里显式 404，客户端才不会把「账户域没部署」读成「用户服务
		// 没这个接口」。
		case hasPathPrefix(path, "/v1/admin/fortune-cards"),
			hasPathPrefix(path, "/v1/admin/coffee-beans"),
			hasPathPrefix(path, "/v1/miniapp/fortune-cards"),
			hasPathPrefix(path, "/v1/miniapp/coffee-beans"):
			if account == nil {
				http.NotFound(w, r)
				return
			}
			upstream = account
		// 订单域的四条路径必须先于 user-service 那一条：/v1/miniapp/orders 与
		// /v1/miniapp/after-sales 同时落在「/v1/miniapp →user-service」这个前缀里，
		// 而 Go 的 switch 取第一个成立的 case，写在它后面就永远轮不到。放到前面的
		// 代价只是多几次前缀比较。
		//
		// 售后是订单域的第二棵树（订单与售后各自一棵），所以四个前缀都写出来，而不是
		// 只写一个 /v1/ 的宽前缀：后台与小程序是两棵不同的端树，订单与售后是两个不同的
		// 资源，将来任何一端或任何一个挪走，都不该把其余的带走。
		//
		// 漏掉售后那两条的后果不是 404 而是一句更难查的话：/v1/admin/after-sales 落到
		// default 直接 404，/v1/miniapp/after-sales 落到下面的 /v1/miniapp 被转给
		// user-service，客户端拿到的 404 看着像「用户服务没有这个接口」。
		//
		// 这里刻意不把 order == nil 合进 case 条件：那样没配上游时这些路径会落到
		// 下面的 /v1/miniapp，被转给 user-service，客户端拿到的 404 看着像「用户
		// 服务没有这个接口」。真正的原因是订单服务没接上，就该在这里 404——与优惠券、
		// 设备域「没配上游就 404」是同一条约定。
		case hasPathPrefix(path, "/v1/admin/orders"),
			hasPathPrefix(path, "/v1/admin/after-sales"),
			hasPathPrefix(path, "/v1/miniapp/orders"),
			hasPathPrefix(path, "/v1/miniapp/after-sales"):
			if order == nil {
				http.NotFound(w, r)
				return
			}
			upstream = order
		case isNestedMerchantUsersPath(path),
			// 小程序用户管理。单独一条，是因为 /v1/admin/users 的前缀匹配
			// （下一行）管不到它——hasPathPrefix 按路径段比较，不按字符串前缀。
			hasPathPrefix(path, "/v1/admin/miniapp-users"),
			// 操作日志。身份库是这张表的归属方（审计消费者就住在 user-service），
			// 所以查询也走它，不另起上游。
			hasPathPrefix(path, "/v1/admin/operation-logs"),
			hasPathPrefix(path, "/v1/admin/merchant-users"),
			hasPathPrefix(path, "/v1/admin/roles"),
			hasPathPrefix(path, "/v1/admin/permissions"),
			hasPathPrefix(path, "/v1/admin/menus"),
			hasPathPrefix(path, "/v1/admin/accounts"),
			hasPathPrefix(path, "/v1/admin/auth"),
			hasPathPrefix(path, "/v1/admin/users"),
			hasPathPrefix(path, "/v1/merchant/auth"),
			hasPathPrefix(path, "/v1/merchant/users"),
			hasPathPrefix(path, "/v1/miniapp"):
			upstream = user
		// The upload path goes to the same upstream as the rest of the admin API but
		// gets its own, larger budget. Raising RequestTimeout instead would hold a
		// gateway goroutine and an upstream connection for two minutes on ANY stuck
		// merchant route, and would quietly change the latency contract the other
		// routes are pinned to.
		case hasPathPrefix(path, "/v1/admin/uploads"):
			upstream, timeout = merchant, cfg.UploadTimeout
		case hasPathPrefix(path, "/v1/admin/merchants"),
			hasPathPrefix(path, "/v1/admin/brands"),
			hasPathPrefix(path, "/v1/admin/stores"):
			upstream = merchant
		case coupon != nil && hasPathPrefix(path, "/v1/admin/coupons"):
			upstream = coupon
		// 设备域（厂商 / 咖啡机设备 / 饮品）。它和上面几条都没有共同前缀，落到
		// default 就是 404，而 404 在页面上表现为「接口不存在」，看着像后端没部署。
		case coffeeMachine != nil && hasPathPrefix(path, "/v1/admin/coffee-machines"):
			upstream = coffeeMachine
		// 支付域：目前只有渠道回调一条（POST /v1/payments/callback/{channelCode}）。
		//
		// 与上面两条不同，这里**不把 payment == nil 合进 case 条件**：没配上游时落到
		// default 也是一句 404，但那样通道号、签名这些都没了上下文，看着像「渠道调错了
		// 地址」。在这里显式 404 与订单、优惠券、设备域是同一条约定。
		//
		// 这条路径**不挂认证是有意的**（渠道的凭据是它自己的签名，验签在支付服务里做），
		// 所以网关这一层就是它唯一的把关：限流在 newMux 那条链上，别把它绕过去。
		case hasPathPrefix(path, "/v1/payments"):
			if payment == nil {
				http.NotFound(w, r)
				return
			}
			upstream = payment
		default:
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		upstream.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func newProxy(rawURL string, client *http.Client) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.Contains(rawURL, "#") {
		return nil, fmt.Errorf("must be an absolute HTTP URL without credentials, query, or fragment")
	}
	if target.Path != "" && target.Path != "/" {
		return nil, fmt.Errorf("upstream base path must be empty or root")
	}
	if client == nil {
		client = http.DefaultClient
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			path, rawPath := req.In.URL.Path, req.In.URL.RawPath
			req.SetURL(target)
			req.Out.URL.Path, req.Out.URL.RawPath = path, rawPath
			req.Out.URL.RawQuery = req.In.URL.RawQuery
			// Strip every identity-bearing header, including the retired service
			// token and the forwarding chain: nothing downstream trusts these any
			// more, and a client must not be able to smuggle one through the facade.
			for header := range req.Out.Header {
				name := strings.ToLower(header)
				if strings.HasPrefix(name, "x-user") || strings.HasPrefix(name, "x-tenant") || name == "x-roles" || name == "x-service-token" || strings.HasPrefix(name, "x-forwarded") {
					delete(req.Out.Header, header)
				}
			}
			setXForwarded(req)
		},
		Transport: client.Transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status := http.StatusBadGateway
			var netErr net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
				status = http.StatusGatewayTimeout
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			code := "BAD_GATEWAY"
			if status == http.StatusGatewayTimeout {
				code = "GATEWAY_TIMEOUT"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false, "errorCode": code, "errorMessage": http.StatusText(status),
			})
		},
	}
	if proxy.Transport == nil {
		proxy.Transport = http.DefaultTransport
	}
	return proxy, nil
}

// setXForwarded 重写转发链，而不是沿用 httputil.ProxyRequest.SetXForwarded。
//
// 后者是**追加**语义：它把入站的 X-Forwarded-For 原样接在新地址前面。客户端
// 随便写一个值，下游看到的链首就是伪造的地址。网关在边缘，本机 RemoteAddr
// 就是真实来源，所以这里整个替换掉；调用前 x-forwarded-* 已被清空。
//
// 如果将来网关前面还有一层可信代理，正确做法是按配置的信任网段取链尾，
// 而不是退回追加——那等于把来源交回给客户端决定。
func setXForwarded(req *httputil.ProxyRequest) {
	clientIP, _, err := net.SplitHostPort(req.In.RemoteAddr)
	if err != nil {
		// 分不出端口就整个丢掉，宁可不给来源，也不给一个可能是伪造的值。
		req.Out.Header.Del("X-Forwarded-For")
	} else {
		req.Out.Header.Set("X-Forwarded-For", clientIP)
	}
	req.Out.Header.Set("X-Forwarded-Host", req.In.Host)
	if req.In.TLS == nil {
		req.Out.Header.Set("X-Forwarded-Proto", "http")
	} else {
		req.Out.Header.Set("X-Forwarded-Proto", "https")
	}
}

func isNestedMerchantUsersPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 6 && parts[0] == "" && parts[1] == "v1" && parts[2] == "admin" && parts[3] == "merchants" && parts[4] != "" && parts[5] == "users"
}
func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
func normalizePath(path string) string {
	if path == "/api" {
		return "/"
	}
	if strings.HasPrefix(path, "/api/") {
		return strings.TrimPrefix(path, "/api")
	}
	return path
}
func normalizeRawPath(path string) string {
	if path == "/api" {
		return "/"
	}
	if strings.HasPrefix(path, "/api/") {
		return strings.TrimPrefix(path, "/api")
	}
	return path
}
