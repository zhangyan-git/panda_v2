package controller

import (
	"encoding/json"
	"errors"
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
	// 补单：这个分支**必须**排在下面对 {id} 的兜底之前。两条路径的形状一样（/v1/admin/orders/
	// 后面跟一段），而 mux 那边登记的顺序也照这个来（见 routes.RegisterAdmin）——落到兜底里
	// 的话它会被当成一个叫 "pickup-repairs" 的订单 id，而那个 id 查出来是一句「不是 uuid」。
	case rest == "pickup-repairs":
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		c.repairPickupOrder(w, r)
	case strings.HasSuffix(rest, "/cancel"):
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		c.cancel(w, r, strings.TrimSuffix(rest, "/cancel"), identity)
	case strings.HasSuffix(rest, "/complete"):
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		c.complete(w, r, strings.TrimSuffix(rest, "/complete"), identity)
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
	// 判据用 model.IsOrderSource 而不是在这里列一遍取值：这份白名单**漏过一次**——后台的
	// 来源下拉里一直摆着「设备下单」（device 是 005 加的），而这里只认到 003 那两个，
	// 于是选中「设备下单」就回 400，看起来像「这个来源没有订单」。列在这里的每多一个取值，
	// 就多一次漏掉的机会；词表的全集在 model 那边，与库上的 CHECK 逐字对齐。
	if source != "" && !model.IsOrderSource(source) {
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
	// userID 传空串：后台查询不校验归属（取杯号两端都返回，客服要答「我的号是多少」）。
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

// complete 把一笔已付款的订单标记为完成。
//
// 不收请求体，也不需要理由：取消要写清「为什么关掉用户这一单」，完成是一次**放行**
// ——它只推进订单、按承诺发福卡，没有任何东西被拒。要一个理由只会催生「无」这样的填充。
//
// 完成人取自令牌，不取自请求体（同取消）：让被审计的人自己填审计字段，等于没有审计。
func (c *AdminOrderController) complete(w http.ResponseWriter, r *http.Request, orderID string, identity auth.Identity) {
	actorID := identity.UserID
	if actorID == "" {
		actorID = identity.Subject
	}
	result, err := c.orders.CompleteOrder(r.Context(), service.CompleteOrderInput{
		OrderID: orderID,
		ActorID: actorID,
		TraceID: traceID(r),
	})
	if err != nil {
		writeOrderError(w, err, "failed to complete order")
		return
	}
	api.Success(w, map[string]any{
		"orderId": result.OrderID,
		"status":  result.Status,
	})
}

// repairPickupOrder 补建一张取货码订单：把「钱扣了、单没建出来」的那一单补出来。
//
// # 它是一个运维入口，**这个前端上没有按钮**
//
// 与设备余额那条后台调整接口同一个形状（coffee-machine-service 的
// POST /v1/admin/coffee-machines/devices/{id}/balance）：路由挂了、权限码加了，但后台页面
// 不提供入口。
// 理由也一样——这不是一个「操作员日常点得到」的动作，而是出事后有人拿着对方单号来补一次。
// 真要给它做一个页面，那张页面必须先把「怎么判断这一单该补」讲清楚，而那是运维手册的事。
//
// # 取货码那一格传空串是有意的
//
// 补的是钱已经扣过的单：扣减撞上同一个 request_id 时直接短路回当初那一笔，不需要验证码
// （见咖啡机域的 DeductDeviceBalance）。而空码本身过不了那台设备的校验（没配过码的设备
// 一律拒绝、配过码的设备空串对不上），所以这条接口**不可能引起一次新的扣款**——它只会把
// 已经扣过的单补出来。副作用是一个很清楚的信号：回「取货码不对」意味着这笔钱从来没扣过，
// 那一单不该补（见下面那个分支）。
//
// # 它的幂等是自己带来的
//
// 重复补同一单不会建出第二张：建单那一步按对方单号命中既有单，返回 created=false。所以
// 这个按钮点两次是安全的——这也是它敢只用一个「补」字命名而不要一个确认参数的原因。
func (c *AdminOrderController) repairPickupOrder(w http.ResponseWriter, r *http.Request) {
	var body dto.PickupRepairRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	result, err := c.orders.CreatePickupOrder(r.Context(), service.CreatePickupOrderInput{
		ThirdPartyOrderNo: body.ThirdPartyOrderNo,
		DeviceSerial:      body.DeviceSerial,
		DrinkCode:         body.DrinkCode,
		// 空码是这条路的一部分，见上面的说明。**不 trim、不判空**：判空会在这里就拒掉一次
		// 本该成功的补单，而「这个码是空的」在持有那一列的那一侧有确定结论。
		PickupPassword: "",
		Remark:         body.Remark,
	})
	switch {
	case err == nil:
		api.Success(w, map[string]any{
			"orderId": result.OrderID,
			"orderNo": result.OrderNo,
			// created=false：这张单早就建好了（可能是上一次补成的，也可能是合作方自己重投
			// 建出来的）。它**不是失败**——调用方要的是「这一单在库里」，那就是。
			"created": result.Created,
		})
	case errors.Is(err, service.ErrDeviceBalancePasswordRejected):
		// 空码被拒 = 这个 request_id 在设备余额流水上没有对应的扣减，也就是说**这笔钱从来
		// 没扣过**。这不是一次「码填错了」，而是「这一单不该补」。回一个说得出话的码与文案，
		// 而不是让取货码那条路的原话（「取货码不对」）出现在一个没有取货码的请求上。
		api.Error(w, http.StatusConflict, "NOT_CHARGED", err.Error())
	case errors.Is(err, service.ErrThirdPartyOrderNoTaken):
		// 这个单号属于另一类设备单（刷卡机那条）。补不了，也不该补——两张单的钱来源不同。
		api.Error(w, http.StatusConflict, "ORDER_NO_TAKEN", err.Error())
	default:
		writeOrderError(w, err, "failed to repair pickup order")
	}
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
