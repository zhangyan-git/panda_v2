package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// 福卡账户域也是独立的上游，且它的两条路径分属两棵树：后台那条没有冲突前缀，
// 小程序那条落在「/v1/miniapp →user-service」里——和订单域同一个坑，所以这里
// 也把「谁答的」记下来，只比状态码是看不出区别的（两边都是 404 或 204）。
func TestNewHandlerRoutesFortuneCardsAheadOfTheMiniappPrefix(t *testing.T) {
	var gotPath string
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer account.Close()

	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = "user:" + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer user.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL: "http://merchant.test",
		UserServiceURL:     user.URL,
		AccountServiceURL:  account.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	for _, path := range []string{
		"/v1/miniapp/fortune-cards",
		"/api/v1/miniapp/fortune-cards",
		"/v1/miniapp/fortune-cards/entries",
		"/v1/admin/fortune-cards/entries",
		"/v1/admin/fortune-cards/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		// 咖啡豆那一棵树。后台三条都列出来：网关是按前缀转发的（不分发到方法），
		// 但「前缀写对了、account-service 那边怎么分发」是两件事，这三条正好覆盖
		// account-service 里那条从长到短的注册顺序（entries → {userId}/adjustments
		// → {userId}），哪一条被网关漏掉都会在这里红。
		"/v1/miniapp/coffee-beans",
		"/v1/admin/coffee-beans/entries",
		"/v1/admin/coffee-beans/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		"/v1/admin/coffee-beans/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/adjustments",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath = ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
			if res.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
			}
			if gotPath != normalizePath(path) {
				t.Errorf("upstream path = %q, want %q (user-service answered instead of account-service)", gotPath, normalizePath(path))
			}
		})
	}

	// 反方向：同前缀下的其它小程序路径仍然归 user-service。少了这一条，把
	// /v1/miniapp 整段挪给账户服务也能让上面几条全绿。
	gotPath = ""
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/v1/miniapp/auth/login", nil))
	if gotPath != "user:/v1/miniapp/auth/login" {
		t.Fatalf("upstream = %q, want the user-service one", gotPath)
	}

	// 前缀按路径段比较，不是字符串前缀：fortune-cards-extra 与 fortune-cards 不是
	// 同一个前缀，配了上游也不能被账户服务接走。
	gotPath = ""
	res = httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/admin/fortune-cards-extra/entries", nil))
	if res.Code != http.StatusNotFound || gotPath != "" {
		t.Fatalf("status/upstream path = %d/%q, want 404 and no upstream call", res.Code, gotPath)
	}

	// 没配上游时保持 404，而不是落到 /v1/miniapp 去让 user-service 回答：那样客户端
	// 拿到的 404 看着像「用户服务没有这个接口」，而真相是账户服务没接上。
	unset, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() without an account upstream error = %v", err)
	}
	for _, path := range []string{"/v1/miniapp/fortune-cards", "/v1/admin/fortune-cards/entries"} {
		gotPath = ""
		res := httptest.NewRecorder()
		unset.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s status without upstream = %d, want %d", path, res.Code, http.StatusNotFound)
		}
		if gotPath != "" {
			t.Fatalf("%s without an account upstream reached %q, want nobody answered it", path, gotPath)
		}
	}
}

func TestNewHandlerRoutesLotteryAheadOfTheMiniappPrefix(t *testing.T) {
	var gotPath string
	lottery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer lottery.Close()

	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = "user:" + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer user.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL: "http://merchant.test",
		UserServiceURL:     user.URL,
		LotteryServiceURL:  lottery.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	for _, path := range []string{
		"/v1/miniapp/lottery/campaigns",
		"/api/v1/miniapp/lottery/campaigns",
		"/v1/miniapp/lottery/wins",
		// 后台那几条。抽奖域的路径形状比账户域深一层：期次带 {id}、人工开奖在
		// {id}/draw 上，两条都列出来，免得前缀少写一段时在这里看不出来。
		"/v1/admin/lottery/activations",
		"/v1/admin/lottery/campaigns/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		"/v1/admin/lottery/rounds/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/draw",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath = ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
			if res.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
			}
			if gotPath != normalizePath(path) {
				t.Errorf("upstream path = %q, want %q (user-service answered instead of lottery-service)", gotPath, normalizePath(path))
			}
		})
	}

	// 反方向：同前缀下的其它小程序路径仍然归 user-service。少了这一条，把
	// /v1/miniapp 整段挪给抽奖服务也能让上面几条全绿。
	gotPath = ""
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/v1/miniapp/auth/login", nil))
	if gotPath != "user:/v1/miniapp/auth/login" {
		t.Fatalf("upstream = %q, want the user-service one", gotPath)
	}

	// 前缀按路径段比较，不是字符串前缀：lottery-extra 与 lottery 不是同一个前缀，
	// 配了上游也不能被抽奖服务接走。用后台那条来断言，因为 /v1/miniapp/lottery-extra
	// 会被下面的 /v1/miniapp 接走——那是对的（user-service 就是那个前缀的兜底），
	// 不是这里要证的事；后台这条没人接，落 default 才是 404。
	gotPath = ""
	res = httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/admin/lottery-extra/campaigns", nil))
	if res.Code != http.StatusNotFound || gotPath != "" {
		t.Fatalf("status/upstream path = %d/%q, want 404 and no upstream call", res.Code, gotPath)
	}

	// 上面那条的反面：小程序的同级路径确实由 /v1/miniapp 的兜底接走。这一条不是
	// 「抽奖域漏了」的证明，而是把兜底行为写下来，免得后来的人把 404 和转给
	// user-service 当成同一件事去「修」。
	gotPath = ""
	res = httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/miniapp/lottery-extra/campaigns", nil))
	if gotPath != "user:/v1/miniapp/lottery-extra/campaigns" {
		t.Fatalf("upstream = %q, want the user-service catch-all", gotPath)
	}

	// 没配上游时保持 404，而不是落到 /v1/miniapp 去让 user-service 回答：那样客户端
	// 拿到的 404 看着像「用户服务没有这个接口」，而真相是抽奖服务没接上。
	unset, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() without a lottery upstream error = %v", err)
	}
	for _, path := range []string{"/v1/miniapp/lottery/campaigns", "/v1/admin/lottery/campaigns"} {
		gotPath = ""
		res := httptest.NewRecorder()
		unset.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s status without upstream = %d, want %d", path, res.Code, http.StatusNotFound)
		}
		if gotPath != "" {
			t.Fatalf("%s without a lottery upstream reached %q, want nobody answered it", path, gotPath)
		}
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

// TestNewHandlerRoutesPaymentCallback 覆盖支付域那条唯一的公网路径。
//
// 它和上面几个域一样测两件事：配了上游要真的转发过去（含 /api 前缀的归一化），没配就
// 保持 404。这一条特别值得单测，因为**渠道回调是唯一一条从公网打进内网的路径**——
// 少写一个 case 的后果不是页面上一个 404，而是渠道的回调永远到不了，支付单停在 pending，
// 而渠道那边以为通知成功了（它收到的是网关的 404，会按自己的策略重试一阵然后放弃）。
func TestNewHandlerRoutesPaymentCallback(t *testing.T) {
	var gotPath, gotMethod string
	payment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":"SUCCESS"}`))
	}))
	defer payment.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL: "http://merchant.test",
		UserServiceURL:     "http://user.test",
		PaymentServiceURL:  payment.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	for _, path := range []string{
		"/v1/payments/callback/manual_dev",
		"/api/v1/payments/callback/manual_dev",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath, gotMethod = "", ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
			}
			if gotPath != normalizePath(path) {
				t.Errorf("upstream path = %q, want %q", gotPath, normalizePath(path))
			}
			if gotMethod != http.MethodPost {
				t.Errorf("upstream method = %q, want POST", gotMethod)
			}
		})
	}

	// 前缀按路径段比较：/v1/payments-extra 不是支付域。
	gotPath = ""
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/v1/payments-extra/callback/x", nil))
	if res.Code != http.StatusNotFound || gotPath != "" {
		t.Fatalf("status/upstream path = %d/%q, want 404 and no upstream call", res.Code, gotPath)
	}

	// 没配上游时保持 404，而不是落到别的域去（/v1/payments 与它们没有共同前缀，落到
	// default 也是 404——这条断言守的是「将来有人给它加前缀时别把它接走」）。
	unset, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: "http://user.test"})
	if err != nil {
		t.Fatalf("NewHandler() without a payment upstream error = %v", err)
	}
	unsetRes := httptest.NewRecorder()
	unset.ServeHTTP(unsetRes, httptest.NewRequest(http.MethodPost, "/v1/payments/callback/manual_dev", nil))
	if unsetRes.Code != http.StatusNotFound {
		t.Fatalf("status without upstream = %d, want %d", unsetRes.Code, http.StatusNotFound)
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
