package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/controller"
)

// RegisterMiniapp 挂载小程序端的抽奖路由。
//
// 这一层只套认证（令牌有效），**不套权限码**：C 端的授权不是「有没有某个权限」，而是
// 「这个令牌是不是一个消费者、他只能碰自己的那一份」。后半句由 controller 的 requireConsumer
// 与逐条记录的归属比较负责，权限码在这里既表达不了也挡不住越权。
//
// 与后台那条路不同，这里没有 routeFor：小程序的动作与路径一一对应（一条路径一个方法），
// 没有「同一个 path 上读写各要一个码」的情形。仍然是一条路径只注册一次。
func RegisterMiniapp(
	r *runtime.HTTPRouter,
	handler *controller.MiniAppLotteryController,
	authenticate func(http.Handler) http.Handler,
) {
	// 装配处没给认证中间件时一律 401，而不是放行：这条路径是公开入口，
	// 任何「忘了装配」都不能变成没有身份也能参与抽奖（参与要扣用户的福卡）。
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

	campaigns := protect(http.HandlerFunc(handler.Campaigns))
	rounds := protect(http.HandlerFunc(handler.Rounds))
	participations := protect(http.HandlerFunc(handler.Participations))
	wins := protect(http.HandlerFunc(handler.Wins))

	// 抽奖中心：活动列表 + 单个活动的中心页。
	r.HandleFunc("/v1/miniapp/lottery/campaigns/{id}", campaigns.ServeHTTP)
	r.HandleFunc("/v1/miniapp/lottery/campaigns", campaigns.ServeHTTP)

	// 参与挂在期次下面：参与的对象是**某一期**，不是活动。活动可以同时有好几期在
	// 历史上，用户点的是「这一期还差几个人」的那个按钮。
	r.HandleFunc("/v1/miniapp/lottery/rounds/{id}/participations", rounds.ServeHTTP)

	// 我的参与 / 我的中奖。两条路径都不带 userId——「谁」来自令牌，不接受查询串。
	r.HandleFunc("/v1/miniapp/lottery/participations", participations.ServeHTTP)
	r.HandleFunc("/v1/miniapp/lottery/wins/{id}", wins.ServeHTTP)
	r.HandleFunc("/v1/miniapp/lottery/wins", wins.ServeHTTP)
}
