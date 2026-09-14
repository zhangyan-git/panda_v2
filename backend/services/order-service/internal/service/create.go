package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// CreateOrderInput 是一次下单的全部输入。
//
// UserID 来自令牌，绝不来自请求体：让调用方自报「我是谁」等于让用户替我们填权限字段。
type CreateOrderInput struct {
	UserID         string
	IdempotencyKey string
	TraceID        string
	Request        dto.CreateOrderRequest
}

// CreateOrder 校验、算钱、快照、落单。
//
// 返回的 replayed 为 true 表示这次命中了幂等键、什么都没做——调用方照常把响应回给
// 客户端，但要知道「这不是一次新的下单」。
func (s *OrderService) CreateOrder(ctx context.Context, in CreateOrderInput) (*dto.CreateOrderResponse, bool, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return nil, false, ErrUserRequired
	}
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.IdempotencyKey == "" {
		return nil, false, ErrIdempotencyKeyRequired
	}

	req := in.Request
	req.Source = strings.TrimSpace(req.Source)
	if req.Source != model.SourceMiniapp && req.Source != model.SourceScreenQR {
		return nil, false, ErrInvalidSource
	}
	if len(req.Lines) == 0 {
		return nil, false, ErrLinesRequired
	}
	if len(req.Lines) > maxOrderLines {
		return nil, false, ErrTooManyLines
	}

	lines := make([]*repository.OrderLineInsert, 0, len(req.Lines))
	var originalAmount, discountAmount int64
	drinkLines, membershipLines := 0, 0
	for i, raw := range req.Lines {
		line, err := buildLine(i+1, raw)
		if err != nil {
			return nil, false, err
		}
		switch line.LineType {
		case model.LineTypeDrink:
			drinkLines++
		case model.LineTypeMembership:
			membershipLines++
		}
		originalAmount += line.OriginalUnitPrice * int64(line.Quantity)
		discountAmount += line.DiscountAmount
		lines = append(lines, line)
	}
	// 一杯一单、一单一个会员：数据库那边有 order_lines_one_drink_per_order 与
	// order_lines_one_membership_per_order 兜底，但在这里先讲清楚，别让客户端拿到一个
	// 从唯一索引里冒出来的错误——它说不清是哪一行的问题。
	if drinkLines > 1 {
		return nil, false, ErrTooManyDrinkLines
	}
	if membershipLines > 1 {
		return nil, false, ErrTooManyMembershipLines
	}

	storeID, deviceID, deviceNo, err := s.resolveDevice(ctx, req, drinkLines > 0)
	if err != nil {
		return nil, false, err
	}
	// 行的 device_id 是订单上那台机器的副本（V2 一台机器服务一次下单），在解析出设备之后
	// 统一填：请求里没有按行的设备字段，让每一行自己去带等于给同一台机器留两个来源。
	for _, line := range lines {
		if line.LineType == model.LineTypeDrink {
			line.DeviceID = deviceID
		}
	}
	if membershipLines > 0 && strings.TrimSpace(derefString(req.MembershipID)) == "" {
		return nil, false, ErrMembershipIDRequired
	}
	if membershipLines == 0 && strings.TrimSpace(derefString(req.MembershipID)) != "" {
		return nil, false, ErrMembershipIDNotAllowed
	}

	payableAmount := originalAmount - discountAmount
	if payableAmount < 0 {
		// 各行分别校验过「本行优惠不超过本行原价」，合计仍可能为负（一行为负、另一行为正），
		// 而 orders_payable_matches 会拒掉它。在这里拦住，把 23514 变成一句人话。
		return nil, false, ErrDiscountExceedsLine
	}

	now := s.now().UTC()
	fulfillmentStatus := model.FulfillmentNone
	if drinkLines > 0 {
		// 有饮品要出杯，才有履约这回事；纯会员订单停在 none，否则它会永远卡在「待制作」队列里。
		fulfillmentStatus = model.FulfillmentPending
	}
	order := &repository.OrderInsert{
		OrderNo:              generateOrderNo(now),
		UserID:               in.UserID,
		Source:               req.Source,
		Status:               model.OrderStatusPendingPayment,
		FulfillmentStatus:    fulfillmentStatus,
		StoreID:              storeID,
		StoreName:            strings.TrimSpace(req.StoreName),
		DeviceID:             deviceID,
		DeviceNo:             derefString(deviceNo),
		SceneToken:           strings.TrimSpace(req.SceneToken),
		OriginalAmount:       originalAmount,
		DiscountAmount:       discountAmount,
		PayableAmount:        payableAmount,
		MembershipID:         normalizedID(req.MembershipID),
		MembershipSnapshot:   jsonObject(req.MembershipSnapshot),
		FortuneCardsExpected: req.FortuneCardsExpected,
		FortuneCardSnapshot:  jsonObject(req.FortuneCardSnapshot),
		Remark:               strings.TrimSpace(req.Remark),
		ExpiresAt:            now.Add(s.paymentTTL),
	}
	if order.FortuneCardsExpected < 0 {
		return nil, false, ErrInvalidAmount
	}

	result, replayed, err := s.repository.CreateOrder(ctx, repository.CreateOrderParams{
		IdempotencyKey: in.IdempotencyKey,
		// request_id 就用幂等键：客户端重发「同一次下单」时带的必然是同一个键，
		// 所以它正是「这次下单请求」的标识。orders_request_id_key 因此与幂等表互为
		// 第二道锁——它挡不住的事，幂等表挡；幂等表没写进去的那一瞬间，它挡。
		RequestID: in.IdempotencyKey,
		TraceID:   in.TraceID,
		Order:     order,
		Lines:     lines,
	})
	if err != nil {
		return nil, false, mapWriteError(err)
	}
	return &dto.CreateOrderResponse{
		OrderID:           result.OrderID,
		OrderNo:           result.OrderNo,
		Status:            result.Status,
		FulfillmentStatus: result.FulfillmentStatus,
		OriginalAmount:    result.OriginalAmount,
		DiscountAmount:    result.DiscountAmount,
		PayableAmount:     result.PayableAmount,
		ExpiresAt:         result.ExpiresAt,
		CreatedAt:         result.CreatedAt,
	}, replayed, nil
}

// resolveDevice 定下这一单的门店与设备，并对设备做下单校验（方案 5.8）。
//
// 门店来自设备，不来自请求：设备是「这台机器属于哪个点位」的唯一来源，请求里再带一个
// storeId 就有了两个说法。带了就必须与设备上的一致，不一致直接拒——静默以请求为准会
// 让订单挂在一个设备并不在的点位上，那正是运维最难查的一类脏数据。
func (s *OrderService) resolveDevice(ctx context.Context, req dto.CreateOrderRequest, needsDevice bool) (storeID, deviceID, deviceNo *string, err error) {
	requestedStore := strings.TrimSpace(derefString(req.StoreID))
	requestedDevice := strings.TrimSpace(derefString(req.DeviceID))

	if !needsDevice {
		// 纯会员订单不下发设备校验：会员是账号上的权益，不落在某一台机器上。
		return normalizedID(req.StoreID), nil, nil, nil
	}
	if requestedDevice == "" {
		return nil, nil, nil, ErrDeviceRequired
	}
	if s.devices == nil {
		// 没有可用的设备读端就不下单：跳过校验等于把「这台机器能不能出杯」交给调用方说，
		// 而那正是这一版唯一的服务端校验。
		return nil, nil, nil, fmt.Errorf("%w: device reader is not configured", ErrDeviceLookupUnavailable)
	}
	device, found, err := s.devices.Get(ctx, requestedDevice)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrDeviceLookupUnavailable, err)
	}
	if !found {
		return nil, nil, nil, ErrDeviceNotFound
	}
	if device.Status != "active" {
		return nil, nil, nil, ErrDeviceUnavailable
	}
	if requestedStore != "" && requestedStore != device.StoreID {
		return nil, nil, nil, ErrStoreMismatch
	}
	if device.StoreID == "" {
		// 设备还没挂点位：可以下单（用户扫的就是这台机器的码），但订单上没有点位，
		// 后台按点位筛的时候这一单不会出现在任何点位下。这是如实记录，不是丢字段。
		return nil, &device.ID, &device.SerialUnique, nil
	}
	store := device.StoreID
	return &store, &device.ID, &device.SerialUnique, nil
}

// buildLine 校验一行并算出它的金额。
//
// 两条恒等式在这里算出、由数据库的 CHECK 再验一遍：
//
//	discount_amount = price_discount_amount + coupon_discount_amount
//	payable_amount  = original_unit_price * quantity - discount_amount
//
// 收进来的只有 priceDiscountAmount 与 couponDiscountAmount，另外两个是算出来的——
// 让同一个数字有两个来源，就会出现「改了一个忘了另一个」。
func buildLine(lineNo int, raw dto.CreateOrderLine) (*repository.OrderLineInsert, error) {
	raw.LineType = strings.TrimSpace(raw.LineType)
	if raw.LineType != model.LineTypeDrink && raw.LineType != model.LineTypeAddon && raw.LineType != model.LineTypeMembership {
		return nil, ErrInvalidLineType
	}
	if raw.Quantity <= 0 {
		return nil, ErrInvalidQuantity
	}
	if raw.OriginalUnitPrice < 0 || raw.UnitPrice < 0 || raw.PriceDiscountAmount < 0 || raw.CouponDiscountAmount < 0 {
		return nil, ErrInvalidAmount
	}
	couponID := normalizedID(raw.CouponID)
	campaignID := normalizedID(raw.CampaignID)

	// 券只优惠饮品：加购行与会员行带券会被拒。数据库的 order_lines_coupon_only_on_drink
	// 与 order_lines_coupon_discount_needs_coupon 也挡这两条，这里是先说清楚。
	if couponID == nil && raw.CouponDiscountAmount > 0 {
		return nil, ErrCouponNeedsDiscount
	}
	if couponID != nil && raw.CouponDiscountAmount == 0 {
		// 挂了券却抵扣 0 元：要么是客户端算错了，要么这张券在这行不生效。两种都不该落库——
		// 落下去以后，coupon-service 的核销记录里就有了一张「用了但没减钱」的券。
		return nil, ErrCouponNeedsDiscount
	}
	if couponID != nil && raw.LineType != model.LineTypeDrink {
		return nil, ErrCouponOnlyOnDrink
	}
	if raw.LineType == model.LineTypeAddon && campaignID == nil {
		return nil, ErrAddonNeedsCampaign
	}
	if campaignID != nil && raw.LineType != model.LineTypeAddon {
		return nil, ErrCampaignOnlyOnAddon
	}
	if len(raw.MembershipPlanSnapshot) > 0 && raw.LineType != model.LineTypeMembership {
		return nil, ErrMembershipPlanOnlyOnPlan
	}
	if raw.LineType == model.LineTypeMembership && len(jsonObject(raw.MembershipPlanSnapshot)) == 2 {
		// len("{}") == 2：会员行必须带套餐快照，否则「他当时买的是哪个套餐」就无从还原，
		// 续费时也算不出这次买的是升级还是平级。
		return nil, ErrMembershipPlanRequired
	}

	originalAmount := raw.OriginalUnitPrice * int64(raw.Quantity)
	discountAmount := raw.PriceDiscountAmount + raw.CouponDiscountAmount
	if discountAmount > originalAmount {
		return nil, ErrDiscountExceedsLine
	}
	payableAmount := originalAmount - discountAmount

	// device_id 不在这里定：它是订单上那台机器的副本，由调用方在设备校验通过后统一填。
	return &repository.OrderLineInsert{
		LineNo:                 lineNo,
		LineType:               raw.LineType,
		ItemID:                 normalizedID(raw.ItemID),
		ItemCode:               strings.TrimSpace(raw.ItemCode),
		ItemName:               strings.TrimSpace(raw.ItemName),
		ItemImage:              strings.TrimSpace(raw.ItemImage),
		Quantity:               raw.Quantity,
		OriginalUnitPrice:      raw.OriginalUnitPrice,
		UnitPrice:              raw.UnitPrice,
		PriceDiscountAmount:    raw.PriceDiscountAmount,
		DiscountAmount:         discountAmount,
		PayableAmount:          payableAmount,
		CouponID:               couponID,
		CouponDiscountAmount:   raw.CouponDiscountAmount,
		Specs:                  jsonObject(raw.Specs),
		SelectionSnapshot:      jsonObject(raw.SelectionSnapshot),
		CampaignID:             campaignID,
		CampaignSnapshot:       jsonObject(raw.CampaignSnapshot),
		MembershipPlanSnapshot: jsonObject(raw.MembershipPlanSnapshot),
		Remark:                 strings.TrimSpace(raw.Remark),
	}, nil
}

// generateOrderNo 生成订单号：3CYM + YmdHis + 6 位数字。
//
// 前缀与格式来自银联商务的要求（老系统的 generateOrderNo 就是这个规则，收单渠道会按
// 这个规范校验商户订单号），所以它不是可以自由发挥的命名风格。后 6 位取毫秒后三位加
// 三位密码学随机数：同一秒内的两笔订单撞号，靠的是那三位随机数。
func generateOrderNo(now time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		// crypto/rand 在 darwin/linux 上不会失败；真失败了也不该退回固定值——
		// 那会让同一秒内的订单号完全由时间戳决定，撞号从概率问题变成必然问题。
		panic(fmt.Sprintf("order-service: read random for order number: %v", err))
	}
	tail := uint32(random[0])<<16 | uint32(random[1])<<8 | uint32(random[2])
	return fmt.Sprintf("3CYM%s%03d%06d", now.Format("20060102150405"), now.UnixNano()%1000, tail%1000000)
}

// mapWriteError 把仓储的哨兵错误翻成业务层的说法。
//
// 幂等与金额相关的哨兵不在这里翻：service 已经把 repository 的那几个错误值原样再导出
// （见 service.go），controller 比的就是同一个值，中间再套一层只会多一个可能写漏的地方。
func mapWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repository.ErrCouponAlreadyUsed):
		return ErrCouponOnlyOnDrink
	case errors.Is(err, repository.ErrOrderNotFound):
		return ErrOrderNotFound
	case errors.Is(err, repository.ErrOrderNotPending):
		return ErrOrderNotPending
	case errors.Is(err, repository.ErrDuplicateRequest):
		return ErrIdempotencyConflict
	default:
		return err
	}
}

// derefString 读一个可选字符串，nil 当空串。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// normalizedID 把可选 ID 规范成 nil 或去掉空白后的值。
//
// 为什么不留空串：这些列是可空 UUID，空串进 NULLIF 才会变 NULL，任何一个忘了写的
// 调用点都会得到 22P02（invalid input syntax for type uuid: ""）。在这里统一成 nil，
// 让「没有」只有一种表示。
func normalizedID(value *string) *string {
	trimmed := strings.TrimSpace(derefString(value))
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// jsonObject 把请求里的快照规范成可以直接写进 JSONB 列的值。
//
// 空值必须变成 '{}' 而不是 NULL：这些列是 NOT NULL DEFAULT '{}'，而 DEFAULT 只在
// 语句里不写这一列时才生效——我们的 INSERT 每一列都写，所以 NULL 会直接撞 NOT NULL。
// 内容本身不校验：json.RawMessage 是 encoding/json 解出来的，非法 JSON 在解请求体那一步
// 就已经失败了。
func jsonObject(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}
