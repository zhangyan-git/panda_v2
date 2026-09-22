package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/controller"
)

// miniappPath 是小程序会员那一棵树的根。
//
// 抽成常量是因为下面三行注册与网关里那条 `/v1/miniapp/membership*` 的转发规则必须逐字对得上
// （.env 的 MEMBERSHIP_SERVICE_URL 注释里写着那条规则）。写三个字面量的话，改一处忘一处会让
// 网关把请求转过来、而这里没有对应的路径——两侧都「看起来没问题」。
const miniappPath = "/v1/miniapp/membership"

// RegisterMiniapp 挂载小程序端的会员路由。
//
// 这一层只套认证（令牌有效），**不套权限码**：C 端的授权不是「有没有某个权限」，而是
// 「这个令牌是不是一个消费者、他只能碰自己的那一条」。后半句由 controller 的 requireUser
// 负责——路径上根本没有 user_id 参数可越权（见 controller.MiniappMembershipController 的
// 说明），权限码在这里既表达不了也没有对象。
//
// 与后台那条路不同，这里没有 routeFor：小程序的动作与路径一一对应（一条路径一个方法），
// 没有「同一个 path 上读写各要一个码」的情形。仍然是一条路径只注册一次。
func RegisterMiniapp(
	r *runtime.HTTPRouter,
	handler *controller.MiniappMembershipController,
	authenticate func(http.Handler) http.Handler,
) {
	// 装配处没给认证中间件时一律 401，而不是放行：这条路径是公开入口，
	// 任何「忘了装配」都不能变成没有身份也能读到别人的会员状态。
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

	me := protect(http.HandlerFunc(handler.Me))
	autoRenew := protect(http.HandlerFunc(handler.AutoRenew))

	// **注册顺序：/auto-renew 必须在 "" 之前**。gorilla/mux 取第一个匹配，顺序反了的话
	// /v1/miniapp/membership/auto-renew 会先匹配上根路径那条，控制器把 rest 读成 "auto-renew"
	// 之后落进 default 分支，回一个 404——一个看着像「这个接口没做」的错法。
	r.HandleFunc(miniappPath+"/auto-renew", autoRenew.ServeHTTP)
	// 套餐列表在**同一棵树**上（会员页要的东西都在这儿），但它不套身份以外的东西——它不需要
	// 登录态里的任何信息，甚至不需要登录。仍然挂在 protect 后面：这一棵树整体是 C 端入口，
	// 单独为它开一条不鉴权的路径，日后加一个字段就会忘了它没有闸门。
	r.HandleFunc(miniappPath+"/plans", protect(http.HandlerFunc(handler.Plans)).ServeHTTP)
	// 连续包月的签约。两条路径，同一条链的两端：
	//
	//	POST /subscriptions          发起（回跳转微信的签名参数）
	//	POST /subscriptions/confirm  用户回来之后回渠道核一次
	//
	// **两条都必须挂在 protect 后面**：签约签的是「令牌里那个人的」代扣授权，没有身份就没有
	// 当事人。注册顺序上长路径在前，与这一棵树其余几行同一个写法。
	r.HandleFunc(miniappPath+"/subscriptions/confirm", protect(http.HandlerFunc(handler.ConfirmSubscription)).ServeHTTP)
	// 发起签约是这一棵树上**唯一一个会在渠道那边留下东西的接口**（付款授权）。它比另外几个
	// 都危险一点，所以尤其不能有第二条入口：这条链上「谁在签」只认令牌，客户端能影响的只有
	// 「签哪一款套餐」与那个幂等号。
	r.HandleFunc(miniappPath+"/subscriptions", protect(http.HandlerFunc(handler.Subscribe)).ServeHTTP)

	// 店铺码活动的领取。**它比这一棵树上的其余接口都危险一点**：另外三个只读自己的那一条，
	// 而这个是写——所以它同样只认令牌里的身份，scene 决定发什么，客户端没有别的可影响的东西。
	r.HandleFunc(miniappPath+"/campaigns/claim", protect(http.HandlerFunc(handler.ClaimCampaign)).ServeHTTP)
	r.HandleFunc(miniappPath, me.ServeHTTP)
}
