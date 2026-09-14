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

// 订单域的四条路径都要转发到订单服务，尤其是 /v1/miniapp/orders 与
// /v1/miniapp/after-sales——它们同时落在「/v1/miniapp →user-service」那个前缀里，
// 而 switch 取第一个成立的 case。
// 这一条测的就是那个顺序：把订单的 case 挪到 user-service 后面，只有这里会红。
func TestNewHandlerRoutesOrderAheadOfTheMiniappPrefix(t *testing.T) {
	var gotPath string
	order := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer order.Close()

	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = "user:" + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer user.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL: "http://merchant.test",
		UserServiceURL:     user.URL,
		OrderServiceURL:    order.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	for _, path := range []string{
		"/v1/admin/orders",
		"/v1/admin/orders/8f1c/cancel",
		"/v1/miniapp/orders",
		"/api/v1/miniapp/orders/8f1c",
		// 售后是订单域的第二个资源。少了这几条，漏掉售后前缀的改动不会被这里拦住：
		// /v1/admin/after-sales 会变成网关的 404，而 /v1/miniapp/after-sales 会被
		// user-service 用「没有这个接口」答掉。
		"/v1/admin/after-sales",
		"/v1/admin/after-sales/REF202609140001/approve",
		"/v1/miniapp/orders/8f1c/after-sales",
		"/v1/miniapp/after-sales/REF202609140001/cancel",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath = ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
			if res.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
			}
			if gotPath != normalizePath(path) {
				t.Errorf("upstream path = %q, want %q (user-service answered instead of order-service)", gotPath, normalizePath(path))
			}
		})
	}

	// 反方向：同前缀下的其它小程序路径仍然归 user-service。少了这一条，把
	// /v1/miniapp 整段挪给订单服务也能让上面四条全绿。
	gotPath = ""
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/v1/miniapp/auth/login", nil))
	if gotPath != "user:/v1/miniapp/auth/login" {
		t.Fatalf("upstream = %q, want the user-service one", gotPath)
	}

	// 没配上游时保持 404，而不是落到下面的 /v1/miniapp 去让 user-service 回答：
	// 那样客户端拿到的 404 看着像「用户服务没有这个接口」，而真相是订单服务没接上。
	unset, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() without an order upstream error = %v", err)
	}
	unsetRes := httptest.NewRecorder()
	unset.ServeHTTP(unsetRes, httptest.NewRequest(http.MethodGet, "/v1/miniapp/orders", nil))
	if unsetRes.Code != http.StatusNotFound {
		t.Fatalf("status without upstream = %d, want %d", unsetRes.Code, http.StatusNotFound)
	}
	// 售后单独再验一次，而且验的是「谁答的」而不只是状态码：没配订单上游时
	// /v1/miniapp/after-sales 一旦落到下面那条 /v1/miniapp，user-service 会回自己的
	// 404，状态码一样是 404，只比状态码看不出区别。
	gotPath = ""
	unsetAfterSaleRes := httptest.NewRecorder()
	unset.ServeHTTP(unsetAfterSaleRes, httptest.NewRequest(http.MethodPost, "/v1/miniapp/after-sales/REF1/cancel", nil))
	if unsetAfterSaleRes.Code != http.StatusNotFound {
		t.Fatalf("after-sale status without upstream = %d, want %d", unsetAfterSaleRes.Code, http.StatusNotFound)
	}
	if gotPath != "" {
		t.Fatalf("after-sale without an order upstream reached %q, want nobody answered it", gotPath)
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
