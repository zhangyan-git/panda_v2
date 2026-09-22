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

// 会员域两端各有一棵树，所以这条用例要证两件事：小程序那半棵**抢在 /v1/miniapp 前面**，
// 以及后台那**四条**前缀一条都没漏。
//
// 前者漏了的症状是 /v1/miniapp/membership/plans 被转给 user-service，小程序拿到的 404
// 看着像「用户服务没有这个接口」；后者漏了的症状是网关自己的 404，看着像服务没部署。
// 两种都不报错，所以只能在这里钉住。
func TestNewHandlerRoutesMembershipPaths(t *testing.T) {
	var gotPath string
	membership := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = "membership:" + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer membership.Close()

	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = "user:" + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer user.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL:   "http://merchant.test",
		UserServiceURL:       user.URL,
		MembershipServiceURL: membership.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	for _, path := range []string{
		// 小程序那半棵。树根、套餐列表与自动续费开关三条都列出来：
		// membership-service 的 routes/miniapp.go 注册的正是这三条，而 /auto-renew
		// 是**注册顺序**踩过坑的那一条（顺序反了会回 404）。
		"/v1/miniapp/membership",
		"/api/v1/miniapp/membership",
		"/v1/miniapp/membership/plans",
		"/v1/miniapp/membership/auto-renew",
		// 小程序那半棵里最里面的一条：店铺码领取。它在 /campaigns 下面，是唯一一条
		// 会写会员的接口，也是那一批里唯一一个**没被漏掉**的（它落在上面前缀里）。
		"/v1/miniapp/membership/campaigns/claim",
		// 后台那半棵的**四棵树**，各带一条 {id} 与一条动作路径——会员那四个动作
		// （freeze / unfreeze / revoke / expire）长在 {id} 下面，最容易因为前缀少写一段而整片漏掉。
		//
		// 四棵树是逐条钉的：/v1/admin/memberships 按路径段比较匹配不上
		// /v1/admin/membership-subscriptions，所以后两棵树各写一遍不算重复。少了这组的哪一步，
		// 症状是**那一页整片 404**，而页面会把它显示成一句业务错话（踩过一次，见 proxy.go 上那
		// 段说明）。
		"/v1/admin/membership-plans",
		"/v1/admin/membership-plans/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		"/v1/admin/membership-plans/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/status",
		"/v1/admin/membership-subscriptions",
		"/v1/admin/membership-subscriptions/stats",
		"/v1/admin/membership-subscriptions/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/cancel",
		"/v1/admin/membership-campaigns",
		"/v1/admin/membership-campaigns/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/claims",
		"/v1/admin/memberships",
		"/v1/admin/memberships/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		"/v1/admin/memberships/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/freeze",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath = ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
			if res.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
			}
			if gotPath != "membership:"+normalizePath(path) {
				t.Errorf("upstream = %q, want membership:%q (user-service or the gateway itself answered instead)", gotPath, normalizePath(path))
			}
		})
	}

	// 反方向：同前缀下的其它路径仍然归各自的上游。少了这一条，把 /v1/miniapp 或
	// /v1/admin 整段挪给会员服务也能让上面全绿。
	for _, tc := range []struct{ path, want string }{
		{"/v1/miniapp/auth/login", "user:/v1/miniapp/auth/login"},
		{"/v1/admin/users", "user:/v1/admin/users"},
	} {
		gotPath = ""
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if gotPath != tc.want {
			t.Fatalf("%s upstream = %q, want %q", tc.path, gotPath, tc.want)
		}
	}

	// 前缀按路径段比较：memberships-extra 与 memberships 不是同一个前缀。这条落在
	// default，所以是网关自己的 404。四条后台前缀都要试——它们本来就是分开写的四行，
	// 谁被写成字符串前缀谁在这里露出来。
	//
	// membership-subscriptions-extra 那一行还多钉一件事：/v1/admin/memberships 与
	// /v1/admin/membership-subscriptions 是两个前缀（段比较，不是字符串前缀），
	// 把后者写成前者的通配也能让上面全绿，唯独这条会挂。
	for _, path := range []string{
		"/v1/admin/memberships-extra",
		"/v1/admin/membership-plans-extra",
		"/v1/admin/membership-subscriptions-extra",
		"/v1/admin/membership-campaigns-extra",
	} {
		gotPath = ""
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound || gotPath != "" {
			t.Fatalf("%s status/upstream = %d/%q, want 404 and no upstream call", path, res.Code, gotPath)
		}
	}

	// 没配上游时保持 404，而不是被别处接走。小程序那一条尤其要试：不显式拦住的话它会
	// 落到下面的 /v1/miniapp，客户端拿到的 404 看着像「用户服务没这个接口」，而真正的原因
	// 是会员服务没接上。
	unset, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() without a membership upstream error = %v", err)
	}
	for _, path := range []string{"/v1/miniapp/membership", "/v1/admin/memberships"} {
		gotPath = ""
		res := httptest.NewRecorder()
		unset.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s status without upstream = %d, want %d", path, res.Code, http.StatusNotFound)
		}
		if gotPath != "" {
			t.Fatalf("%s without a membership upstream reached %q, want nobody answered it", path, gotPath)
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

// TestNewHandlerRoutesPaymentCallback 覆盖支付域从公网打进来的那两条回调路径。
//
// 它和上面几个域一样测两件事：配了上游要真的转发过去（含 /api 前缀的归一化），没配就
// 保持 404。这一条特别值得单测，因为**渠道回调是仅有的从公网打进内网的路径**——
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
		// 协议变更通知（签约 / 解约）与扣款结果通知（某一期扣到了没有）走同一条前缀、同一个
		// 上游。它们落在 /v1/payments 这棵前缀里是**有意的**——网关这一层按上游分，不按用途
		// 分；渠道配 notify_url 时用的是完整地址，多一段路径不需要网关多认识一个前缀。列在这里
		// 是为了钉住它们真的被转出去：签约通知到不了的症状与支付回调一模一样，但更隐蔽（用户
		// 签了，我们这边永远是待签约）；扣款结果通知到不了的症状是「钱扣了、会员没续，而且
		// 那一期永远停在已受理」。
		"/v1/payments/agreement-notify/manual_dev",
		"/api/v1/payments/agreement-notify/manual_dev",
		"/v1/payments/agreement-charge-notify/manual_dev",
		"/api/v1/payments/agreement-charge-notify/manual_dev",
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

	// 后台那几条只读路径走同一条上游。
	//
	// **分账那三条是分开列的**："/v1/admin/settlement" 与 "/v1/admin/payments" 是两个不同的
	// 路径段，hasPathPrefix 一个都匹配不上另一个。这一条曾经真漏过——分账接口在支付服务里
	// 已经注册好（直连 :18085 回 401），网关这张表里没列，于是后台点开分账三页整页 404，
	// 而 404 在页面上表现为「接口不存在」，看着像后端没部署。所以这三条路径写在这里是为了
	// 钉住**网关认识它们**，不是为了好看。
	for _, path := range []string{
		"/v1/admin/payments",
		"/v1/admin/payments/PAY20260916140744000053049",
		"/v1/admin/settlement/rules",
		"/v1/admin/settlement/accounts",
		"/v1/admin/settlement/tasks",
		"/v1/admin/settlement/channels",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath, gotMethod = "", ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
			}
			if gotPath != path {
				t.Errorf("upstream path = %q, want %q", gotPath, path)
			}
			if gotMethod != http.MethodGet {
				t.Errorf("upstream method = %q, want GET", gotMethod)
			}
		})
	}

	// 支付方式与渠道那两条前缀**已经删了**（收钱那四种是支付服务里的一张常量表，后台没有
	// 可配的东西），所以它们今天不该被转给任何上游：落到 default 是一句 404。
	//
	// 留着这条断言是因为这一对路径**在支付域里有共同前缀的兄弟**（/v1/admin/payments），
	// 写错的方式不是「漏了一条」而是「把前缀放宽成一条通配」——那会把两个已经删掉的接口
	// 继续转给支付服务，而它那边连路由都没注册，症状是一个从 404 变成 404 的假象。顺带
	// 也钉住 hasPathPrefix 是按**路径段**比较的："/v1/admin/payments" 匹配不上
	// "/v1/admin/payment-methods"（与上面 /v1/payments-extra 是同一个道理）。
	for _, path := range []string{
		"/v1/admin/payment-methods",
		"/v1/admin/payment-channels",
		"/v1/admin/payment-channels/providers",
	} {
		t.Run("removed_"+path, func(t *testing.T) {
			gotPath = ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
			if res.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d（这一条已经删了，不该再转到支付服务）", res.Code, http.StatusNotFound)
			}
			if gotPath != "" {
				t.Errorf("upstream path = %q, want 空的——它不该被转给任何上游", gotPath)
			}
		})
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

// TestNewHandlerRoutesPartnerPaths 覆盖合作方（开放平台）域的三条前缀。
//
// 三条**必须分开写**这件事是本用例的主要目的：hasPathPrefix 按路径段比较，"/v1/admin/partners"
// 匹配不上 "/v1/admin/partner-call-logs"（partner-call-logs 是另一个路径段，不是 partners 的
// 子路径），而 /v1/openapi 根本不在 /v1/admin 下面。少列一条的症状只是一个静默的 404：
// 调用日志页打不开、或者合作方调进来拿到网关自己的 404，而这份自测里只试 /v1/admin/partners
// 的版本完全看不出来——与 membership / payment 那两段是同一个坑。
//
// ⚠️ 线下刷卡机那条（POST /v1/openapi/device/sync-order，归 order-service）**当前还不存在**，
// 所以这里没有它的断言。加它的时候要记住：设备那条 case 必须排在 /v1/openapi 这条**之前**，
// 否则设备同步会被转给合作方服务，拿到一个看着像「对方没实现这个接口」的 404。见 proxy.go
// 里那条 case 上的注释。
func TestNewHandlerRoutesPartnerPaths(t *testing.T) {
	var gotPath, gotMethod string
	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = "partner:"+r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer partner.Close()

	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = "user:"+r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer user.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL: "http://merchant.test",
		UserServiceURL:     user.URL,
		PartnerServiceURL:  partner.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	// 后台那两棵树：合作方列表 / 详情 / 启停 / 密钥（含密钥的启停，那是**最深**的一条，
	// 前缀少写一段时它和它的兄弟一起消失），以及独立的调用日志。
	for _, path := range []string{
		"/v1/admin/partners",
		"/api/v1/admin/partners",
		"/v1/admin/partners/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		"/v1/admin/partners/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/status",
		"/v1/admin/partners/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/keys",
		"/v1/admin/partners/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/keys/1f2a3b4c-5d6e-4f7a-8b9c-0d1e2f3a4b5c",
		"/v1/admin/partners/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/keys/1f2a3b4c-5d6e-4f7a-8b9c-0d1e2f3a4b5c/status",
		"/v1/admin/partner-call-logs",
		"/v1/admin/partner-call-logs?partnerId=0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		// 开放接口那一棵。合作方带的是自己算的签名，网关只转发——**不挂我们的认证**，
		// 所以这里能通不代表任何人可用（验签在合作方服务里）。
		"/v1/openapi/member-price-entitlement",
		"/api/v1/openapi/member-price-entitlement",
	} {
		t.Run(path, func(t *testing.T) {
			gotPath, gotMethod = "", ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
			if res.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
			}
			wantPath := normalizePath(strings.SplitN(path, "?", 2)[0])
			if gotPath != "partner:"+wantPath {
				t.Errorf("upstream = %q, want partner:%q (user-service or the gateway itself answered instead)", gotPath, wantPath)
			}
			if gotMethod != http.MethodGet {
				t.Errorf("upstream method = %q, want GET", gotMethod)
			}
		})
	}

	// 反方向：同前缀下的其它路径仍然归各自的上游。少了这一条，把 /v1/admin 或 /v1/miniapp
	// 整段挪给合作方服务也能让上面全绿。
	for _, tc := range []struct{ path, want string }{
		{"/v1/admin/users", "user:/v1/admin/users"},
		{"/v1/admin/roles", "user:/v1/admin/roles"},
		{"/v1/admin/operation-logs", "user:/v1/admin/operation-logs"},
		{"/v1/miniapp/auth/login", "user:/v1/miniapp/auth/login"},
	} {
		gotPath = ""
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if gotPath != tc.want {
			t.Fatalf("%s upstream = %q, want %q", tc.path, gotPath, tc.want)
		}
	}

	// 前缀按路径段比较：partners-extra 与 partners 不是同一个前缀。两条后台前缀都要试——
	// 它们本来就是分开写的两行，谁被写成字符串前缀谁在这里露出来。
	for _, path := range []string{
		"/v1/admin/partners-extra",
		"/v1/admin/partner-call-logs-extra",
		"/v1/openapi-extra/member-price-entitlement",
	} {
		gotPath = ""
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound || gotPath != "" {
			t.Fatalf("%s status/upstream = %d/%q, want 404 and no upstream call", path, res.Code, gotPath)
		}
	}

	// 没配上游时保持 404，而不是被别处接走——尤其是 /v1/openapi 那一条：本用例里它与
	// user-service 没有共同前缀，但设备域那条路由加进来之后它就会有了（见上面那段说明）。
	unset, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() without a partner upstream error = %v", err)
	}
	for _, path := range []string{"/v1/admin/partners", "/v1/openapi/member-price-entitlement"} {
		gotPath = ""
		res := httptest.NewRecorder()
		unset.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("%s status without upstream = %d, want %d", path, res.Code, http.StatusNotFound)
		}
		if gotPath != "" {
			t.Fatalf("%s without a partner upstream reached %q, want nobody answered it", path, gotPath)
		}
	}
}

// 商户端的三棵前缀各接一个上游，而其中两棵是「后加的服务」那种——没配就 404。
//
// 值得单测的理由和别的域一样，只是后果更绕：三条路径同时落在 /v1/merchant 这一棵上，
// 而 switch 取第一个成立的 case——写错一条，症状是某一页 404，看着像「后端没实现这个
// 接口」，实际是网关把它送错了上游（或压根没送）。
//
// 三条必须分开写：hasPathPrefix 按路径段比较，"/v1/merchant" 一条通配接不到
// stores/devices/orders 里任何一个。
func TestNewHandlerRoutesMerchantConsolePaths(t *testing.T) {
	var gotMerchant, gotCoffee, gotOrder string
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMerchant = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer merchant.Close()
	coffee := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCoffee = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer coffee.Close()
	order := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrder = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer order.Close()

	h, err := NewHandler(Config{
		MerchantServiceURL:      merchant.URL,
		UserServiceURL:          "http://user.test",
		CoffeeMachineServiceURL: coffee.URL,
		OrderServiceURL:         order.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	for _, tt := range []struct {
		path string
		// seen 由上游闭包写，看哪个上游收到了这次请求。
		seen func() string
		want string
	}{
		// 门店：与后台那棵 /v1/admin/stores 同属 merchant-service。
		{"/api/v1/merchant/stores", func() string { return gotMerchant }, "/v1/merchant/stores"},
		{"/api/v1/merchant/stores/abc", func() string { return gotMerchant }, "/v1/merchant/stores/abc"},
		{"/api/v1/merchant/devices", func() string { return gotCoffee }, "/v1/merchant/devices"},
		{"/api/v1/merchant/devices/abc", func() string { return gotCoffee }, "/v1/merchant/devices/abc"},
		{"/api/v1/merchant/orders", func() string { return gotOrder }, "/v1/merchant/orders"},
		{"/api/v1/merchant/orders/abc", func() string { return gotOrder }, "/v1/merchant/orders/abc"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			gotMerchant, gotCoffee, gotOrder = "", "", ""
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.Code)
			}
			// 只断言「该到的那个上游收到了」，还断言它收到的是归一化之后的路径：
			// 三个上游都记下来是为了让「送错了上游」也报出来，而不只是「没送到」。
			if got := tt.seen(); got != tt.want {
				t.Fatalf("upstream path = %q, want %q", got, tt.want)
			}
			for name, got := range map[string]string{"merchant": gotMerchant, "coffee": gotCoffee, "order": gotOrder} {
				if got != "" && got != tt.want {
					t.Fatalf("%s 上游收到了 %q，这条路径不该送给它", name, got)
				}
			}
		})
	}

	// 登录与「我是谁」留在 user-service：它们不带数据范围，与上面三棵不是一回事。
	// 这条路径的断言方式是「三个业务上游都没收到」——user 上游在这条用例里是个假地址，
	// 请求会以 502 结束，那正是「它被送去 user-service 了」的证据。
	gotMerchant, gotCoffee, gotOrder = "", "", ""
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/v1/merchant/auth/login", nil))
	if gotMerchant != "" || gotCoffee != "" || gotOrder != "" {
		t.Fatal("/v1/merchant/auth/login 被送到了业务上游")
	}

	// 设备域没配上游时保持 404，而不是转到一个空地址上。
	unset, err := NewHandler(Config{MerchantServiceURL: merchant.URL, UserServiceURL: "http://user.test"})
	if err != nil {
		t.Fatalf("NewHandler() without a coffee upstream error = %v", err)
	}
	unsetRes := httptest.NewRecorder()
	unset.ServeHTTP(unsetRes, httptest.NewRequest(http.MethodGet, "/v1/merchant/devices", nil))
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
