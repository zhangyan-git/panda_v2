package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/controller"
)

// RegisterAdmin 挂载后台的订单路由。认证由调用方套上，这样「忘了装配」不会变成
// fail-open——装配处（cmd/main.go 的 adminAuthorizer）一定会传一个中间件进来。
//
// 注册顺序按「从长到短」：gorilla/mux 取第一个匹配的路径，/{id}/cancel 注册在 /{id}
// 之后就会被永远解析成 id="cancel"。同一棵树上的路由必须保持这个顺序（售后那棵树同理：
// /{afterSaleNo}/approve 要在 /{afterSaleNo} 之前）。
func RegisterAdmin(
	r *runtime.HTTPRouter,
	handler *controller.AdminOrderController,
	afterSales *controller.AdminAfterSaleController,
	authenticate func(...string) func(http.Handler) http.Handler,
) {
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

	list := protect("order:read", http.HandlerFunc(handler.Orders))
	detail := protect("order:read", http.HandlerFunc(handler.Orders))
	// 取消是人工干预用户资产的动作，用 manage 而不是 read：能看订单不等于能替用户关单。
	cancel := protect("order:manage", http.HandlerFunc(handler.Orders))

	r.HandleFunc("/v1/admin/orders/{id}/cancel", cancel.ServeHTTP)
	r.HandleFunc("/v1/admin/orders/{id}", detail.ServeHTTP)
	r.HandleFunc("/v1/admin/orders", list.ServeHTTP)

	// 售后是另一棵树：售后单有自己的单号（REF 开头），客服拿到的就是它，不需要先知道
	// 订单号。同样从长到短注册。
	afterSaleList := protect("order:read", http.HandlerFunc(afterSales.AfterSales))
	// 审核是分配用户资产的动作，单独一个权限码：能看申请不等于能同意退款。
	// 权限码见 migrations/identity/017_order_after_sale.sql。
	afterSaleReview := protect("order:after-sale:audit", http.HandlerFunc(afterSales.AfterSales))

	r.HandleFunc("/v1/admin/after-sales/{afterSaleNo}/approve", afterSaleReview.ServeHTTP)
	r.HandleFunc("/v1/admin/after-sales/{afterSaleNo}/reject", afterSaleReview.ServeHTTP)
	r.HandleFunc("/v1/admin/after-sales", afterSaleList.ServeHTTP)
}
