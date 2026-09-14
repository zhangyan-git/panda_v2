package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/controller"
)

// 这个文件只验路由表本身：哪个方法落在哪个处理器上、带的是哪个权限码。处理器不会被
// 真正调用——授了权的那个中间件在进处理器之前就短路了——所以控制器可以带 nil 服务，
// 整个用例不碰数据库。
//
// 它挡的是一个很难在别处发现的错法：gorilla/mux 的 HandleFunc 是往路由表里追加而不是
// 按路径合并，同一条路径注册两次的话，先注册的那条会吃掉所有方法。表现出来是 GET 好
// 用、POST 静默回 404——只看读接口的自测完全看不出来，而这套路由里几乎每条路径都是
// 读写在同一个 path 上。

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

func TestAdminRoutesMapEachMethodToItsPermission(t *testing.T) {
	server := khttp.NewServer()
	var seen string
	RegisterAdmin(
		runtime.NewHTTPRouter(server),
		controller.NewAdminMasterDataController(nil),
		controller.NewAdminWriteController(nil),
		probeAuthorizer(&seen),
	)

	cases := []struct {
		method, path string
		want         string
	}{
		// 读路径
		{http.MethodGet, "/v1/admin/coffee-machines/manufacturers", permRead},
		{http.MethodGet, "/v1/admin/coffee-machines/devices", permRead},
		{http.MethodGet, "/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c", permRead},
		{http.MethodGet, "/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c/drinks", permRead},
		{http.MethodGet, "/v1/admin/coffee-machines/drinks", permRead},

		// 写路径：这些在「同一条路径注册两次」的写法下会回 404，因为它们注册在
		// 只带方法分派的第二条路由上，永远匹配不到。
		{http.MethodPost, "/v1/admin/coffee-machines/manufacturers", permManage},
		{http.MethodPut, "/v1/admin/coffee-machines/manufacturers/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c", permManage},
		{http.MethodPatch, "/v1/admin/coffee-machines/manufacturers/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c/status", permManage},
		{http.MethodPost, "/v1/admin/coffee-machines/devices", permManage},
		{http.MethodPut, "/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c", permManage},
		{http.MethodPatch, "/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c/status", permManage},
		// 设备详情那一屏的改价、改上下架走的就是这两个「饮品自身」的写接口：饮品行
		// 自带 device_id，没有一条 /devices/{id}/drinks/{drinkId} 的写路由了。
		{http.MethodPost, "/v1/admin/coffee-machines/drinks", permManage},
		{http.MethodPut, "/v1/admin/coffee-machines/drinks/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c", permManage},
		{http.MethodPatch, "/v1/admin/coffee-machines/drinks/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c/status", permManage},

		// 余额是单独一个码：能让运营维护饮品目录，不该顺手给出加钱的能力。
		// 读流水与调余额同码：这一条 GET 是和 POST 写在同一个 routeFor 表里的，漏掉它
		// 就是「同一条路径注册两次」那类错法的另一半——POST 能用、GET 回 404。
		{http.MethodPost, "/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c/balance", permBalance},
		{http.MethodGet, "/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c/balance", permBalance},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			seen = ""
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s did not match any route", tc.method, tc.path)
			}
			if rec.Code != http.StatusTeapot {
				t.Fatalf("%s %s status = %d, want the authorizer to have run", tc.method, tc.path, rec.Code)
			}
			if seen != tc.want {
				t.Fatalf("%s %s required %q, want %q", tc.method, tc.path, seen, tc.want)
			}
		})
	}
}

// TestAdminRoutesRejectUndeclaredMethods 反方向：路径存在但方法没声明，要回 404 而不是
// 落到某个处理器上。DELETE 一个厂商不在这次范围内（表上是 ON DELETE RESTRICT），
// 所以它必须显式地什么都不做。
func TestAdminRoutesRejectUndeclaredMethods(t *testing.T) {
	server := khttp.NewServer()
	var seen string
	RegisterAdmin(
		runtime.NewHTTPRouter(server),
		controller.NewAdminMasterDataController(nil),
		controller.NewAdminWriteController(nil),
		probeAuthorizer(&seen),
	)

	paths := []string{
		"/v1/admin/coffee-machines/manufacturers",
		"/v1/admin/coffee-machines/manufacturers/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c",
		"/v1/admin/coffee-machines/devices",
		"/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c",
		"/v1/admin/coffee-machines/drinks",
	}
	for _, method := range []string{http.MethodDelete, http.MethodPatch, http.MethodTrace} {
		for _, path := range paths {
			seen = ""
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404", method, path, rec.Code)
			}
			if seen != "" {
				t.Fatalf("%s %s reached the authorizer with %q, want no route", method, path, seen)
			}
		}
	}
}

// TestAdminRoutesHaveNoDeviceScopedDrinkWrite 守住已经拆掉的写法：饮料行自带 device_id
// 之后，「设备 × 饮品」不再是一张单独可写的关系，原来的 PUT /devices/{id}/drinks/{drinkId}
// 必须真的消失。留着一条还能改价的路由，会让「同一个字段有两个写入方」重新长回来——
// 而两个写入方迟早对同一个值给出不同答案。
//
// 读那条留着（设备详情页要一个「这台设备全部饮品」的入口），所以这里只打 PUT。
func TestAdminRoutesHaveNoDeviceScopedDrinkWrite(t *testing.T) {
	server := khttp.NewServer()
	var seen string
	RegisterAdmin(
		runtime.NewHTTPRouter(server),
		controller.NewAdminMasterDataController(nil),
		controller.NewAdminWriteController(nil),
		probeAuthorizer(&seen),
	)

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/v1/admin/coffee-machines/devices/2f8a1f1e-0f4a-4a5e-9c3b-1d2e3f4a5b6c/drinks/3a9b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404：这条写路由应当已经在迁移到 device_id 时拆掉", rec.Code)
	}
	if seen != "" {
		t.Fatalf("reached the authorizer with %q, want no route", seen)
	}
}

// TestRegisterAdminWithoutAuthorizerFailsClosed 漏传鉴权器时必须是 401，不能变成
// 「所有路由都对外开放」。
func TestRegisterAdminWithoutAuthorizerFailsClosed(t *testing.T) {
	server := khttp.NewServer()
	RegisterAdmin(
		runtime.NewHTTPRouter(server),
		controller.NewAdminMasterDataController(nil),
		controller.NewAdminWriteController(nil),
		nil,
	)

	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/coffee-machines/devices", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no authorizer is wired", rec.Code)
	}
}
