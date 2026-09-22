package controller

import (
	"context"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
)

// AdminMembershipController 是后台的会员接口。两棵树：套餐与会员。
//
// 它们的权限码见 routes/admin.go 的 const：套餐是**卖什么**（改了影响之后所有的成交），
// 会员是**谁**（改了影响一个人已经买到手的东西）。两件事的爆炸半径不同，所以两枚码。
type AdminMembershipController struct {
	membership *service.MembershipService
}

// NewAdminMembershipController 构造后台控制器。
func NewAdminMembershipController(s *service.MembershipService) *AdminMembershipController {
	return &AdminMembershipController{membership: s}
}

// 路径前缀。抽成常量是因为 restOf 的裁剪与 routes 里注册的那两行必须逐字一致——写成两个字面
// 量的话，改一处忘一处会让每个 id 都变成 "plans/{uuid}" 这样的串，然后所有请求都回 404。
const (
	adminPlansPath       = "/v1/admin/membership-plans"
	adminMembershipsPath = "/v1/admin/memberships"
)

// restOf 取路径里前缀之后的那一段，两端的斜杠都去掉。
//
// 与 payment-service 的同名函数逐字一致：冗余的斜杠与尾斜杠在这里被一并吃掉，路由表与
// 控制器只认「前缀 + / + 剩下的」这一种形状。
func restOf(path, prefix string) string {
	return strings.Trim(strings.TrimPrefix(path, prefix), "/")
}

// ============================================================
// 套餐
// ============================================================

// Plans 分发 /v1/admin/membership-plans 这一棵树。
//
// 两个子路径：`{id}` 本身，以及 `{id}/status`（上下架）。
//
// **上下架是独立的动作，不是 PUT 套餐时顺带改的一个字段**：合成一个的话，运营改一句描述就会
// 把一个正在售的套餐顺手存成草稿——而那一刻可能正有人在支付页上。见 dto.PlanRequest 的说明。
func (c *AdminMembershipController) Plans(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := restOf(r.URL.Path, adminPlansPath)
	id, sub, _ := strings.Cut(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listPlans(w, r)
	case rest == "" && r.Method == http.MethodPost:
		c.createPlan(w, r)
	case id != "" && sub == "" && r.Method == http.MethodGet:
		c.getPlan(w, r, id)
	case id != "" && sub == "" && r.Method == http.MethodPut:
		c.updatePlan(w, r, id)
	case id != "" && sub == "status" && r.Method == http.MethodPost:
		c.setPlanStatus(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminMembershipController) listPlans(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	plans, total, err := c.membership.ListPlans(r.Context(), dto.PlanQuery{
		Status:   strings.TrimSpace(query.Get("status")),
		Keyword:  strings.TrimSpace(query.Get("keyword")),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		writeMembershipError(w, r, err, "failed to list membership plans")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    planResponses(plans),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminMembershipController) getPlan(w http.ResponseWriter, r *http.Request, id string) {
	plan, err := c.membership.GetPlan(r.Context(), id)
	if err != nil {
		writeMembershipError(w, r, err, "failed to get membership plan")
		return
	}
	api.Success(w, planResponse(plan))
}

func (c *AdminMembershipController) createPlan(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.CreatePlanRequest
	if !decodeBody(w, r, &body) {
		return
	}
	plan, err := c.membership.CreatePlan(r.Context(), body, actor.AdminID)
	if err != nil {
		writeMembershipError(w, r, err, "failed to create membership plan")
		return
	}
	api.Success(w, planResponse(plan))
}

func (c *AdminMembershipController) updatePlan(w http.ResponseWriter, r *http.Request, id string) {
	var body dto.PlanRequest
	if !decodeBody(w, r, &body) {
		return
	}
	plan, err := c.membership.UpdatePlan(r.Context(), id, body)
	if err != nil {
		writeMembershipError(w, r, err, "failed to update membership plan")
		return
	}
	api.Success(w, planResponse(plan))
}

func (c *AdminMembershipController) setPlanStatus(w http.ResponseWriter, r *http.Request, id string) {
	var body dto.PlanStatusRequest
	if !decodeBody(w, r, &body) {
		return
	}
	plan, err := c.membership.SetPlanStatus(r.Context(), id, body.Status)
	if err != nil {
		writeMembershipError(w, r, err, "failed to set membership plan status")
		return
	}
	api.Success(w, planResponse(plan))
}

// ============================================================
// 会员
// ============================================================

// Memberships 分发 /v1/admin/memberships 这一棵树。
//
// 集合本身两个动作（列表、开通），子路径都挂在**同一个会员 ID** 上（列表给的就是它）：
//
//	{id}/freeze    暂停权益
//	{id}/unfreeze  恢复
//	{id}/revoke    撤销（不可逆）
//	{id}/expire    调整有效期
//
// 四个动作各有各的接口、各有各的权限与理由，**没有一个通用的 PATCH**：那是本域破坏力最大的
// 一类操作，做成一个能改任意字段的接口，等于把「改一句备注」与「把有效期挪到明年」放在同一
// 把钥匙下。见 dto.AdjustRequest 的说明。
//
// POST 到集合上是**开通**，不是「新建一条会员记录」这种 CRUD 语义：它补的是一个运营口子
// （客服补偿、线下活动），走的是 membership:adjust 那一枚「直接白送钱」的权限。一次付款带来
// 的开通走的是另一条路（消费 order.paid，见 service.HandleEvent），不经过这里。
func (c *AdminMembershipController) Memberships(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rest := restOf(r.URL.Path, adminMembershipsPath)
	id, sub, _ := strings.Cut(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		c.listMemberships(w, r)
	case rest == "" && r.Method == http.MethodPost:
		c.grantMembership(w, r)
	case id != "" && sub == "" && r.Method == http.MethodGet:
		c.getMembership(w, r, id)
	case id != "" && sub == "freeze" && r.Method == http.MethodPost:
		c.freezeMembership(w, r, id)
	case id != "" && sub == "unfreeze" && r.Method == http.MethodPost:
		c.unfreezeMembership(w, r, id)
	case id != "" && sub == "revoke" && r.Method == http.MethodPost:
		c.revokeMembership(w, r, id)
	case id != "" && sub == "expire" && r.Method == http.MethodPost:
		c.adjustMembershipExpiry(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (c *AdminMembershipController) listMemberships(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok := pageParams(w, r, dto.MaxPageSize)
	if !ok {
		return
	}
	query := r.URL.Query()
	autoRenew, ok := parseOptionalBool(query.Get("autoRenew"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidQuery)
		return
	}
	expireFrom, ok := parseOptionalTime(query.Get("expireFrom"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidQuery)
		return
	}
	expireTo, ok := parseOptionalTime(query.Get("expireTo"))
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidQuery)
		return
	}

	memberships, total, err := c.membership.ListMemberships(r.Context(), dto.MembershipQuery{
		UserID:     strings.TrimSpace(query.Get("userId")),
		Status:     strings.TrimSpace(query.Get("status")),
		PlanCode:   strings.TrimSpace(query.Get("planCode")),
		ExpireFrom: expireFrom,
		ExpireTo:   expireTo,
		AutoRenew:  autoRenew,
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		writeMembershipError(w, r, err, "failed to list memberships")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    c.responses(r.Context(), memberships),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// getMembership 是详情：会员本身 + 变更时间线。
//
// 时间线一次全取（见 service.ListChanges 的说明），不分页：它是详情页的一部分，跟着一条会员
// 走。一条会员的变更次数以年计，上百分页只会在界面上多出一个永远点不到的「下一页」。
func (c *AdminMembershipController) getMembership(w http.ResponseWriter, r *http.Request, id string) {
	membership, err := c.membership.GetMembershipByID(r.Context(), id)
	if err != nil {
		writeMembershipError(w, r, err, "failed to get membership")
		return
	}
	changes, err := c.membership.ListChanges(r.Context(), id)
	if err != nil {
		// 这里**不把详情一起返回**：一个没有时间线的会员详情看起来是完整的，而运营会据此
		// 以为这个人从没被改过。宁可整个请求失败。
		writeMembershipError(w, r, err, "failed to list membership changes")
		return
	}
	api.Success(w, membershipDetailResponse(membership, changes, c.membership.Now(),
		c.storeNames(r.Context(), []string{membership.StoreID})))
}

func (c *AdminMembershipController) freezeMembership(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.FreezeRequest
	if !decodeBody(w, r, &body) {
		return
	}
	membership, err := c.membership.FreezeMembership(r.Context(), id, body.Reason, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to freeze membership")
		return
	}
	api.Success(w, c.response(r.Context(), membership))
}

func (c *AdminMembershipController) unfreezeMembership(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.FreezeRequest
	if !decodeBody(w, r, &body) {
		return
	}
	membership, err := c.membership.UnfreezeMembership(r.Context(), id, body.Reason, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to unfreeze membership")
		return
	}
	api.Success(w, c.response(r.Context(), membership))
}

func (c *AdminMembershipController) revokeMembership(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.RevokeRequest
	if !decodeBody(w, r, &body) {
		return
	}
	membership, err := c.membership.RevokeMembership(r.Context(), id, body.Reason, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to revoke membership")
		return
	}
	api.Success(w, c.response(r.Context(), membership))
}

func (c *AdminMembershipController) adjustMembershipExpiry(w http.ResponseWriter, r *http.Request, id string) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.AdjustRequest
	if !decodeBody(w, r, &body) {
		return
	}
	membership, err := c.membership.AdjustExpireAt(r.Context(), id, body, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to adjust membership expiry")
		return
	}
	api.Success(w, c.response(r.Context(), membership))
}

// ============================================================
// 开通与归属门店的名字
// ============================================================

// grantMembership 后台直接开通一个会员。
//
// 它是这一棵树上**唯一一个创建**动作：其余四个都要求已经有一条会员。权限码沿用
// membership:adjust——那一枚的语义就是「直接白送钱」，而开一次会员正是这件事（见
// routes/admin.go 的注册块）。
func (c *AdminMembershipController) grantMembership(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	var body dto.GrantRequest
	if !decodeBody(w, r, &body) {
		return
	}
	membership, err := c.membership.GrantMembership(r.Context(), body, actor)
	if err != nil {
		writeMembershipError(w, r, err, "failed to grant membership")
		return
	}
	api.Success(w, c.response(r.Context(), membership))
}

// response 映射一条会员，顺手把它的归属门店名字解出来。
//
// 名字的解析放在 controller 而不是 service：它是**渲染**的一部分，只影响显示（见
// service.StoreNames——解不出来就是空格子，不该让页面打不开）。service 那层只回答会员的
// 事实，门店叫什么不是这个域的事实。
func (c *AdminMembershipController) response(ctx context.Context, membership *model.Membership) dto.MembershipResponse {
	return membershipResponse(membership, c.membership.Now(),
		c.storeNames(ctx, []string{membership.StoreID}))
}

// responses 映射一页会员。归属门店的名字**整页只解一次**（不是一行一次）。
func (c *AdminMembershipController) responses(ctx context.Context, memberships []*model.Membership) []dto.MembershipResponse {
	return membershipResponses(memberships, c.membership.Now(),
		c.storeNames(ctx, membershipStoreIDs(memberships)))
}

// storeNames 是 service.StoreNames 的转发，只为让上面两处读起来是一句话。
func (c *AdminMembershipController) storeNames(ctx context.Context, storeIDs []string) map[string]string {
	return c.membership.StoreNames(ctx, storeIDs)
}
