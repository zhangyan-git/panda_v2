package controller

import (
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
)

// 连续包月订阅的后台接口。单独一个文件，因为它是**另一棵树**：路径前缀与会员那棵树不同
// （/v1/admin/membership-subscriptions），权限码也不同——只读三枚用 membership:read，
// 取消用 membership:manage（见 routes/admin.go 的注册块）。
const adminSubscriptionsPath = "/v1/admin/membership-subscriptions"

// Subscriptions 分发 /v1/admin/membership-subscriptions 这一棵树。
//
//	GET    ""            列表
//	GET    "stats"       两张统计卡
//	GET    "{id}"        一条订阅（详情抽屉），另带协议号、续费明细、首月支付信息三块回显
//	POST   "{id}/sync"   同步（回渠道核协议，按渠道的结论纠正本地）
//	POST   "{id}/cancel" 取消（只改本地状态，见 service/subscription.go）
//
// **没有 POST 到集合上**：订阅只能由小程序签约产生（发起与确认都在 /v1/miniapp 那一棵树），
// 后台连建都建不了——老后台也一样。
//
// `stats` 必须先于 `{id}` 判：它是固定字面量，掉进 `{id}` 那一支会拿 "stats" 去当 UUID 解析，
// 结果是一个「这条包月订阅不存在」的 404——一个能跑但说不通的答案。
func (c *AdminMembershipController) Subscriptions(w http.ResponseWriter, r *http.Request) {
	rest := restOf(r.URL.Path, adminSubscriptionsPath)
	id, sub, _ := strings.Cut(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listSubscriptions(w, r)
	case rest == "stats" && r.Method == http.MethodGet:
		c.subscriptionStats(w, r)
	case id != "" && sub == "" && r.Method == http.MethodGet:
		c.getSubscription(w, r, id)
	case id != "" && sub == "sync" && r.Method == http.MethodPost:
		c.syncSubscription(w, r, id)
	case id != "" && sub == "cancel" && r.Method == http.MethodPost:
		c.cancelSubscription(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminMembershipController) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	subscriptions, total, err := c.membership.ListSubscriptions(r.Context(), dto.SubscriptionQuery{
		UserID:   strings.TrimSpace(query.Get("userId")),
		Status:   strings.TrimSpace(query.Get("status")),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeMembershipError(w, r, err, "failed to list subscriptions")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    subscriptions,
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// subscriptionStats 是列表页头上那两张卡。**单独一个请求**而不是跟着列表一起返回：
// 翻页时那两个数与当前页无关，跟页面走会让第一页之后每次都白算一遍全表（老系统就是把这个
// 数塞在列表响应里的）。
func (c *AdminMembershipController) subscriptionStats(w http.ResponseWriter, r *http.Request) {
	stats, err := c.membership.SubscriptionStats(r.Context())
	if err != nil {
		writeMembershipError(w, r, err, "failed to load subscription stats")
		return
	}
	api.Success(w, stats)
}

func (c *AdminMembershipController) getSubscription(w http.ResponseWriter, r *http.Request, id string) {
	subscription, err := c.membership.GetSubscription(r.Context(), id)
	if err != nil {
		writeMembershipError(w, r, err, "failed to get subscription")
		return
	}
	api.Success(w, subscription)
}

// syncSubscription 是后台的「同步」：回渠道核一次协议，按渠道的结论纠正本地订阅。
//
// **请求体是空的**（没有 decodeBody）：这个动作没有一个字段要运营填——协议号取自订阅那一行，
// 结论取自渠道。与 cancel 那条一样要操作人：它会写审计。
func (c *AdminMembershipController) syncSubscription(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	result, err := c.membership.SyncSubscription(r.Context(), id, actor, requestID(r), traceID(r))
	if err != nil {
		writeMembershipError(w, r, err, "failed to sync subscription")
		return
	}
	api.Success(w, result)
}

// cancelSubscription 取消一条订阅。
//
// 它是这棵树上**唯一一个写动作**，也是唯一需要操作人的地方——审计要记是谁掐掉的（见
// repository.CancelSubscription）。读那三个不读身份，与会员列表那几条一致。
func (c *AdminMembershipController) cancelSubscription(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.CancelSubscriptionRequest
	if !decodeBody(w, r, &body) {
		return
	}
	subscription, err := c.membership.CancelSubscription(r.Context(), id, body, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to cancel subscription")
		return
	}
	api.Success(w, subscription)
}
