// Package routes 注册 coffee-machine-service 的 HTTP 路由。
package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/controller"
)

// 权限码。读与写分开，余额调整再单独一个码：给设备改个名字和给设备加钱是两件事，
// 合成一个码意味着想让人维护设备资料就得连资金操作一起给出去。
const (
	permRead    = "coffee_machine:read"
	permManage  = "coffee_machine:manage"
	permBalance = "coffee_machine:balance"
)

// methodRoute 把一个 HTTP 方法绑到它自己的权限码和处理器上。
type methodRoute struct {
	permission string
	handler    http.HandlerFunc
}

// RegisterAdmin 挂载后台的设备域路由。鉴权由调用方套在最外层，这样一个漏传校验器的
// 调用不可能变成失败开放。
//
// 路由底层是 gorilla/mux：{id} 只吃一个路径段，所以 /devices/{id} 与
// /devices/{id}/status 是两个模式而不是前缀关系，可以各自绑不同方法。
//
// **一条路径只注册一次。** mux 的 HandleFunc 是往路由表里追加，而不是按路径合并：
// 对同一个 path 调两次，先注册的那条没有方法匹配器、会吃掉所有方法，后注册的永远
// 到不了——表现是 GET 正常、POST 回 404。所以「GET 要读权限、POST 要写权限」这件事
// 写在同一张 byMethod 表里，由 routeFor 在注册时按方法各自套上权限中间件。
func RegisterAdmin(r *runtime.HTTPRouter, read *controller.AdminMasterDataController, write *controller.AdminWriteController, authenticate func(...string) func(http.Handler) http.Handler) {
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"errorCode":"UNAUTHORIZED","errorMessage":"unauthorized"}`))
	})
	protect := func(permission string, next http.Handler) http.Handler {
		if authenticate == nil {
			return unauthorized
		}
		return authenticate(permission)(next)
	}
	// routeFor 在建表时就把每个方法各自包上自己的权限中间件，请求来时只按方法查表。
	routeFor := func(byMethod map[string]methodRoute) http.HandlerFunc {
		protected := make(map[string]http.HandlerFunc, len(byMethod))
		for method, route := range byMethod {
			protected[method] = protect(route.permission, route.handler).ServeHTTP
		}
		return func(w http.ResponseWriter, r *http.Request) {
			h, ok := protected[r.Method]
			if !ok {
				// 路径存在但方法不对，按 kratos 的既有做法回 404 而不是 405：
				// 这套路由表里没有声明 Allow 的地方，回 405 还得自己拼头。
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}

	// ---- 厂商 ----
	r.HandleFunc("/v1/admin/coffee-machines/manufacturers", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, read.Manufacturers},
		http.MethodPost: {permManage, write.CreateManufacturer},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/manufacturers/{id}", routeFor(map[string]methodRoute{
		http.MethodPut: {permManage, write.UpdateManufacturer},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/manufacturers/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPatch: {permManage, write.SetManufacturerStatus},
	}))

	// ---- 设备 ----
	r.HandleFunc("/v1/admin/coffee-machines/devices", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, read.ListDevices},
		http.MethodPost: {permManage, write.CreateDevice},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/devices/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, read.GetDevice},
		http.MethodPut: {permManage, write.UpdateDevice},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/devices/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPatch: {permManage, write.SetDeviceStatus},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/devices/{id}/balance", routeFor(map[string]methodRoute{
		http.MethodPost: {permBalance, write.AdjustDeviceBalance},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/devices/{id}/drinks", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, read.ListDeviceDrinks},
	}))

	// ---- 饮品 ----
	r.HandleFunc("/v1/admin/coffee-machines/drinks", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, read.Drinks},
		http.MethodPost: {permManage, write.CreateDrink},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/drinks/{id}", routeFor(map[string]methodRoute{
		http.MethodPut: {permManage, write.UpdateDrink},
	}))
	r.HandleFunc("/v1/admin/coffee-machines/drinks/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPatch: {permManage, write.SetDrinkStatus},
	}))
}
