package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/controller"
)

// RegisterMiniapp 挂载小程序端的订单路由。
//
// 这一层只套认证（令牌有效），不套权限码：C 端的授权不是「有没有某个权限」，而是
// 「这个令牌是不是一个消费者、他只能碰自己的单」。后半句由 controller 的 requireConsumer
// 与 service 的归属校验负责，权限码在这里既表达不了也挡不住越权。
func RegisterMiniapp(
	r *runtime.HTTPRouter,
	handler *controller.MiniappOrderController,
	afterSales *controller.MiniappAfterSaleController,
	authenticate func(http.Handler) http.Handler,
) {
	// 装配处没给认证中间件时一律 401，而不是放行：这条路径是公开入口，
	// 任何「忘了装配」都不能变成没有身份也能下单。
	protect := authenticate
	if protect == nil {
		protect = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"errorCode":"UNAUTHORIZED","errorMessage":"unauthorized"}`))
			})
		}
	}

	orders := protect(http.HandlerFunc(handler.Orders))
	// 申请退款挂在订单树上（申请的对象是这一单），路径比 /{id} 长，必须注册在它前面。
	r.HandleFunc("/v1/miniapp/orders/{id}/after-sales", orders.ServeHTTP)
	r.HandleFunc("/v1/miniapp/orders/{id}/cancel", orders.ServeHTTP)
	// 发起支付也挂在订单树上，但它的 {orderNo} 与上面三个的 {id} **不是同一个键**：这条
	// 链路的两端（小程序、支付服务）手上都只有订单号，内部 UUID 在这里没有用武之地。
	// 两个键在同一棵树里并存是有意的——它不改变分发（handler 是同一个，路径是自己剥的），
	// 只影响这一行读起来像不像笔误，所以写在这里说明白。
	r.HandleFunc("/v1/miniapp/orders/{orderNo}/pay", orders.ServeHTTP)
	r.HandleFunc("/v1/miniapp/orders/{id}", orders.ServeHTTP)
	r.HandleFunc("/v1/miniapp/orders", orders.ServeHTTP)

	// 售后单自己那棵树：申请之后动它（撤销）用的是售后单号。
	afterSaleActions := protect(http.HandlerFunc(afterSales.AfterSales))
	r.HandleFunc("/v1/miniapp/after-sales/{afterSaleNo}/cancel", afterSaleActions.ServeHTTP)
}
