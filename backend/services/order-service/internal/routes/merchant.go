package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/controller"
)

// RegisterMerchant 挂载商户端的订单路由。
//
// 与 RegisterAdmin 分开，是因为这条链路里有一样东西是后台那条完全没有的：数据范围。
// 后台的每条路由先过权限码再看数据，商户域**没有权限码**（009 删掉商户角色表之后，商户
// 账号只有范围、没有角色），所以这里没有 requirePermission 这一层：没有任何权限码可以
// 授予它，加一个进来的结果只会是一条永远为空的检查。
//
// 中间件链由调用方装配（auth.Middleware → authz.MerchantMiddleware），传 nil 一律
// 失败关闭：一个漏传鉴权器的调用不可能变成失败开放。
//
// 只有读。商户端能不能写这个问题，答案写在路由表里，而不是留给每个处理器自己去记得挡。
func RegisterMerchant(
	r *runtime.HTTPRouter,
	orders *controller.MerchantOrderController,
	authorize func(http.Handler) http.Handler,
) {
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"errorCode":"UNAUTHORIZED","errorMessage":"unauthorized"}`))
	})
	protected := func(h http.HandlerFunc) http.HandlerFunc {
		if authorize == nil {
			return unauthorized.ServeHTTP
		}
		authorized := authorize(h).ServeHTTP
		return func(w http.ResponseWriter, req *http.Request) {
			// 路径存在但方法不对，按这套路由表的既有做法回 404 而不是 405。
			if req.Method != http.MethodGet {
				http.NotFound(w, req)
				return
			}
			authorized(w, req)
		}
	}

	r.HandleFunc("/v1/merchant/orders", protected(orders.Orders))
	r.HandleFunc("/v1/merchant/orders/{id}", protected(orders.Orders))
}
