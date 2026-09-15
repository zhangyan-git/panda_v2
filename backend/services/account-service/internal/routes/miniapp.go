package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/controller"
)

// RegisterMiniapp 挂载小程序端的资产账户路由：福卡与咖啡豆各一条。
//
// 这一层只套认证（令牌有效），不套权限码：C 端的授权不是「有没有某个权限」，而是
// 「这个令牌是不是一个消费者、他只能看自己的账户」。后半句由 controller 的
// requireConsumer 负责，权限码在这里既表达不了也挡不住越权。
func RegisterMiniapp(
	r *runtime.HTTPRouter,
	cards *controller.MiniappFortuneCardController,
	beans *controller.MiniappCoffeeBeanController,
	authenticate func(http.Handler) http.Handler,
) {
	// 装配处没给认证中间件时一律 401，而不是放行：这两条路径是公开入口，任何「忘了装配」
	// 都不能变成没有身份也能读账户。
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

	cardRoutes := protect(http.HandlerFunc(cards.Cards))
	r.HandleFunc("/v1/miniapp/fortune-cards", cardRoutes.ServeHTTP)

	beanRoutes := protect(http.HandlerFunc(beans.Beans))
	r.HandleFunc("/v1/miniapp/coffee-beans", beanRoutes.ServeHTTP)
}
