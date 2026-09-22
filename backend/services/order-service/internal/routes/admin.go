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
	// 标记完成也是人工干预用户资产的动作（它按承诺把福卡发出去），同样用 manage：
	// 能看订单不等于能替用户完成它。将来的履约完成事件不走这条 HTTP 路，见 service/complete.go。
	complete := protect("order:manage", http.HandlerFunc(handler.Orders))
	// 补建一张取货码单（「钱扣了、单没建出来」的运维入口，见 controller.repairPickupOrder）。
	// 权限码用 manage：它落出来的是**一张真正入账的订单**，与「能看订单」是两件事。
	repairPickup := protect("order:manage", http.HandlerFunc(handler.Orders))

	r.HandleFunc("/v1/admin/orders/{id}/complete", complete.ServeHTTP)
	r.HandleFunc("/v1/admin/orders/{id}/cancel", cancel.ServeHTTP)
	// 这一条**必须在 /{id} 之前**：两条路径的形状一样（/v1/admin/orders/ 后面跟一段），
	// gorilla/mux 取第一个匹配的，排在后面的话 "pickup-repairs" 会被解析成 id。
	// 与上面那两条 /{id}/xxx 同一个道理，只是这次是同一段数。
	r.HandleFunc("/v1/admin/orders/pickup-repairs", repairPickup.ServeHTTP)
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
	// 发起退款（重试出口）：审核已通过、但退款单没建成时，把这件事再推一次。走的是同一个人
	// 群的决定——它不是一次新的审批，而是上一次审批的执行；再要一个权限码只会让同一个审核
	// 人需要两枚码，却没有任何一处安全边界被它挡住。所以复用 order:after-sale:audit。
	r.HandleFunc("/v1/admin/after-sales/{afterSaleNo}/refund", afterSaleReview.ServeHTTP)
	r.HandleFunc("/v1/admin/after-sales", afterSaleList.ServeHTTP)
}
