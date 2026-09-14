package routes

import (
	"net/http"
	"strings"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/controller"
)

// RegisterAdmin mounts administrative coupon routes. Authentication is applied
// by the caller so a missing verifier cannot accidentally become fail-open.
//
// 路由底层是 gorilla/mux，只做精确匹配：早先注册的 "/v1/admin/coupons/templates/"
// 这类尾斜杠写法不是前缀匹配，带 id 的路径会直接 404。所有需要路径参数的路由
// 都必须写成 {id} 模式，并且把 /stats、/redeem 这类固定后缀注册在 /{id} 之前——
// gorilla/mux 按注册顺序取第一个匹配，反了就永远解析成 id。
func RegisterAdmin(r *runtime.HTTPRouter, handler *controller.AdminCouponController, authenticate func(...string) func(http.Handler) http.Handler) {
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
	issue := protect("coupon:issue", http.HandlerFunc(handler.Issue))
	r.HandleFunc("/v1/admin/coupons/issue", issue.ServeHTTP)

	userCouponHandler := http.HandlerFunc(handler.UserCoupons)
	userCouponsRead := protect("coupon:user-coupon:read", userCouponHandler)
	userCouponsRedeem := protect("coupon:user-coupon:redeem", userCouponHandler)
	userCouponsRevoke := protect("coupon:user-coupon:revoke", userCouponHandler)
	r.HandleFunc("/v1/admin/coupons/user-coupons/{id}/redeem", userCouponsRedeem.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/user-coupons/{id}/revoke", userCouponsRevoke.ServeHTTP)
	// mux 的 {id} 只吃一个路径段，/stats/{user_id} 是两段，必须单独注册。
	r.HandleFunc("/v1/admin/coupons/user-coupons/stats/{user_id}", userCouponsRead.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/user-coupons/{id}", userCouponsRead.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/user-coupons", userCouponsRead.ServeHTTP)

	typeManage := protect("coupon:type:manage", http.HandlerFunc(handler.Types))
	r.HandleFunc("/v1/admin/coupons/types", typeManage.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/types/{id}", typeManage.ServeHTTP)

	templateRead := protect("coupon:read", http.HandlerFunc(handler.Templates))
	templateManage := protect("coupon:template:manage", http.HandlerFunc(handler.Templates))
	templateAudit := protect("coupon:template:audit", http.HandlerFunc(handler.Templates))
	templateDispatch := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/audit") {
			templateAudit.ServeHTTP(w, req)
			return
		}
		if req.Method == http.MethodGet {
			templateRead.ServeHTTP(w, req)
			return
		}
		templateManage.ServeHTTP(w, req)
	})
	r.HandleFunc("/v1/admin/coupons/templates/{id}/audit", templateDispatch.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/templates/{id}/status", templateDispatch.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/templates/{id}/stats", templateRead.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/templates/{id}", templateDispatch.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/templates", templateDispatch.ServeHTTP)

	batchRead := protect("coupon:batch:manage", http.HandlerFunc(handler.Batches))
	r.HandleFunc("/v1/admin/coupons/batches/{id}/stats", batchRead.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/batches/{id}", batchRead.ServeHTTP)
	r.HandleFunc("/v1/admin/coupons/batches", batchRead.ServeHTTP)
}
