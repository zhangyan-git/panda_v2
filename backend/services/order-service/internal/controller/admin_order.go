package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// AdminOrderController 是后台的订单接口。它挂在 adminOrderPath 这一棵路径下，
// 认证与权限由装配处（routes.RegisterAdmin）加在每一个路由上。
type AdminOrderController struct{ orders *service.OrderService }

const adminOrderPath = "/v1/admin/orders"

func NewAdminOrderController(orders *service.OrderService) *AdminOrderController {
	return &AdminOrderController{orders: orders}
}

// Orders 分发 /v1/admin/orders 这一棵路径。
//
// 一个 handler 配多条路由（列表 / 详情 / 取消）而不是每个动作一个方法：路径参数只有
// 一层 {id}，用 URL.Path 的剩余部分判分支比让 mux 传参更直接，也和 coupon-service 的
// 写法一致——那边已经踩过「mux 的 {id} 只吃一个路径段」的坑，注册顺序稍有出入就会
// 把 /{id}/cancel 解析成 id="cancel"。
func (c *AdminOrderController) Orders(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, adminOrderPath), "/")
	switch {
	case rest == "":
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		c.list(w, r)
	case strings.HasSuffix(rest, "/cancel"):
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		c.cancel(w, r, strings.TrimSuffix(rest, "/cancel"), identity)
	default:
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		c.detail(w, r, rest)
	}
}

func (c *AdminOrderController) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isKnownOrderStatus(status) {
		// 状态是枚举，写错一个字母就会静默返回空列表——调用方会以为「这个状态下没有订单」，
		// 而不是「我筛错了」。这里先挡一下。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "status is invalid")
		return
	}
	source := strings.TrimSpace(query.Get("source"))
	if source != "" && source != model.SourceMiniapp && source != model.SourceScreenQR {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "source is invalid")
		return
	}
	filter := repository.OrderFilter{
		UserID:   strings.TrimSpace(query.Get("userId")),
		OrderNo:  strings.TrimSpace(query.Get("orderNo")),
		Status:   status,
		Source:   source,
		StoreID:  strings.TrimSpace(query.Get("storeId")),
		DeviceID: strings.TrimSpace(query.Get("deviceId")),
		Page:     page,
		PageSize: pageSize,
	}
	// 三个行类型筛选：不传 = 不筛，true/1 = 要有，false/0 = 要没有。写错值要报错而不是当成
	// 「不筛」——静默忽略参数，前端会以为自己筛过了，看到的却是全量。
	if filter.HasDrink, message = parseLinePresence("hasDrink", query.Get("hasDrink")); message != "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	if filter.HasAddon, message = parseLinePresence("hasAddon", query.Get("hasAddon")); message != "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	if filter.HasMembership, message = parseLinePresence("hasMembership", query.Get("hasMembership")); message != "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
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

	rows, total, err := c.orders.ListOrders(r.Context(), filter, "")
	if err != nil {
		writeOrderError(w, err, "failed to list orders")
		return
	}
	api.Success(w, api.PageResponse{
		Items:    orderSummaryResponses(rows),
		Total:    int64(total),
		Page:     page,
		PageSize: pageSize,
	})
}

func (c *AdminOrderController) detail(w http.ResponseWriter, r *http.Request, orderID string) {
	// userID 传空串：后台查询不校验归属，也不返回取杯码（service 层决定的）。
	detail, err := c.orders.GetOrderDetail(r.Context(), orderID, "")
	if err != nil {
		writeOrderError(w, err, "failed to get order")
		return
	}
	api.Success(w, orderDetailResponse(detail))
}

func (c *AdminOrderController) cancel(w http.ResponseWriter, r *http.Request, orderID string, identity auth.Identity) {
	var body dto.CancelOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	// 取消人取自令牌，不取自请求体：让被审计的人自己填审计字段，等于没有审计。
	actorID := identity.UserID
	if actorID == "" {
		actorID = identity.Subject
	}
	result, err := c.orders.CancelOrder(r.Context(), service.CancelOrderInput{
		OrderID:   orderID,
		Reason:    strings.TrimSpace(body.Reason),
		ActorType: model.ActorAdmin,
		ActorID:   &actorID,
		TraceID:   traceID(r),
	})
	if err != nil {
		writeOrderError(w, err, "failed to cancel order")
		return
	}
	api.Success(w, map[string]any{
		"orderId": result.OrderID,
		"status":  result.Status,
	})
}

// parseLinePresence 解析「有没有某类行」这类三态筛选参数：不传 = 不筛，true/1 = 要有，
// false/0 = 要没有。第二个返回值是错误文案，空串表示解析成功（同 api.ParsePage 的写法）。
//
// 返回 nil 表示不筛，所以调用方不能把返回值直接当布尔用：`*filter.HasDrink` 之前必须先判
// 非 nil。这和 dto/OrderQuery 里那三个 *bool 是同一件事，也是「不筛」与「筛：没有」唯一能
// 区分开的表达方式。
func parseLinePresence(key, raw string) (*bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	switch strings.ToLower(raw) {
	case "true", "1":
		value := true
		return &value, ""
	case "false", "0":
		value := false
		return &value, ""
	default:
		return nil, key + " must be true or false"
	}
}

func isKnownOrderStatus(status string) bool {
	switch status {
	case model.OrderStatusPendingPayment, model.OrderStatusPaid, model.OrderStatusCompleted,
		model.OrderStatusCancelled, model.OrderStatusExpired, model.OrderStatusRefunding,
		model.OrderStatusRefunded:
		return true
	default:
		return false
	}
}

// parseTimeParam 解析时间筛选参数，空串表示不筛。
//
// 只收 RFC3339（带时区的），不收 "2006-01-02"：日期字符串没有时区，而「今天」是业务
// 时区的今天。前端把「今天」换算成带时区的起止时刻再传过来，是唯一不会在跨时区部署时
// 悄悄错一天的做法。
func parseTimeParam(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
