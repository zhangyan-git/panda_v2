package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsAuthPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/v1/admin/auth/login", true},
		{"/v1/admin/auth/login/", true},
		{"/v1/merchant/auth/login", true},
		{"/v1/admin/auth/refresh", true},
		{"/v1/admin/auth/logout", false},
		{"/v1/admin/users", false},
		{"/v1/admin/auth/login-extra", false},
	} {
		if got := isAuthPath(tc.path); got != tc.want {
			t.Errorf("isAuthPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// 认证接口的严格额度不能被普通接口的流量提前耗掉——两类各记各的键。
func TestKeyForSeparatesCategories(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/login", nil)
	r.RemoteAddr = "10.0.0.7:33333"

	// nil 就是没配 TRUSTED_PROXY_CIDRS 的默认形态：只看 RemoteAddr。
	auth := keyFor("auth", nil)(r)
	api := keyFor("api", nil)(r)
	if auth == api {
		t.Fatalf("both categories produced key %q; they must not share a budget", auth)
	}
	if auth != "auth|10.0.0.7" {
		t.Fatalf("auth key = %q, want auth|10.0.0.7", auth)
	}
}

// 没开限流（或 REDIS_ADDR 为空时的退化路径由 platform 负责）时，网关必须
// 原样放行，不能把请求拦在自己这里。
func TestThrottleDisabledPassesThrough(t *testing.T) {
	throttle := &throttle{enabled: false}
	handler := throttle.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the inner handler's 418", rec.Code)
	}
}

// 用进程内限流器建出来的 throttle 走一遍两类请求：认证类打到自己的额度就 429，
// 普通接口的额度不受影响。
func TestThrottleAppliesSeparateBudgets(t *testing.T) {
	t.Setenv(rateLimitEnabled, "true")
	t.Setenv(defaultRateLimitEnv, "10")
	t.Setenv(authRateLimitEnv, "1")
	t.Setenv("REDIS_ADDR", "")

	throttle, err := newThrottle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handler := throttle.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	login := func() int {
		r := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/login", nil)
		r.RemoteAddr = "10.0.0.7:33333"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := login(); code != http.StatusOK {
		t.Fatalf("first login = %d, want 200", code)
	}
	if code := login(); code != http.StatusTooManyRequests {
		t.Fatalf("second login = %d, want 429", code)
	}

	// 同一个 IP 上的普通接口不受登录额度影响。
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	r.RemoteAddr = "10.0.0.7:33333"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("read request = %d, want 200: it must not share the login budget", rec.Code)
	}
}

func TestNewThrottleRejectsInvalidLimits(t *testing.T) {
	t.Setenv(defaultRateLimitEnv, "0")
	t.Setenv("REDIS_ADDR", "")
	if _, err := newThrottle(context.Background()); err == nil {
		t.Fatal("expected an error for a non-positive limit")
	}
	t.Setenv(defaultRateLimitEnv, "abc")
	if _, err := newThrottle(context.Background()); err == nil {
		t.Fatal("expected an error for a non-numeric limit")
	}
}
