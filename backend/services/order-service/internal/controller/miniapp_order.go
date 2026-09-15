package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// MiniappOrderController 是小程序端（C 端）的订单接口。
type MiniappOrderController struct{ orders *service.OrderService }

const miniappOrderPath = "/v1/miniapp/orders"

func NewMiniappOrderController(orders *service.OrderService) *MiniappOrderController {
	return &MiniappOrderController{orders: orders}
}

// Orders 分发 /v1/miniapp/orders 这一棵路径。每个动作执行前都要过 requireConsumer。
func (c *MiniappOrderController) Orders(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, miniappOrderPath), "/")
	switch {
	case rest == "" && r.Method == http.MethodPost:
		c.create(w, r, userID)
	case rest == "" && r.Method == http.MethodGet:
		c.list(w, r, userID)
	case strings.HasSuffix(rest, "/cancel") && r.Method == http.MethodPost:
		c.cancel(w, r, userID, strings.TrimSuffix(rest, "/cancel"))
	// 发起支付。这里的 {orderNo} 与上面几个动作的 {id} 不是同一个键：支付这条链路的两端
	// 手上只有订单号（见 miniapp_pay.go）。
	case strings.HasSuffix(rest, "/pay") && r.Method == http.MethodPost:
		c.pay(w, r, userID, strings.TrimSuffix(rest, "/pay"))
	case strings.HasSuffix(rest, "/after-sales") && r.Method == http.MethodPost:
		c.applyAfterSale(w, r, userID, strings.TrimSuffix(rest, "/after-sales"))
	case r.Method == http.MethodGet:
		c.detail(w, r, userID, rest)
	default:
		http.NotFound(w, r)
	}
}

// requireConsumer 是 C 端入口的闸门，返回调用者的用户 ID；失败时已经写好了响应。
//
// 判定必须落在 realm 上，不能只看 subject == user_id：平台管理员和商户账号的令牌同样
// 满足那两条，只查它们等于把订单接口对管理端敞开——那不只是越权，还会让订单挂在一个
// 根本不是消费者的 user_id 上。缺失和域不对分成 401 与 403：前者是没登录，后者是拿错了
// 身份的令牌，客户端要做的处理不同。这一段与 user-service 的 requireConsumer 是同一套
// 判定，两边必须保持一致，否则同一种令牌在两个服务上会得到不同的回答。
//
// 为什么 subject 要与 user_id 一致：user_id 是订单的归属字段，subject 是令牌的主体。
// 两者不一致的令牌要么是伪造的，要么是我们自己签错了，两种都不该产生订单。
func requireConsumer(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" || identity.Subject != identity.UserID {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
		return "", false
	}
	if identity.Realm != auth.RealmConsumer {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "非小程序用户")
		return "", false
	}
	return identity.UserID, true
}

func (c *MiniappOrderController) create(w http.ResponseWriter, r *http.Request, userID string) {
	// 幂等键从请求头来，不从请求体来：它是「这一次提交」的标识，不是订单的内容，
	// 混进请求体就会被当成订单字段一起做哈希、一起校验。
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is required")
		return
	}
	var body dto.CreateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	response, replayed, err := c.orders.CreateOrder(r.Context(), service.CreateOrderInput{
		UserID:         userID,
		IdempotencyKey: idempotencyKey,
		TraceID:        traceID(r),
		Request:        body,
	})
	if err != nil {
		writeOrderError(w, err, "failed to create order")
		return
	}
	if replayed {
		// 重放的是同一次下单：回 200 而不是 201。调用方据此知道「没有再落一单」，
		// 前端也才不会把它当成一次新的下单去跳支付。
		api.Success(w, response)
		return
	}
	api.Created(w, response)
}

// applyAfterSale 受理一次退款申请。
//
// 挂在订单这棵树上（/orders/{id}/after-sales）而不是售后树：申请的对象是**这一单**，
// 而售后单号是申请之后才有的东西。
func (c *MiniappOrderController) applyAfterSale(w http.ResponseWriter, r *http.Request, userID, orderID string) {
	// 幂等键从请求头来，理由同下单：它是「这一次提交」的标识，不是申请的内容。
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is required")
		return
	}
	var body dto.ApplyAfterSaleRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	row, replayed, err := c.orders.ApplyAfterSale(r.Context(), service.ApplyAfterSaleInput{
		OrderID:        orderID,
		UserID:         userID,
		IdempotencyKey: idempotencyKey,
		Scope:          body.Scope,
		OrderLineID:    body.OrderLineID,
		Reason:         body.Reason,
		Images:         body.Images,
		TraceID:        traceID(r),
		ActorID:        &userID,
	})
	if err != nil {
		writeOrderError(w, err, "failed to apply after sale")
		return
	}
	// 重放的是同一次申请：回 200 而不是 201，理由同下单——调用方据此知道没有再落一张单，
	// 前端也才不会把这笔已经提交过的退款当成一次新的申请再走一遍。
	if replayed {
		api.Success(w, afterSaleView(row))
		return
	}
	api.Created(w, afterSaleView(row))
}

func (c *MiniappOrderController) list(w http.ResponseWriter, r *http.Request, userID string) {
	query := r.URL.Query()
	page, pageSize, ok, message := api.ParsePage(query.Get("page"), query.Get("pageSize"), dto.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !isKnownOrderStatus(status) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "status is invalid")
		return
	}
	// 只认 orderNo / status / hasMembership 三个筛选：userId、storeId 这类是后台的维度，
	// 小程序端传了也没有意义——归属在 service 层被强制覆盖成调用者自己。hasDrink / hasAddon
	// 同理由后台独有：拿到自己订单的人在别的列表里找自己的单，是后台的分类视角。
	filter := repository.OrderFilter{
		OrderNo:  strings.TrimSpace(query.Get("orderNo")),
		Status:   status,
		Page:     page,
		PageSize: pageSize,
	}
	if filter.HasMembership, message = parseLinePresence("hasMembership", query.Get("hasMembership")); message != "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", message)
		return
	}
	rows, total, err := c.orders.ListOrders(r.Context(), filter, userID)
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

func (c *MiniappOrderController) detail(w http.ResponseWriter, r *http.Request, userID, orderID string) {
	// 传 userID：service 会校验归属（别人的单回「不存在」）。
	detail, err := c.orders.GetOrderDetail(r.Context(), orderID, userID)
	if err != nil {
		writeOrderError(w, err, "failed to get order")
		return
	}
	api.Success(w, orderDetailResponse(detail))
}

func (c *MiniappOrderController) cancel(w http.ResponseWriter, r *http.Request, userID, orderID string) {
	var body dto.CancelOrderRequest
	// 用户取消可以不写原因（前端只有一个「取消订单」按钮），空原因补一句默认的：
	// 状态流水里那一行得能读懂是谁、为什么。后台取消必须写原因（admin_order.cancel 那边
	// 的校验会拒空串），因为那是一次人工干预。
	reason := "用户取消"
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil && strings.TrimSpace(body.Reason) != "" {
		reason = strings.TrimSpace(body.Reason)
	}
	result, err := c.orders.CancelOrder(r.Context(), service.CancelOrderInput{
		OrderID: orderID,
		Reason:  reason,
		// 传 userID 让 service 校验归属；ActorType 记 user，取消人就是他自己。
		UserID:    userID,
		ActorType: model.ActorUser,
		ActorID:   &userID,
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
