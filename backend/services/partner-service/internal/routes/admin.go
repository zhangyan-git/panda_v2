package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/controller"
)

// 权限码，与 migrations/identity/028_partner_admin.sql 逐字一致。
//
// 两枚，不是一枚：read 看合作方、密钥（只有掩码）与调用日志；manage 动的是**别人调用我们的
// 凭据**——签发一把新密钥、改一条 IP 白名单、停用一把钥匙，每一件都能立刻改变谁能打进来。
// 这种码的破坏半径比「看一张表」大得多，所以分开，与 023 的 manage / receipt、026 的
// payment:read / payment:manage 同一条思路。
//
// manage **不等于能看到明文密钥**：明文只在签发那一次的响应里出现，之后读接口只回掩码
// （见 controller 的 issueKey 与 dto.APIKeyItem）。manage 能做的最后一件事是**再签发一把**，
// 而不是把已有的那把读出来。
//
// 「为什么这两枚走实时授权、不读令牌里的旧 claims」写在 internal/client/admin_access.go。
const (
	permRead   = "partner:read"
	permManage = "partner:manage"
)

// methodRoute 把一个 HTTP 方法绑到它自己的权限码和处理器上。
//
// 这张表是**唯一**声明「这条路径允许什么方法」的地方：本域的 read / manage 分界不是按资源
// 而是按**方法**（同一个 /v1/admin/partners/{id} 上 GET 要 read、PUT 要 manage），没有这张
// 表的话那个分界只剩 handler 里的一句 if，而那是可以被顺手删掉的一句。留着它，加写方法的人
// 必须先在这里加一行、再想一遍自己该挂哪一枚码。
type methodRoute struct {
	permission string
	handler    http.HandlerFunc
}

// RegisterAdmin 挂载后台的开放平台路由。鉴权由调用方套在最外层，这样一个漏传校验器的调用
// 不可能变成失败开放。
//
// 传 nil 的 authenticate 时**所有**路由都回 401（见 protect），不是只跳过权限检查——
// 「忘了装配」必须表现为「谁都进不去」。
func RegisterAdmin(
	r *runtime.HTTPRouter,
	handler *controller.AdminPartnerController,
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
				// 路径存在但方法不对，按 kratos 的既有做法回 404 而不是 405（这套路由表里
				// 没有声明 Allow 的地方）。
				//
				// 这条分支是**有内容的**：密钥与本域的任何资源都没有 DELETE（密钥的「不再
				// 使用」是 status=disabled，不是删行——调用日志还指着它的 api_key_id，见
				// repository 的说明），而合作方那棵树上连 PATCH 都不在 {id} 上。验收时会拿它
				// 当「没有删除入口」的证据（404 而不是 200）。
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}

	// —— 合作方 ——
	//
	// 注册顺序从长到短（与 payment-service 的 payment-channels/{id} 那几条同一个理由）：
	// 底层按注册顺序取第一个匹配，`{id}` 那条会吃掉它后面同段数的任何模式。这里六条路径的
	// 段数各不相同（2 / 3 / 4 / 5 / 6 段），本来不会互相吃掉，但**顺序写对了才不用去想**
	// ——将来有人给 {id} 加一条子资源时，那是唯一会踩的地方。
	//
	// 两种写语义两条路径：PUT 是**整份覆盖**（没带的字段就是清空，表单是全字段提交的），
	// PATCH {id}/status 只改状态。启停单独一条不是懒——停用一家合作方会让它名下**所有**密钥
	// 当场失效，那件事不该与「顺手改个联系电话」共用一次提交。
	r.HandleFunc("/v1/admin/partners/{id}/keys/{keyId}/status", routeFor(map[string]methodRoute{
		http.MethodPatch: {permManage, handler.Partners},
	}))
	r.HandleFunc("/v1/admin/partners/{id}/keys/{keyId}", routeFor(map[string]methodRoute{
		http.MethodPut: {permManage, handler.Partners},
	}))
	r.HandleFunc("/v1/admin/partners/{id}/keys", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, handler.Partners},
		http.MethodPost: {permManage, handler.Partners},
	}))
	r.HandleFunc("/v1/admin/partners/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPatch: {permManage, handler.Partners},
	}))
	r.HandleFunc("/v1/admin/partners/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Partners},
		http.MethodPut: {permManage, handler.Partners},
	}))
	r.HandleFunc("/v1/admin/partners", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, handler.Partners},
		http.MethodPost: {permManage, handler.Partners},
	}))

	// —— 调用日志（只读） ——
	//
	// 一条平铺的路径，不嵌在合作方下面：运营排查时手上往往只有一个 api_key 的掩码或一个
	// 时间点，让他先确定是哪个合作方再查日志是反过来的。**只有 GET**——这张表是只增的，
	// 本服务没有任何一条写它的路径（见 model.CallLog）。它会随日志量增长，所以是本域唯一
	// 一条分页的读接口。
	r.HandleFunc("/v1/admin/partner-call-logs", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.CallLogs},
	}))
}
