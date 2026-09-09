package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Config contains the upstream addresses used by the compatibility facade.
type Config struct {
	MerchantServiceURL string
	UserServiceURL     string
}

// NewHandler returns a thin HTTP facade for browser API paths. It preserves
// the request path (apart from an optional browser /api prefix), query,
// headers, status, and response body.
func NewHandler(cfg Config) (http.Handler, error) {
	merchant, err := newProxy(cfg.MerchantServiceURL)
	if err != nil {
		return nil, fmt.Errorf("merchant service URL: %w", err)
	}
	user, err := newProxy(cfg.UserServiceURL)
	if err != nil {
		return nil, fmt.Errorf("user service URL: %w", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := normalizePath(r.URL.Path)
		r.URL.Path = path
		r.URL.RawPath = ""
		switch {
		case hasPathPrefix(path, "/v1/admin/merchants"),
			hasPathPrefix(path, "/v1/admin/brands"),
			hasPathPrefix(path, "/v1/admin/stores"):
			merchant.ServeHTTP(w, r)
		case hasPathPrefix(path, "/v1/admin/accounts"),
			hasPathPrefix(path, "/v1/admin/auth"),
			hasPathPrefix(path, "/v1/admin/users"),
			hasPathPrefix(path, "/v1/merchant/auth"),
			hasPathPrefix(path, "/v1/merchant/users"):
			user.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}), nil
}

func newProxy(rawURL string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("must be an absolute URL")
	}
	return &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			req.SetXForwarded()
		},
	}, nil
}

func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func normalizePath(path string) string {
	if strings.HasPrefix(path, "/api/") {
		return strings.TrimPrefix(path, "/api")
	}
	if path == "/api" {
		return "/"
	}
	return path
}
