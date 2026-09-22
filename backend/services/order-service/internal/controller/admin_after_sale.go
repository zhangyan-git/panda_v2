package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// AdminAfterSaleController 是后台的售后接口。认证与权限由装配处
// （routes.RegisterAdmin）加在每一条路由上。
type AdminAfterSaleController struct{ orders *service.OrderService }

const adminAfterSalePath = "/v1/admin/after-sales"

func NewAdminAfterSaleController(orders *service.OrderService) *AdminAfterSaleController {
	return &AdminAfterSaleController{orders: orders}
}

// AfterSales 分发 /v1/admin/after-sales 这一棵路径。
//
// 与 AdminOrderController.Orders 同一套写法与同一个理由（见那边的注释）：路径参数只有
// 一层 {afterSaleNo}，用 Path 的剩余部分判分支比让 mux 传参更直接，注册顺序也仍然要
// 从长到短。
func (c *AdminAfterSaleController) AfterSales(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminAfterSalePath), "/")
	switch {
	case rest == "":
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		c.list(w, r)
	case strings.HasSuffix(rest, "/approve") && r.Method == http.MethodPost:
		c.review(w, r, strings.TrimSuffix(rest, "/approve"), model.AfterSaleActionApprove, identity)
	case strings.HasSuffix(rest, "/reject") && r.Method == http.MethodPost:
		c.review(w, r, strings.TrimSuffix(rest, "/reject"), model.AfterSaleActionReject, identity)
	case strings.HasSuffix(rest, "/refund") && r.Method == http.MethodPost:
		c.startRefund(w, r, strings.TrimSuffix(rest, "/refund"), identity)
	default:
		// 没有后台详情接口：列表返回的就是完整售后单（含凭证与福卡快照），点开只是为了看
		// 同一份数据。等后台页面落地、真需要「详情里带订单行与出资分摊」时再加。
		http.NotFound(w, r)
	}
}

func (c *AdminAfterSaleController) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	// 状态与范围都是枚举，写错一个字母会静默返回空列表——调用方会以为「这个状态下没有
	// 申请」，而不是「我筛错了」。先挡一下。
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isKnownAfterSaleStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "status is invalid")
		return
	}
	scope := strings.TrimSpace(query.Get("scope"))
	if scope != "" && !isKnownAfterSaleScope(scope) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "scope is invalid")
		return
	}

	filter := repository.AfterSaleFilter{
		AfterSaleNo: strings.TrimSpace(query.Get("afterSaleNo")),
		OrderNo:     strings.TrimSpace(query.Get("orderNo")),
		UserID:      strings.TrimSpace(query.Get("userId")),
		Status:      status,
		Scope:       scope,
		Page:        page,
		PageSize:    pageSize,
	}
	var err error
	if filter.CreatedFrom, err = parseTimeParam(query.Get("createdFrom")); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "createdFrom must be an RFC3339 timestamp")
		return
	}
	if filter.CreatedTo, err = parseTimeParam(query.Get("createdTo")); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "createdTo must be an RFC3339 timestamp")
		return
	}

	rows, total, err := c.orders.ListAfterSales(r.Context(), filter)
	if err != nil {
		writeOrderError(w, err, "failed to list after sales")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    afterSaleViews(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

// review 处理一次审核：通过或驳回。
//
// 两个动作走同一个 handler，动作写死在路径上（approve / reject），不放进请求体：这样
// 「审了什么」在访问日志和权限注解里就看得见，而请求体里的一个字段是看不见的。
func (c *AdminAfterSaleController) review(w http.ResponseWriter, r *http.Request, afterSaleNo, action string, identity auth.Identity) {
	var body dto.ReviewAfterSaleRequest
	// 空体是合法的（通过、不写备注，且订单没有福卡时只需带 FortuneCardUnusedConfirmed），
	// io.EOF 就是「没带体」；解出别的错才是请求坏了。
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	// 审核人取自令牌，不取自请求体：让被审计的人自己填审计字段，等于没有审计。
	reviewedBy := identity.UserID
	if reviewedBy == "" {
		reviewedBy = identity.Subject
	}
	row, err := c.orders.ReviewAfterSale(r.Context(), service.ReviewAfterSaleInput{
		AfterSaleNo:                afterSaleNo,
		Action:                     action,
		Remark:                     body.Remark,
		FortuneCardUnusedConfirmed: body.FortuneCardUnusedConfirmed,
		ReviewedBy:                 reviewedBy,
		TraceID:                    traceID(r),
	})
	if err != nil {
		writeOrderError(w, err, "failed to review after sale")
		return
	}
	api.Success(w, afterSaleView(row))
}

// startRefund 是**重试出口**：把一张已审核通过、但退款单没建起来的售后单再推一次。
//
// 它为什么是一个独立入口而不是「再点一次通过」：审核是一次决定，只该发生一次；而发起退款是
// 那个决定的**执行**，它可以重试任意多次（幂等键是售后单号，重试不会退两次钱）。合成一个
// 的话，审核人点第二次会拿到「这张单已经不在待审核状态」，那是个死胡同——单停在那儿，
// 没有任何动作能把它推下去。
//
// 没有请求体：要退多少、退哪一张支付单，全都记在售后单上了。
func (c *AdminAfterSaleController) startRefund(w http.ResponseWriter, r *http.Request, afterSaleNo string, identity auth.Identity) {
	actor := identity.UserID
	if actor == "" {
		actor = identity.Subject
	}
	row, err := c.orders.StartRefund(r.Context(), service.StartRefundInput{
		AfterSaleNo: afterSaleNo,
		ActorID:     actor,
		TraceID:     traceID(r),
	})
	if err != nil {
		writeOrderError(w, err, "failed to start the refund")
		return
	}
	api.Success(w, afterSaleView(row))
}

// isKnownAfterSaleStatus 挡枚举写错。名单与 order_after_sales.status 的 CHECK 逐字一致。
func isKnownAfterSaleStatus(status string) bool {
	switch status {
	case model.AfterSaleStatusPending, model.AfterSaleStatusApproved, model.AfterSaleStatusRejected,
		model.AfterSaleStatusRefunding, model.AfterSaleStatusRefunded, model.AfterSaleStatusFailed,
		model.AfterSaleStatusCancelled:
		return true
	default:
		return false
	}
}

func isKnownAfterSaleScope(scope string) bool {
	switch scope {
	case model.AfterSaleScopeAll, model.AfterSaleScopeDrink, model.AfterSaleScopeAddon,
		model.AfterSaleScopeMembership:
		return true
	default:
		return false
	}
}
