package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/controller"
)

// 权限码。三个分开，与 migrations/identity/022_lottery_admin.sql 逐字一致。
//
// 分开的理由不是「细一点更好」，是这三件事的爆炸半径完全不同：
//
//   - read   —— 看活动、看期次、看中奖名单。泄露的是运营数据。
//   - manage —— 建/改活动、改奖池、开关门店。改奖池就是改**将来中奖的名额与奖品**，
//     下一次开奖按它发。它今天不直接动人资产，明天就会（奖池里的券要发出去）。
//   - draw   —— 人工开奖与作废。全系统唯一一个能凭空决定「谁中奖」的动作，所以它
//     只绑 super_admin，且权限码单列（见 022 的文件头：本轮没有审批流，所以只能收窄）。
//
// 把 draw 并进 manage 是最容易犯的错：那等于让任何一个能改活动名字的人也能决定中奖名单。
const (
	permRead   = "lottery:read"
	permManage = "lottery:manage"
	permDraw   = "lottery:draw"
)

// methodRoute 把一个 HTTP 方法绑到它自己的权限码和处理器上。
type methodRoute struct {
	permission string
	handler    http.HandlerFunc
}

// RegisterAdmin 挂载后台的抽奖路由。鉴权由调用方套在最外层，这样一个漏传校验器的调用
// 不可能变成失败开放。
//
// 传 nil 的 authenticate 时**所有**路由都回 401（见 protect），不是只跳过权限检查——
// 「忘了装配」必须表现为「谁都进不去」。
func RegisterAdmin(
	r *runtime.HTTPRouter,
	handler *controller.AdminLotteryController,
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

	// —— 开通 ——
	//
	// 开通是整个域的第一道动作：它建出的默认活动与第一期，是这家店抽奖的全部起点。
	// 它用 manage 而不是 write 之外的某个码——它与「再建一个活动」是同一类事。
	r.HandleFunc("/v1/admin/lottery/activations", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, handler.Activations},
		http.MethodPost: {permManage, handler.Activations},
	}))
	r.HandleFunc("/v1/admin/lottery/activations/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPost: {permManage, handler.Activations},
	}))
	r.HandleFunc("/v1/admin/lottery/activations/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Activations},
	}))

	// —— 活动与奖池 ——
	//
	// 奖池跟着活动走（创建 / 修改活动时整份替换），所以 /prizes 只有读。给它单独开一个
	// 写接口等于让奖池能在活动之外被改，而名额总数正是开奖要用的那个数。
	r.HandleFunc("/v1/admin/lottery/campaigns", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, handler.Campaigns},
		http.MethodPost: {permManage, handler.Campaigns},
	}))
	r.HandleFunc("/v1/admin/lottery/campaigns/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPost: {permManage, handler.Campaigns},
	}))
	r.HandleFunc("/v1/admin/lottery/campaigns/{id}/prizes", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Campaigns},
	}))
	r.HandleFunc("/v1/admin/lottery/campaigns/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Campaigns},
		http.MethodPut: {permManage, handler.Campaigns},
	}))

	// —— 期次、开奖、作废 ——
	//
	// 期次是滚出来的，没有「建一期」的接口；能动的只有开奖与作废，两个都用 draw。
	r.HandleFunc("/v1/admin/lottery/rounds", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Rounds},
	}))
	r.HandleFunc("/v1/admin/lottery/rounds/{id}/draw", routeFor(map[string]methodRoute{
		http.MethodPost: {permDraw, handler.Rounds},
	}))
	r.HandleFunc("/v1/admin/lottery/rounds/{id}/cancel", routeFor(map[string]methodRoute{
		http.MethodPost: {permDraw, handler.Rounds},
	}))
	r.HandleFunc("/v1/admin/lottery/rounds/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Rounds},
	}))

	// —— 开奖记录 ——
	//
	// 只读，且只增表。它存的是「这一次开奖用了什么种子、开了谁」，改它等于改历史。
	r.HandleFunc("/v1/admin/lottery/draws/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Draws},
	}))

	// —— 中奖 ——
	//
	// 本轮只有读。将来核销那一轮的写入口是商户端（/v1/merchant/lottery/...），不是这里：
	// 后台替用户核销要单独设计（谁在什么情况下能替核销），不该顺手挂在 manage 上。
	r.HandleFunc("/v1/admin/lottery/wins", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Wins},
	}))
	r.HandleFunc("/v1/admin/lottery/wins/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Wins},
	}))

	// —— 参与（只读） ——
	//
	// 后台不提供「替用户参与」：参与要扣用户的福卡，而福卡只能由用户自己花。
	r.HandleFunc("/v1/admin/lottery/participations", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Participations},
	}))
}
