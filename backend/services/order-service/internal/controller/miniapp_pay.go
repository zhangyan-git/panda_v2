package controller

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// pay 发起一次支付。
//
// 它挂在 `POST /v1/miniapp/orders/{orderNo}/pay`——**键是订单号而不是内部 id**，因为发起
// 支付这条链路的两端（这里的小程序、下游的支付服务）用的都是订单号。这条路径树上的
// detail/cancel 用 id 是既成事实，两者并存是有意的，不是漏改。
//
// 这个 handler 一行订单事实都不写：读单、校验能不能付，然后由支付域建支付单。订单变 paid
// 是支付结果事件回来之后的事。所以这里没有幂等事务，重发同一把 Idempotency-Key 拿到的是
// 同一张支付单——幂等发生在支付侧。
func (c *MiniappOrderController) pay(w http.ResponseWriter, r *http.Request, userID, orderNo string) {
	// 幂等键从请求头来，理由同下单与申请退款：它是「这一次提交」的标识，不是支付的内容。
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Idempotency-Key is required")
		return
	}
	var body dto.PayOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request body")
		return
	}
	action, err := c.orders.InitiatePayment(r.Context(), service.InitiatePaymentInput{
		OrderNo:         orderNo,
		UserID:          userID,
		PaymentMethodID: body.PaymentMethodID,
		RequestID:       idempotencyKey,
		TraceID:         traceID(r),
	})
	if err != nil {
		writeOrderError(w, err, "failed to initiate payment")
		return
	}
	// 统一回 200，**包括 status='failed' 的那一次**（渠道明确拒绝）。它不是一个错误，
	// 是「这次发起有结论了，结论是不能付」：客户端看 status 决定是调起支付还是让用户换一种
	// 方式。回 4xx 会让客户端以为请求有问题去重发，而重发只会得到同一个结论。
	//
	// 也不区分首次与重放：这个接口什么都没新建（支付单的幂等由支付侧负责），订单这边没有
	// 任何变化可以据此区分。客户端要判断「是不是同一笔」，看 paymentNo 就够了。
	api.Success(w, action)
}
