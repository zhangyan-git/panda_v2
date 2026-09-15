package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/controller"
)

// 这个文件只验路由表本身：哪个方法落在哪个处理器上、带的是哪个权限码。处理器不会被
// 真正调用——授了权的那个中间件在进处理器之前就短路了——所以控制器可以带 nil 服务，
// 整个用例不碰数据库。
//
// 它挡的是一个很难在别处发现的错法：gorilla/mux 的 HandleFunc 是往路由表里追加而不是
// 按路径合并，同一条路径注册两次的话，先注册的那条会吃掉所有方法。表现出来是 GET 好
// 用、POST 静默回 404——只看读接口的自测完全看不出来，而这套路由里几乎每条路径都是
// 读写在同一个 path 上。
//
// 第二件它挡的事是**开奖与作废被人顺手写成 manage**：那等于让任何一个能改活动名字的
// 人也能决定中奖名单。这一条只在权限码上看不出来，所以下面把两条动作的期望值写死。

// probeAuthorizer 返回一个假的 authenticate：它记下每个请求被要求持有的权限码，
// 然后回 418 并**不调用**处理器。418 是「路由命中了、权限码是对的」的信号，
// 404 是「没匹配上」，两者都不依赖服务层的实现。
func probeAuthorizer(seen *string) func(...string) func(http.Handler) http.Handler {
	return func(permissions ...string) func(http.Handler) http.Handler {
		permission := ""
		if len(permissions) > 0 {
			permission = permissions[0]
		}
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				*seen = permission
				w.WriteHeader(http.StatusTeapot)
			})
		}
	}
}

// 几个长得像 uuid 的路径参数。写死而不是随机生成：用例失败时要能一眼看出是哪一条。
const (
	aUUID = "2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c"
	bUUID = "7c1d0e2b-3a4f-4c5d-8e9f-0a1b2c3d4e5f"
)

func TestAdminRoutesMapEachMethodToItsPermission(t *testing.T) {
	server := khttp.NewServer()
	var seen string
	RegisterAdmin(
		runtime.NewHTTPRouter(server),
		controller.NewAdminLotteryController(nil),
		probeAuthorizer(&seen),
	)

	cases := []struct {
		method, path string
		want         string
	}{
		// —— 开通 ——
		{http.MethodGet, "/v1/admin/lottery/activations", permRead},
		{http.MethodPost, "/v1/admin/lottery/activations", permManage},
		{http.MethodGet, "/v1/admin/lottery/activations/" + aUUID, permRead},
		{http.MethodPost, "/v1/admin/lottery/activations/" + aUUID + "/status", permManage},

		// —— 活动与奖池 ——
		{http.MethodGet, "/v1/admin/lottery/campaigns", permRead},
		{http.MethodPost, "/v1/admin/lottery/campaigns", permManage},
		{http.MethodGet, "/v1/admin/lottery/campaigns/" + aUUID, permRead},
		{http.MethodPut, "/v1/admin/lottery/campaigns/" + aUUID, permManage},
		{http.MethodGet, "/v1/admin/lottery/campaigns/" + aUUID + "/prizes", permRead},
		{http.MethodPost, "/v1/admin/lottery/campaigns/" + aUUID + "/status", permManage},

		// —— 期次、开奖、作废 ——
		{http.MethodGet, "/v1/admin/lottery/rounds", permRead},
		{http.MethodGet, "/v1/admin/lottery/rounds/" + aUUID, permRead},
		// 这两条是分开的权限码，不是 manage。见文件头第二段。
		{http.MethodPost, "/v1/admin/lottery/rounds/" + aUUID + "/draw", permDraw},
		{http.MethodPost, "/v1/admin/lottery/rounds/" + aUUID + "/cancel", permDraw},

		// —— 开奖记录 ——
		{http.MethodGet, "/v1/admin/lottery/draws/" + bUUID, permRead},

		// —— 中奖与参与 ——
		{http.MethodGet, "/v1/admin/lottery/wins", permRead},
		{http.MethodGet, "/v1/admin/lottery/wins/" + aUUID, permRead},
		{http.MethodGet, "/v1/admin/lottery/participations", permRead},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			seen = ""
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))

			if recorder.Code == http.StatusNotFound {
				t.Fatalf("%s %s 没有匹配到任何路由", tc.method, tc.path)
			}
			if recorder.Code != http.StatusTeapot {
				t.Fatalf("%s %s 回的是 %d，期望 418（授权中间件短路）", tc.method, tc.path, recorder.Code)
			}
			if seen != tc.want {
				t.Fatalf("%s %s 要的是 %q，期望 %q", tc.method, tc.path, seen, tc.want)
			}
		})
	}
}

// TestAdminRoutesWithoutAuthenticatorDenyEverything 挡的是「忘了装配」变成失败开放。
//
// 少了这条，RegisterAdmin 收到 nil 时所有抽奖接口都会是公开的——包括人工开奖，
// 而它是全系统唯一一个能凭空决定谁中奖的动作。
func TestAdminRoutesWithoutAuthenticatorDenyEverything(t *testing.T) {
	server := khttp.NewServer()
	RegisterAdmin(runtime.NewHTTPRouter(server), controller.NewAdminLotteryController(nil), nil)

	paths := []string{
		"/v1/admin/lottery/activations",
		"/v1/admin/lottery/campaigns",
		"/v1/admin/lottery/rounds",
		"/v1/admin/lottery/wins",
		"/v1/admin/lottery/participations",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("没装配授权中间件时 %s 回的是 %d，期望 401", path, recorder.Code)
			}
		})
	}
}

// TestAdminRoutesUnlistedMethodIsNotFound 确认路径在、方法不对时是 404 而不是落到某个
// 别的处理器上。抽奖没有「改一改」的接口：POST 到 /rounds/{id} 不能变成一次开奖。
func TestAdminRoutesUnlistedMethodIsNotFound(t *testing.T) {
	server := khttp.NewServer()
	var seen string
	RegisterAdmin(runtime.NewHTTPRouter(server),
		controller.NewAdminLotteryController(nil), probeAuthorizer(&seen))

	cases := []struct{ method, path string }{
		{http.MethodPost, "/v1/admin/lottery/rounds/" + aUUID},
		{http.MethodDelete, "/v1/admin/lottery/campaigns/" + aUUID},
		{http.MethodPut, "/v1/admin/lottery/activations/" + aUUID + "/status"},
		{http.MethodPost, "/v1/admin/lottery/wins"},
		{http.MethodPost, "/v1/admin/lottery/draws/" + bUUID},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s %s 回的是 %d，期望 404", tc.method, tc.path, recorder.Code)
			}
		})
	}
}

// TestMiniappRoutesRequireAuthentication 确认小程序那条路**每一条**都在认证后面。
//
// 它挡的错法与上面那条不同：后台那条路有权限码可以断言，小程序这条路只有「有没有套上
// 认证」这一件事可查。漏一条就是一条公开接口，而这里的公开接口会扣用户的福卡。
func TestMiniappRoutesRequireAuthentication(t *testing.T) {
	server := khttp.NewServer()
	reached := false
	RegisterMiniapp(runtime.NewHTTPRouter(server),
		controller.NewMiniAppLotteryController(nil),
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusTeapot)
			})
		})

	paths := []struct{ method, path string }{
		{http.MethodGet, "/v1/miniapp/lottery/campaigns"},
		{http.MethodGet, "/v1/miniapp/lottery/campaigns/" + aUUID},
		{http.MethodPost, "/v1/miniapp/lottery/rounds/" + aUUID + "/participations"},
		{http.MethodGet, "/v1/miniapp/lottery/participations"},
		{http.MethodGet, "/v1/miniapp/lottery/wins"},
		{http.MethodGet, "/v1/miniapp/lottery/wins/" + bUUID},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			reached = false
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if !reached {
				t.Fatalf("%s %s 没有经过认证中间件", tc.method, tc.path)
			}
			if recorder.Code != http.StatusTeapot {
				t.Fatalf("%s %s 回的是 %d，期望 418", tc.method, tc.path, recorder.Code)
			}
		})
	}

	// 装配处漏传中间件时，同样一条都不能通。
	open := khttp.NewServer()
	RegisterMiniapp(runtime.NewHTTPRouter(open), controller.NewMiniAppLotteryController(nil), nil)
	for _, tc := range paths {
		t.Run("no-auth "+tc.method+" "+tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			open.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("没装配认证中间件时 %s %s 回的是 %d，期望 401", tc.method, tc.path, recorder.Code)
			}
		})
	}
}
