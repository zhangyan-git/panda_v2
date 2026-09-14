package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// MiniappAfterSaleController 是小程序端（C 端）已经存在的售后单上的动作。
//
// 申请不在这里：它的路径是 /v1/miniapp/orders/{id}/after-sales，挂在订单那棵树上
// （见 MiniappOrderController.Orders）。这里管的是申请之后再动它——撤销。
type MiniappAfterSaleController struct{ orders *service.OrderService }

const miniappAfterSalePath = "/v1/miniapp/after-sales"

func NewMiniappAfterSaleController(orders *service.OrderService) *MiniappAfterSaleController {
	return &MiniappAfterSaleController{orders: orders}
}

// AfterSales 分发 /v1/miniapp/after-sales 这一棵路径。每个动作执行前都要过 requireConsumer。
func (c *MiniappAfterSaleController) AfterSales(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, miniappAfterSalePath), "/")
	switch {
	case strings.HasSuffix(rest, "/cancel") && r.Method == http.MethodPost:
		c.cancel(w, r, userID, strings.TrimSuffix(rest, "/cancel"))
	default:
		http.NotFound(w, r)
	}
}

// cancel 撤销一张还没被审核的售后申请。
//
// 撤销后要重新申请就开一张新的：一张单的 status 讲两个故事，查起来会分不清。
func (c *MiniappAfterSaleController) cancel(w http.ResponseWriter, r *http.Request, userID, afterSaleNo string) {
	var body dto.CancelAfterSaleRequest
	// 用户撤销可以不写理由（点错了一次不需要解释），空体也是合法的，因此 io.EOF 放过。
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	reason := strings.TrimSpace(body.Reason)

	row, err := c.orders.CancelAfterSale(r.Context(), service.CancelAfterSaleInput{
		AfterSaleNo: afterSaleNo,
		// 传 userID 让仓储校验归属：别人的售后单一律回「不存在」。
		UserID:  userID,
		Reason:  reason,
		TraceID: traceID(r),
	})
	if err != nil {
		writeOrderError(w, err, "failed to cancel after sale")
		return
	}
	api.Success(w, afterSaleView(row))
}
