package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewHandlerRoutesAndNormalizesAPIPath(t *testing.T) {
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/brands/7" || r.URL.RawQuery != "q=coffee" {
			t.Errorf("merchant request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("authorization header was not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"7"}}`))
	}))
	defer merchant.Close()

	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/merchant/auth/login", "/v1/miniapp/auth/login", "/v1/admin/miniapp-users",
			"/v1/admin/operation-logs":
		default:
			t.Errorf("user request path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"opaque"}`))
	}))
	defer user.Close()

	h, err := NewHandler(Config{MerchantServiceURL: merchant.URL, UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	t.Run("merchant", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/brands/7?q=coffee", nil)
		req.Header.Set("Authorization", "Bearer test")
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
		}
		body, _ := io.ReadAll(res.Result().Body)
		if string(body) != `{"success":true,"data":{"id":"7"}}` {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("user", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/merchant/auth/login", nil)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
		}
	})

	// 小程序走 user-service 的同一个上游，不需要新的上游配置。
	t.Run("miniapp", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/miniapp/auth/login", nil)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
		}
	})

	// 后台的小程序用户管理。这条单独立用例：它和 /v1/admin/users 只差一个路径段，
	// 而前缀匹配是按段比较的——写成字符串前缀的话它会跟着 /v1/admin/user* 一起
	// 被误判，改错了这里才看得见。
	t.Run("admin miniapp users", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/miniapp-users", nil)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
		}
	})

	// 后台的操作日志。和上面同理：它和 /v1/admin/users 没有共同前缀，落到 default
	// 分支就是 404，而 404 在页面上表现为「接口不存在」，看着像后端没部署。
	t.Run("admin operation logs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/operation-logs", nil)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
		}
	})
}

// 设备域是独立的上游，和上面几条都没有共同前缀。/v1/admin/coffee-machines 落到
// default 就是 404，页面上看着像后端没部署。
//
// 两件事一起测：配了上游要真的转发过去，没配就保持 404（而不是转发到一个空地址
// 或者干脆让网关起不来）。
func TestNewHandlerRoutesCoffeeMachineAdmin(t *testing.T) {
	var gotPath, gotMethod string
	coffee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer coffee.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL:      "http://merchant.test",
		UserServiceURL:          "http://user.test",
		CoffeeMachineServiceURL: coffee.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	for _, path := range []string{
		"/api/v1/admin/coffee-machines/devices",
		"/v1/admin/coffee-machines/devices/123/balance",
		"/v1/admin/coffee-machines/manufacturers",
		"/v1/admin/coffee-machines/drinks/123/status",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath = ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, path, nil))
			if res.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
			}
			if gotPath != normalizePath(path) {
				t.Errorf("upstream path = %q, want %q", gotPath, normalizePath(path))
			}
			if gotMethod != http.MethodPost {
				t.Errorf("upstream method = %q, want POST", gotMethod)
			}
		})
	}

	// 前缀按路径段比较，不是字符串前缀：配了上游也不能让 coffee-machines-extra
	// 被设备域接走。
	gotPath = ""
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/admin/coffee-machines-extra/devices", nil))
	if res.Code != http.StatusNotFound || gotPath != "" {
		t.Fatalf("status/upstream path = %d/%q, want 404 and no upstream call", res.Code, gotPath)
	}

	// 没配上游时保持 404：既有部署里没这个服务，网关不能因此起不来，也不能把
	// 请求转到一个空地址上。
	unset, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: "http://user.test"})
	if err != nil {
		t.Fatalf("NewHandler() without a coffee upstream error = %v", err)
	}
	unsetRes := httptest.NewRecorder()
	unset.ServeHTTP(unsetRes, httptest.NewRequest(http.MethodGet, "/v1/admin/coffee-machines/devices", nil))
	if unsetRes.Code != http.StatusNotFound {
		t.Fatalf("status without upstream = %d, want %d", unsetRes.Code, http.StatusNotFound)
	}
}

// 客户端塞进来的 X-Forwarded-For 必须被丢掉、换成真实的 RemoteAddr。
// 追加语义（httputil 的默认行为）会把伪造值留在链首，下游按「第一个」取来源时
// 拿到的就是它——限流键、审计里的来源都会跟着错。
func TestNewHandlerOverwritesInboundForwardedHeaders(t *testing.T) {
	var gotForwardedFor, gotForwardedProto, gotForwardedHost string
	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		gotForwardedProto = r.Header.Get("X-Forwarded-Proto")
		gotForwardedHost = r.Header.Get("X-Forwarded-Host")
		w.WriteHeader(http.StatusOK)
	}))
	defer user.Close()

	h, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/roles", nil)
	req.RemoteAddr = "192.0.2.10:45678"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.1.1.1")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "evil.example")

	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if gotForwardedFor != "192.0.2.10" {
		t.Fatalf("X-Forwarded-For = %q, want the real client address only", gotForwardedFor)
	}
	if gotForwardedProto != "http" {
		t.Fatalf("X-Forwarded-Proto = %q, want http (the actual inbound scheme)", gotForwardedProto)
	}
	if gotForwardedHost != "example.com" {
		t.Fatalf("X-Forwarded-Host = %q, want the inbound Host", gotForwardedHost)
	}
}

func TestNewHandlerRejectsUnknownPath(t *testing.T) {
	h, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: "http://user.test"})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func TestNewHandlerRequiresAbsoluteUpstreamURLs(t *testing.T) {
	if _, err := NewHandler(Config{MerchantServiceURL: "/merchant", UserServiceURL: "http://user.test"}); err == nil {
		t.Fatal("NewHandler() error = nil, want invalid merchant URL error")
	}
}
