package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/controller"
)

// 权限码。三个分开，与 migrations/identity/025_membership_admin.sql 逐字一致。
//
//   - read    —— 看套餐、看会员列表与详情、看变更流水。这一档迟早要发给客服：用户问「我是不是
//     会员、什么时候到期、为什么被冻了」，答这三句只需要它。它一次账都动不了。
//   - manage  —— 建改套餐、上下架。改的是**接下来卖什么**：改一个价格，之后每一笔成交都跟着变；
//     但它一个已经卖出去的会员都动不了——memberships 上存着成交快照（plan_code / plan_name /
//     member_price_mode 那一组），套餐再改也不追溯改变已购会员的权益。这是本域唯一一处
//     「改了也不用怕」的写入口。
//   - adjust  —— 冻结 / 解冻 / 撤销 / 直接改有效期。**这一枚直接白送钱**：把一个人的 expire_at
//     往后挪一年，等于送他一年会员价，没有任何订单、支付、流水跟着发生。
//
// 把 adjust 并进 manage 是最容易犯的错：那等于让任何一个能改套餐文案的人也能给人加一年会员。
// 025 的注释里把这条写死了。
const (
	permRead   = "membership:read"
	permManage = "membership:manage"
	permAdjust = "membership:adjust"
)

// methodRoute 把一个 HTTP 方法绑到它自己的权限码和处理器上。
type methodRoute struct {
	permission string
	handler    http.HandlerFunc
}

// RegisterAdmin 挂载后台的会员路由。鉴权由调用方套在最外层，这样一个漏传校验器的调用
// 不可能变成失败开放。
//
// 传 nil 的 authenticate 时**所有**路由都回 401（见 protect），不是只跳过权限检查——
// 「忘了装配」必须表现为「谁都进不去」。
func RegisterAdmin(
	r *runtime.HTTPRouter,
	handler *controller.AdminMembershipController,
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

	// —— 套餐 ——
	//
	// 路径上的资源是**卖什么**。改与建用 manage，看用 read。
	//
	// **注册顺序：{id}/status 必须在 {id} 之前**。gorilla/mux 取第一个匹配，顺序反了的话
	// /membership-plans/{uuid}/status 会先匹配上 {id} 那条，被 `id = "<uuid>/status"` 解析成
	// 一个不存在的套餐——一个看着像「路由没挂上」的错法。控制器那边的 switch 也排了同一个序，
	// **两处都要对**（mux 先筛了一遍）。
	//
	// **没有 DELETE**：套餐可以下架，不能删除。一个码一旦卖过，memberships 的快照列上就留着
	// 它的编码，删掉定义会让「这个人当初买的是什么」在后台变成一个查不到的名字。不用了就停用。
	r.HandleFunc("/v1/admin/membership-plans", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, handler.Plans},
		http.MethodPost: {permManage, handler.Plans},
	}))
	r.HandleFunc("/v1/admin/membership-plans/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPost: {permManage, handler.Plans},
	}))
	r.HandleFunc("/v1/admin/membership-plans/{id}", routeFor(map[string]methodRoute{
		// 修改用 PUT 而不是 PATCH：请求体是**整份**套餐（见 dto.PlanRequest），校验也整份跑。
		// 一个只发一半字段的 PATCH 会被当成「把没发的字段清空了」，那正好是它不该有的语义。
		http.MethodGet: {permRead, handler.Plans},
		http.MethodPut: {permManage, handler.Plans},
	}))

	// —— 会员 ——
	//
	// 路径上的资源是**谁**。看用 read，开通与四个动作一律 adjust。
	//
	// **注册顺序同上：四个动作必须在 {id} 之前**。
	//
	// 四个动作用的是四个不同的动词，**没有一个通用的 PATCH**：合成一个能改任意字段的接口，
	// 等于把「改一句备注」与「把有效期挪到明年」放在同一把钥匙下（见 dto.AdjustRequest）。
	// 它们也因此全是 POST 而不是 PUT——这里改的不是「这条记录的完整形状」，而是「发生了一件事」。
	r.HandleFunc("/v1/admin/memberships", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Memberships},
		// POST 到集合上是**开通**（客服补偿、线下活动），用的也是 adjust：那一枚的语义就是
		// 「直接白送钱」，而开一次会员正是这件事，所以**不新增权限码**（migrations/identity/025
		// 一个字不改）。它不是 manage——manage 管的是「接下来卖什么」，改一个在售套餐动不了
		// 任何人的会员。
		http.MethodPost: {permAdjust, handler.Memberships},
	}))
	r.HandleFunc("/v1/admin/memberships/{id}/freeze", routeFor(map[string]methodRoute{
		http.MethodPost: {permAdjust, handler.Memberships},
	}))
	r.HandleFunc("/v1/admin/memberships/{id}/unfreeze", routeFor(map[string]methodRoute{
		http.MethodPost: {permAdjust, handler.Memberships},
	}))
	r.HandleFunc("/v1/admin/memberships/{id}/revoke", routeFor(map[string]methodRoute{
		http.MethodPost: {permAdjust, handler.Memberships},
	}))
	r.HandleFunc("/v1/admin/memberships/{id}/expire", routeFor(map[string]methodRoute{
		http.MethodPost: {permAdjust, handler.Memberships},
	}))
	r.HandleFunc("/v1/admin/memberships/{id}", routeFor(map[string]methodRoute{
		// 只读。**这里不会有 PATCH**：会员的每一个字段要么是成交带进来的（快照列、有效期），
		// 要么由用户自己开关（auto_renew），要么由上面五个动作改（四个状态动作 + 集合上的开通）。
		http.MethodGet: {permRead, handler.Memberships},
	}))

	// —— 连续包月订阅 ——
	//
	// 与上面两棵树不同，这一棵**没有创建**：订阅只能由小程序端签约产生（发起与确认都在
	// /v1/miniapp/membership/subscriptions），后台连建都建不了——老后台也一样。所以只有
	// 「看」「同步」和「取消」。
	//
	// 注册顺序同上：**stats 与 {id}/sync、{id}/cancel 必须在 {id} 之前**，否则 "stats" 会被当成
	// 一个订阅 ID 去解析（表现为一条说得通但错的 404）。控制器那边的 switch 排了同一个序。
	r.HandleFunc("/v1/admin/membership-subscriptions", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Subscriptions},
	}))
	r.HandleFunc("/v1/admin/membership-subscriptions/stats", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Subscriptions},
	}))
	r.HandleFunc("/v1/admin/membership-subscriptions/{id}/sync", routeFor(map[string]methodRoute{
		// 同步用 manage，与取消同一枚：它是「回渠道核一次、按渠道的结论纠正本地」，不涉及任何
		// 金额也不改会员有效期。它写审计（谁点的），但**不产生财产后果**——把一条订阅从
		// pending_sign 变成 active 只是让「以后可以扣款」这件事在本地成立，扣款本身还没有。
		http.MethodPost: {permManage, handler.Subscriptions},
	}))
	r.HandleFunc("/v1/admin/membership-subscriptions/{id}/cancel", routeFor(map[string]methodRoute{
		// 取消用 manage 而不是 adjust：adjust 那一枚是「直接白送钱」（改有效期、开通），
		// 而取消是把一条自动续费掐掉、不涉及任何金额也不改会员有效期。它与「接下来卖什么」
		// 同属配置与管理，是 manage。
		http.MethodPost: {permManage, handler.Subscriptions},
	}))
	r.HandleFunc("/v1/admin/membership-subscriptions/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Subscriptions},
	}))

	// —— 店铺码会员活动 ——
	//
	// 权限码**不照抄老系统**（那边用券模板的 `coupon:template:manage`，是权宜之计）：活动配置
	// 是会员域的营销配置，用本域已有那两枚。看用 read，增删改与启停一律 manage——它不改任何人
	// 的会员有效期，只决定「接下来送什么」，与套餐管理同一类。
	//
	// **没有 /qrcode**：生成小程序码要走微信的 wxacode.getUnlimited，appid/secret 与 token 缓存
	// 都在 user-service，本服务没有任何微信配置；而且码的唯一用途是被扫、扫码入口在小程序端。
	// 见 service/campaign.go 的开头。
	//
	// 注册顺序同上：**{id}/claims 与 {id}/status 必须在 {id} 之前**，否则 "claims" 会被当成一个
	// 活动 ID 去解析（表现为一条说得通但错的 404）。控制器那边的 switch 排了同一个序。
	r.HandleFunc("/v1/admin/membership-campaigns", routeFor(map[string]methodRoute{
		http.MethodGet:  {permRead, handler.Campaigns},
		http.MethodPost: {permManage, handler.Campaigns},
	}))
	r.HandleFunc("/v1/admin/membership-campaigns/{id}/status", routeFor(map[string]methodRoute{
		http.MethodPost: {permManage, handler.Campaigns},
	}))
	r.HandleFunc("/v1/admin/membership-campaigns/{id}/claims", routeFor(map[string]methodRoute{
		// 领取记录是**用户的行为数据**，不是配置：看它只需要 read。今天它一定是空的（领取动作
		// 在小程序端，那一端还没接）。
		http.MethodGet: {permRead, handler.Campaigns},
	}))
	r.HandleFunc("/v1/admin/membership-campaigns/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Campaigns},
		http.MethodPut: {permManage, handler.Campaigns},
	}))
}
