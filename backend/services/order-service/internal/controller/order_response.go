package controller

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/service"
)

// traceID 取当前请求的链路 id，用于把它带进状态流水与领域事件。
//
// 没有链路（没接 tracing）时返回空串——那正是「这个请求不在一条链路上」的如实表达，
// 而不是随便编一个 id 让日志里多一条对不上的线索。
func traceID(r *http.Request) string { return audit.TraceIDFromContext(r.Context()) }

// writeOrderError 把业务层的错误翻成 HTTP 状态码，两端共用。
//
// 放在一处而不是每个 handler 各写一份：同一个错误在小程序和后台必须是同一个状态码，
// 否则前端得按接口记两套规矩，而漏掉的那一个会变成 500——一次「这单已经关了」的取消
// 请求看起来像服务端崩了。
func writeOrderError(w http.ResponseWriter, err error, message string) {
	switch {
	case service.IsValidationError(err):
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	case errors.Is(err, service.ErrIdempotencyConflict):
		api.Error(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error())
	case errors.Is(err, service.ErrIdempotencyInProgress):
		// 同一个幂等键的上一笔还在跑：让调用方等一下再重发，重发会命中同一把锁。
		api.Error(w, http.StatusConflict, "IDEMPOTENCY_IN_PROGRESS", err.Error())
	case errors.Is(err, service.ErrOrderNotFound), errors.Is(err, service.ErrDeviceNotFound),
		errors.Is(err, service.ErrAfterSaleNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, err.Error())
	case errors.Is(err, service.ErrDeviceUnavailable):
		api.Error(w, http.StatusConflict, "DEVICE_UNAVAILABLE", err.Error())
	case errors.Is(err, service.ErrDeviceLookupUnavailable):
		// 问不到设备是我们这侧暂时答不上来，不是请求有错：回 503 而不是 400，
		// 否则客户端会以为「这台机器不能下单」而不再重试。
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, err.Error())
	case errors.Is(err, service.ErrOrderNotPending), errors.Is(err, service.ErrOrderNotCompletable):
		api.Error(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, service.ErrOrderNotPayable):
		// 应付额为 0：这一单本来就不用付钱。它不是「请求写错了」，所以是 409 而不是 400。
		api.Error(w, http.StatusConflict, "NOTHING_TO_PAY", err.Error())
	// —— 发起支付：结论来自支付域 ——
	//
	// 这一组的判据是「调用方该做什么」，与支付侧 createPayment 的分法一一对应
	// （见 client/mapPaymentError）。要分清的核心是**有结论**与**没结论**：
	// 前者让用户改，后者让用户等，而把没结论的说成「支付失败」会让用户换一种方式再付一次，
	// 而渠道那边那张预支付单可能仍然有效。
	case errors.Is(err, service.ErrPaymentRejected):
		// 这个支付方式现在用不了（不存在、被停用、依赖的服务没建）。重发一模一样的一次没用，
		// 所以是 409 不是 503——客户端该做的是让用户换一种方式，不是重试。
		api.Error(w, http.StatusConflict, "PAYMENT_REJECTED", err.Error())
	case errors.Is(err, service.ErrPaymentConflict):
		api.Error(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", err.Error())
	case errors.Is(err, service.ErrPaymentUncertain):
		// 渠道没给出确定的答复。回 503 而不是 5xx 里的「内部错误」：这不是我们崩了，是
		// 这一笔暂时没有结论，稍后带着**同一把**幂等号重发就可能拿到那张支付单。
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, err.Error())
	case errors.Is(err, service.ErrPaymentServiceUnavailable):
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, err.Error())
	// —— 售后：判据在仓储的事务里，所以不经过 IsValidationError ——
	//
	// 这一组里有几个确实是「请求不合法」（按行退的那一行不是这一单的），但判不判得了要看
	// 账上的事实，所以它们不在 service.ValidationErrors 里——那份名单是「光看请求就能判」
	// 的全集，混进去会让下一个人以为在 controller 也能提前挡掉。
	case errors.Is(err, service.ErrAfterSaleLineMismatch), errors.Is(err, service.ErrAfterSaleNothingToRefund),
		errors.Is(err, service.ErrAfterSaleExceedsRefundable):
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	case errors.Is(err, service.ErrFortuneCardConfirmationRequired):
		// 单独一个码而不是并进 INVALID_ARGUMENT：这不是「请求体写错了」，是「这一单有个
		// 必须由人来确认的前提」。审核页面要靠它把确认框摆出来，而不是把审核人钉在一句
		// 报错上——用人话描述合同，靠的是码，不是 message 的措辞。
		api.Error(w, http.StatusBadRequest, "FORTUNE_CARD_CONFIRMATION_REQUIRED", err.Error())
	case errors.Is(err, service.ErrOrderNotRefundable), errors.Is(err, service.ErrAfterSaleNotPending):
		api.Error(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, service.ErrAfterSaleAlreadyOpen):
		api.Error(w, http.StatusConflict, "AFTER_SALE_ALREADY_OPEN", err.Error())
	case errors.Is(err, service.ErrAfterSaleAlreadyRefunded):
		api.Error(w, http.StatusConflict, "AFTER_SALE_ALREADY_REFUNDED", err.Error())
	case errors.Is(err, service.ErrPaymentAmountMismatch):
		api.Error(w, http.StatusConflict, "PAYMENT_AMOUNT_MISMATCH", err.Error())
	default:
		api.Error(w, http.StatusInternalServerError, "INTERNAL_ERROR", message)
	}
}

// 这一层只做 model → DTO 的搬运：model 上只有 db tag，直接序列化会输出 PascalCase
// （Go 字段名），前端按 camelCase 取值会全部落空。所有对外响应都必须经过这里。

func orderSummaryResponse(order *model.Order) dto.OrderSummary {
	return dto.OrderSummary{
		ID:                   order.ID,
		OrderNo:              order.OrderNo,
		UserID:               order.UserID,
		Source:               order.Source,
		Status:               order.Status,
		FulfillmentStatus:    order.FulfillmentStatus,
		StoreID:              order.StoreID,
		StoreName:            order.StoreName,
		DeviceID:             order.DeviceID,
		DeviceNo:             order.DeviceNo,
		OriginalAmount:       order.OriginalAmount,
		DiscountAmount:       order.DiscountAmount,
		PayableAmount:        order.PayableAmount,
		PaidAmount:           order.PaidAmount,
		RefundedAmount:       order.RefundedAmount,
		PaymentMethod:        order.PaymentMethod,
		PaymentNo:            order.PaymentNo,
		PaidAt:               order.PaidAt,
		FinishedAt:           order.FinishedAt,
		CancelledAt:          order.CancelledAt,
		CancellationReason:   order.CancellationReason,
		ExpiresAt:            order.ExpiresAt,
		Remark:               order.Remark,
		FortuneCardsExpected: order.FortuneCardsExpected,
		CreatedAt:            order.CreatedAt,
		UpdatedAt:            order.UpdatedAt,
	}
}

// orderSummaryResponses 把列表结果映射成响应。
//
// 那三个行类型标记由列表查询顺手算出来（repository.OrderRow），不在这里再从行里推——
// 列表根本没读行。详情那边反过来，行已经在手上，就地从行里推。
func orderSummaryResponses(rows []*repository.OrderRow) []dto.OrderSummary {
	out := make([]dto.OrderSummary, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Order == nil {
			continue
		}
		summary := orderSummaryResponse(row.Order)
		summary.HasDrinkLine = row.HasDrinkLine
		summary.HasAddonLine = row.HasAddonLine
		summary.HasMembershipLine = row.HasMembershipLine
		summary.PickupCode = row.PickupCode
		out = append(out, summary)
	}
	return out
}

func orderDetailResponse(detail *repository.OrderDetail) dto.OrderDetail {
	summary := orderSummaryResponse(detail.Order)
	lines := make([]dto.OrderLineView, 0, len(detail.Lines))
	for _, line := range detail.Lines {
		// 这三个标记是给「咖啡订单 / 幸运杯套订单 / 会员订单」这类分类用的，详情页已经从行里
		// 读出来了，就地算，不必再为它多一条查询。取杯号同理：列表靠一条标量子查询带出来
		// （repository.orderRowPickupColumn），详情这边行就在手上。
		switch line.LineType {
		case model.LineTypeDrink:
			summary.HasDrinkLine = true
			if summary.PickupCode == nil {
				// 饮品行共用同一个取杯号，取第一行即可；万一不一致也按行序稳定取第一条。
				summary.PickupCode = line.PickupCode
			}
		case model.LineTypeAddon:
			summary.HasAddonLine = true
		case model.LineTypeMembership:
			summary.HasMembershipLine = true
		}
		lines = append(lines, orderLineView(line))
	}
	paymentLines := make([]dto.OrderPaymentLineView, 0, len(detail.PaymentLines))
	for _, line := range detail.PaymentLines {
		paymentLines = append(paymentLines, orderPaymentLineView(line))
	}
	transitions := make([]dto.OrderTransitionView, 0, len(detail.Transitions))
	for _, transition := range detail.Transitions {
		transitions = append(transitions, orderTransitionView(transition))
	}
	return dto.OrderDetail{
		OrderSummary:        summary,
		SceneToken:          detail.Order.SceneToken,
		MembershipID:        detail.Order.MembershipID,
		MembershipSnapshot:  decodeJSON(detail.Order.MembershipSnapshot),
		FortuneCardSnapshot: decodeJSON(detail.Order.FortuneCardSnapshot),
		Lines:               lines,
		PaymentLines:        paymentLines,
		Transitions:         transitions,
		AfterSales:          orderAfterSaleViews(detail, lines),
	}
}

// orderAfterSaleViews 映射订单详情里的售后记录。
//
// 派生字段（承诺的福卡、被退的那一行）就地从订单与行里取：详情已经把它们读出来了。这样
// 同一条售后记录在订单详情与后台售后列表里是同一份数据，不会因为两处各查一次而出现
// 「详情说这单没福卡、列表说有」。
func orderAfterSaleViews(detail *repository.OrderDetail, lines []dto.OrderLineView) []dto.AfterSaleView {
	byID := make(map[string]dto.OrderLineView, len(lines))
	for _, line := range lines {
		byID[line.ID] = line
	}
	out := make([]dto.AfterSaleView, 0, len(detail.AfterSales))
	for _, sale := range detail.AfterSales {
		view := afterSaleView(&repository.AfterSaleRow{
			AfterSale:            sale,
			FortuneCardsExpected: detail.Order.FortuneCardsExpected,
			FortuneCardSnapshot:  detail.Order.FortuneCardSnapshot,
		})
		if sale.OrderLineID != nil {
			if line, ok := byID[*sale.OrderLineID]; ok {
				view.OrderLine = afterSaleLineRef(line.ID, line.LineNo, line.LineType,
					line.ItemName, line.Quantity, line.PayableAmount)
			}
		}
		out = append(out, view)
	}
	return out
}

// afterSaleView 把一条售后记录映射成响应。
//
// 输入是 repository.AfterSaleRow 而不是 model.OrderAfterSale：后者只有售后单自己的列，
// 而审核福卡规则要看的「这单承诺过几张福卡」在订单上。少传一个字段就会让响应里的
// fortuneCardsExpected 静默变成 0——那是最坏的一种错：它长得像一个答案。
func afterSaleView(item *repository.AfterSaleRow) dto.AfterSaleView {
	sale := item.AfterSale
	view := dto.AfterSaleView{
		ID:                   sale.ID,
		AfterSaleNo:          sale.AfterSaleNo,
		OrderID:              sale.OrderID,
		OrderNo:              sale.OrderNo,
		UserID:               sale.UserID,
		Type:                 sale.Type,
		Scope:                sale.Scope,
		OrderLineID:          sale.OrderLineID,
		Status:               sale.Status,
		Reason:               sale.Reason,
		Images:               decodeJSON(sale.Images),
		RefundAmount:         sale.RefundAmount,
		RefundNo:             sale.RefundNo,
		FailureCode:          sale.FailureCode,
		ReviewedBy:           sale.ReviewedBy,
		ReviewedAt:           sale.ReviewedAt,
		ReviewRemark:         sale.ReviewRemark,
		RefundedAt:           sale.RefundedAt,
		CreatedAt:            sale.CreatedAt,
		UpdatedAt:            sale.UpdatedAt,
		FortuneCardsExpected: item.FortuneCardsExpected,
		FortuneCardSnapshot:  decodeJSON(item.FortuneCardSnapshot),
	}
	if item.OrderLine != nil {
		view.OrderLine = afterSaleLineRef(item.OrderLine.ID, item.OrderLine.LineNo,
			item.OrderLine.LineType, item.OrderLine.ItemName, item.OrderLine.Quantity,
			item.OrderLine.PayableAmount)
	}
	return view
}

func afterSaleViews(rows []*repository.AfterSaleRow) []dto.AfterSaleView {
	out := make([]dto.AfterSaleView, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.AfterSale == nil {
			continue
		}
		out = append(out, afterSaleView(row))
	}
	return out
}

// afterSaleLineRef 是「退的是哪一行」的摘要。两个调用点的来源不同（列表的路是 SQL 里
// COALESCE 出来的宽表列，详情的路是已经读出来的订单行），字段一样，所以按值传而不是
// 造一个假的中间结构。
func afterSaleLineRef(id string, lineNo int, lineType, itemName string, quantity int, payableAmount int64) *dto.AfterSaleLineRef {
	return &dto.AfterSaleLineRef{
		ID: id, LineNo: lineNo, LineType: lineType,
		ItemName: itemName, Quantity: quantity, PayableAmount: payableAmount,
	}
}

func orderLineView(line *model.OrderLine) dto.OrderLineView {
	return dto.OrderLineView{
		ID:                     line.ID,
		LineNo:                 line.LineNo,
		LineType:               line.LineType,
		ItemID:                 line.ItemID,
		ItemCode:               line.ItemCode,
		ItemName:               line.ItemName,
		ItemImage:              line.ItemImage,
		Quantity:               line.Quantity,
		OriginalUnitPrice:      line.OriginalUnitPrice,
		UnitPrice:              line.UnitPrice,
		PriceDiscountAmount:    line.PriceDiscountAmount,
		DiscountAmount:         line.DiscountAmount,
		PayableAmount:          line.PayableAmount,
		CouponID:               line.CouponID,
		CouponDiscountAmount:   line.CouponDiscountAmount,
		Specs:                  decodeJSON(line.Specs),
		SelectionSnapshot:      decodeJSON(line.SelectionSnapshot),
		CampaignID:             line.CampaignID,
		CampaignSnapshot:       decodeJSON(line.CampaignSnapshot),
		MembershipPlanSnapshot: decodeJSON(line.MembershipPlanSnapshot),
		DeviceID:               line.DeviceID,
		DeviceOrderNo:          line.DeviceOrderNo,
		FulfillmentTaskNo:      line.FulfillmentTaskNo,
		PickupCode:             line.PickupCode,
		Remark:                 line.Remark,
		CreatedAt:              line.CreatedAt,
		UpdatedAt:              line.UpdatedAt,
	}
}

func orderPaymentLineView(line *model.OrderPaymentLine) dto.OrderPaymentLineView {
	return dto.OrderPaymentLineView{
		ID:             line.ID,
		LineNo:         line.LineNo,
		LineType:       line.LineType,
		Amount:         line.Amount,
		Status:         line.Status,
		PaymentNo:      line.PaymentNo,
		FailureCode:    line.FailureCode,
		AccountEntryID: line.AccountEntryID,
		SucceededAt:    line.SucceededAt,
		ReversedAt:     line.ReversedAt,
		CreatedAt:      line.CreatedAt,
		// ProviderTransactionID 有意不映射：渠道流水号是对账凭据，要看去 payment-service
		// 查，这里多带一份只会多一个泄漏面。
	}
}

func orderTransitionView(transition *model.OrderStateTransition) dto.OrderTransitionView {
	return dto.OrderTransitionView{
		ID:            transition.ID,
		AggregateType: transition.AggregateType,
		AggregateID:   transition.AggregateID,
		FromStatus:    transition.FromStatus,
		ToStatus:      transition.ToStatus,
		Reason:        transition.Reason,
		ActorType:     transition.ActorType,
		ActorID:       transition.ActorID,
		CreatedAt:     transition.CreatedAt,
	}
}

// decodeJSON 把库里的 JSONB 解成 any 再交给序列化器。
//
// 不解的话就是一段 base64——json.RawMessage 是 []byte，直接放进 any 字段会被
// encoding/json 当成字节切片编码成 base64 字符串，前端拿到的是一串乱码。
// 解不开时返回 nil 而不是报错：详情页不该因为一个历史行里的坏快照整页 500，
// 少一段快照比看不到订单好。
func decodeJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}
