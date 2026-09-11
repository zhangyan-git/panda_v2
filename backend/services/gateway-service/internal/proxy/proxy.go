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
	RequestTimeout     time.Duration
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := normalizePath(r.URL.Path)
		r.URL.Path = path
		// RawPath is intentionally retained (after the same /api removal) so
		// encoded path segments are not decoded and re-escaped differently.
		r.URL.RawPath = normalizeRawPath(r.URL.RawPath)
		var upstream http.Handler
		timeout := cfg.RequestTimeout
		switch {
		case isNestedMerchantUsersPath(path),
			hasPathPrefix(path, "/v1/admin/merchant-users"),
			hasPathPrefix(path, "/v1/admin/roles"),
			hasPathPrefix(path, "/v1/admin/permissions"),
			hasPathPrefix(path, "/v1/admin/menus"),
			hasPathPrefix(path, "/v1/admin/accounts"),
			hasPathPrefix(path, "/v1/admin/auth"),
			hasPathPrefix(path, "/v1/admin/users"),
			hasPathPrefix(path, "/v1/merchant/auth"),
			hasPathPrefix(path, "/v1/merchant/users"):
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
