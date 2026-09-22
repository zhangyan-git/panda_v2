package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// MerchantOrderController 是商户端的订单只读面。
//
// 分发写法与 AdminOrderController 相同（一个 handler 配几条路由 + TrimPrefix 判分支），
// 理由也一样：路径参数只有一层 {id}，mux 的 {id} 只吃一个路径段，注册顺序稍有出入就会
// 把 /{id}/cancel 解析成 id="cancel"。商户这条路上没有那类子路径，所以 switch 只有两支。
type MerchantOrderController struct{ orders *service.MerchantOrderService }

const merchantOrderPath = "/v1/merchant/orders"

func NewMerchantOrderController(orders *service.MerchantOrderService) *MerchantOrderController {
	return &MerchantOrderController{orders: orders}
}

// Orders 分发 /v1/merchant/orders 这一棵路径。
//
// 查询串里只认 status / orderNo / source / createdFrom / createdTo：门店那一项由中间件
// 解析出来的数据范围决定，请求里带了也不看。边界不是过滤条件，是这一屏存在的前提。
func (c *MerchantOrderController) Orders(w http.ResponseWriter, r *http.Request) {
	scope, ok := auth.StoreScopeFromRequest(r)
	if !ok {
		// 没有边界就什么都不给：这一条是 fail-closed 的落点，漏挂中间件的路由必须
		// 返回 503，而不是把「没有范围」读成「全部」。
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户鉴权暂不可用")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, merchantOrderPath), "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		c.list(w, r, scope)
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	c.detail(w, r, scope, rest)
}

func (c *MerchantOrderController) list(w http.ResponseWriter, r *http.Request, scope auth.StoreScope) {
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
	createdFrom, err := parseTimeParam(query.Get("createdFrom"))
	if err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "createdFrom must be an RFC3339 timestamp")
		return
	}
	createdTo, err := parseTimeParam(query.Get("createdTo"))
	if err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "createdTo must be an RFC3339 timestamp")
		return
	}

	rows, total, err := c.orders.List(r.Context(), scope, service.MerchantOrderQuery{
		Status:      status,
		OrderNo:     strings.TrimSpace(query.Get("orderNo")),
		Source:      source,
		CreatedFrom: createdFrom,
		CreatedTo:   createdTo,
		Page:        page,
		PageSize:    pageSize,
	})
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

// detail 返回一单的全量。
//
// 越界与不存在都回 404，同一句话：范围外的那一单对调用方而言就是不存在。
func (c *MerchantOrderController) detail(w http.ResponseWriter, r *http.Request, scope auth.StoreScope, orderID string) {
	detail, err := c.orders.Get(r.Context(), scope, orderID)
	if errors.Is(err, service.ErrOrderOutOfScope) || errors.Is(err, service.ErrOrderNotFound) {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "订单不存在")
		return
	}
	if err != nil {
		writeOrderError(w, err, "failed to get order")
		return
	}
	// 详情形状与后台同一个 DTO：同一单在两个端上要显示的是同一组事实，两处各写一份只会
	// 让字段各自漂移。售后记录随订单一起受范围约束，不额外开口子。
	api.Success(w, orderDetailResponse(detail))
}
