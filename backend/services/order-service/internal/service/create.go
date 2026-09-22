package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

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
	drinkLines, membershipLines := 0, 0
	// 会员行至多一行（下面紧接着就判），所以套餐 ID 存一个标量就够了，不必让它挂在
	// OrderLineInsert 上——仓储不读这个字段，它只读价格与那份拼好的快照。
	membershipPlanID := ""
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
			membershipPlanID = strings.TrimSpace(derefString(raw.MembershipPlanID))
		}
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
	// 会员行的价格与快照在这一步才定下来：**由服务端向会员域取**，请求里给的价格一概不作数。
	// 所以金额加总必须排在它后面——先加总会把客户端填的那个价格算进 payable_amount，而
	// 那正是这条路要堵掉的东西。
	if membershipLines > 0 {
		if err := s.applyMembershipPlan(ctx, lines, membershipPlanID); err != nil {
			return nil, false, err
		}
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
	// 饮品行的价格、名称与图片在这一步定下来：**由服务端按 itemId 现查**，请求里给的那几格
	// 一概不作数。它排在设备之后，是因为「这一杯是不是这台设备上的」要拿解析出来的设备去比；
	// 排在任何 DB 事务之前，是因为它是一次跨服务调用（事务开在仓储内部）。
	//
	// 金额加总必须排在它后面——先加总会把客户端填的那个价格算进 payable_amount，而那正是
	// 这条路要堵掉的东西。
	if drinkLines > 0 {
		if err := s.applyDrinkPricing(ctx, lines, in.UserID); err != nil {
			return nil, false, err
		}
	}

	var originalAmount, discountAmount int64
	for _, line := range lines {
		originalAmount += line.OriginalUnitPrice * int64(line.Quantity)
		discountAmount += line.DiscountAmount
	}
	// membershipId / membershipSnapshot 是**下单这一单时用的会员资格**（按会员价卖饮品时
	// 那份「他当时是不是会员、什么等级」的快照），与「这一单是不是在买会员」无关，所以
	// 这里不再检查它们的配对关系：
	//
	//   * 买会员的人**还没有会员**——这一单正是去给他开会员的，要求他先给一个会员 ID
	//     等于让首次购买永远下不了单；
	//   * 按会员价买咖啡的人有会员、但没有会员行。
	//
	// 曾经这两条都在（会员行必须要 membershipId、非会员行不许带），两条都是把「定价依据」
	// 读成了「本单买了什么」。真要用它定价时，它该由服务端从会员域取（那条路是
	// GetMemberPriceEntitlement），而不是收客户端填的。

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
	// 会员行只认一个套餐 ID：价格、时长、快照都由服务端拿它去会员域取（见 applyMembershipPlan）。
	// 请求里如果还带着价格（originalUnitPrice 这些是各类型行共用的字段），一律**忽略**——
	// 不是「校验它对不对」，而是它压根不参与计算：这样客户端算错了也不会把用户带到一个
	// 与我们实际收的钱不一样的价钱上。
	if raw.LineType != model.LineTypeMembership && raw.MembershipPlanID != nil {
		return nil, ErrMembershipPlanIDNotAllowed
	}
	// 饮品行只收一个 itemId：名称、图片与价格都由服务端拿它去目录里查（见 applyDrinkPricing）。
	// 与会员行那一段同一条规矩，只是形状不同——这里有 uuid 可判，所以格式错的当场挡掉，
	// 不必去咖啡机域换一个 InvalidArgument 回来。
	if raw.LineType == model.LineTypeDrink {
		itemID := strings.TrimSpace(derefString(raw.ItemID))
		if itemID == "" {
			return nil, ErrDrinkItemIDRequired
		}
		if _, err := uuid.Parse(itemID); err != nil {
			return nil, ErrDrinkItemIDInvalid
		}
	}
	if raw.LineType == model.LineTypeMembership {
		planID := strings.TrimSpace(derefString(raw.MembershipPlanID))
		if planID == "" {
			return nil, ErrMembershipPlanIDRequired
		}
		if _, err := uuid.Parse(planID); err != nil {
			// 不合法就不必去问会员域了：那一次往返只会拿回一个 InvalidArgument，
			// 而客户端要处理的是同一件事。
			return nil, ErrMembershipPlanIDInvalid
		}
		if raw.Quantity != 1 {
			// 会员行的金额是「套餐价 × 数量」，但买到的时长只看套餐（period × period_count）：
			// 放过 quantity=3 就是收三份钱、开一期会员。
			return nil, ErrMembershipQuantityInvalid
		}
	}

	// 会员行与饮品行的价格、名称都由服务端填（见 applyMembershipPlan / applyDrinkPricing），
	// 所以这里把它们清掉：请求里那几个共用字段对这两种行没有任何意义，留着只会让下面那几条
	// 恒等式、以及后面的金额加总读到一个客户端说了算的数。
	//
	// 加购行不在其中：加购品今天没有商品目录（见 CreateOrderLine 上那段说明），它的价格
	// 只能是调用方给的，所以那几个共用字段在它身上是有效的。
	originalUnitPrice, unitPrice := raw.OriginalUnitPrice, raw.UnitPrice
	priceDiscount, couponDiscount := raw.PriceDiscountAmount, raw.CouponDiscountAmount
	itemID, itemCode, itemName, itemImage := normalizedID(raw.ItemID),
		strings.TrimSpace(raw.ItemCode), strings.TrimSpace(raw.ItemName), strings.TrimSpace(raw.ItemImage)
	membershipSnapshot := []byte("{}")
	switch raw.LineType {
	case model.LineTypeMembership:
		originalUnitPrice, unitPrice, priceDiscount, couponDiscount = 0, 0, 0, 0
		itemID, itemCode, itemName, itemImage = nil, "", "", ""
	case model.LineTypeDrink:
		// **itemId 留着**：它是去目录里查这一杯的那把钥匙，其余三格作废。
		//
		// couponDiscountAmount 也不清：券抵多少是券域的事，这一版仍由调用方给（见
		// CreateOrderLine 的 couponDiscountAmount）。
		originalUnitPrice, unitPrice, priceDiscount = 0, 0, 0
		itemCode, itemName, itemImage = "", "", ""
	}

	originalAmount := originalUnitPrice * int64(raw.Quantity)
	discountAmount := priceDiscount + couponDiscount
	// 饮品行的原价这一刻还不知道（要去目录里查），这条恒等式它一上来就过不了——原价是 0、
	// 而券可能抵了钱。等 applyDrinkPricing 把价格填好，它会在那边重判一次。
	if raw.LineType != model.LineTypeDrink && discountAmount > originalAmount {
		return nil, ErrDiscountExceedsLine
	}
	payableAmount := originalAmount - discountAmount

	// device_id 不在这里定：它是订单上那台机器的副本，由调用方在设备校验通过后统一填。
	return &repository.OrderLineInsert{
		LineNo:                 lineNo,
		LineType:               raw.LineType,
		ItemID:                 itemID,
		ItemCode:               itemCode,
		ItemName:               itemName,
		ItemImage:              itemImage,
		Quantity:               raw.Quantity,
		OriginalUnitPrice:      originalUnitPrice,
		UnitPrice:              unitPrice,
		PriceDiscountAmount:    priceDiscount,
		DiscountAmount:         discountAmount,
		PayableAmount:          payableAmount,
		CouponID:               couponID,
		CouponDiscountAmount:   couponDiscount,
		Specs:                  jsonObject(raw.Specs),
		SelectionSnapshot:      jsonObject(raw.SelectionSnapshot),
		CampaignID:             campaignID,
		CampaignSnapshot:       jsonObject(raw.CampaignSnapshot),
		MembershipPlanSnapshot: membershipSnapshot,
		Remark:                 strings.TrimSpace(raw.Remark),
	}, nil
}

// drinkStatusOnShelf 是咖啡机域 drinks.status 里「在售」那一档，与那份契约上的取值逐字一致。
//
// 本服务只认这一个值：**除它以外的一切都按不卖处理**。今天只有 on_shelf 与 off_shelf 两档，
// 而将来多出来的那一档（比如「仅设备可见」）在这里默认是拒——一个没见过的状态按「卖」处理，
// 是让一次词表变更变成一次无声的放行。
const drinkStatusOnShelf = "on_shelf"

// applyDrinkPricing 把饮品行的名称、图片、价格与优惠填上——**一样都不来自请求**。
//
// 这是「小程序下单买一杯饮品」这条路上唯一一处跨服务的读，和 applyMembershipPlan 一样放在
// buildLine 之外：buildLine 是个纯函数（不碰网络、不碰时钟），而它承担的校验与算钱正是最
// 需要能单独测的部分。
//
// # 为什么非要问两次
//
// 价格在饮品目录里（coffee-machine-service 的 drinks：原价 / 会员价 / 提货码价），而**谁有
// 资格按会员价买**在会员域——两个事实，两个域，本服务一个都没有。收客户端填的那一份，就等于
// 让客户端定价：一条 originalUnitPrice=1 的请求能一分钱买走一杯美式，而这在库上看起来完全正常。
//
// # 价格怎么定
//
//	original_unit_price = 目录价（always）
//	unit_price          = 会员价（这个人此刻直接享会员价，且目录真给这一杯配了会员价）
//	                      否则目录价
//	price_discount      = original − unit
//
// 三个例外都按原价卖，且都不是「便宜一点」而是「不敢便宜」：
//
//   - vip_price 为 0：目录没给这一杯配会员价。0 不是「会员价 0 元」，按它卖就是白送。
//   - vip_price 不小于原价：一行坏数据（后台的写接口不拦这个）。照它算会得出一个负的优惠额，
//     而 order_lines 上 price_discount_amount 非负。
//   - 会员域没答上来：整条路停在 ErrMemberPriceUnavailable（503），**不退回原价继续下单**——
//     那会把一次下游抖动变成「悄悄按原价卖给了会员」，用户不会知道，我们也不会。
//
// 多收的那一次是可退的，0 元卖出去的那一次不是，所以三条例外都往严的方向倒。
//
// # 拿回来的是「此刻」的一份拷贝
//
// 与套餐快照同一条道理：它落进订单行就不再变。运营改价、下架饮品都不影响已经卖出去的那一单，
// 而订单行上的 item_id 仍然指着目录里那一行，事后要对账拿得到。
func (s *OrderService) applyDrinkPricing(ctx context.Context, lines []*repository.OrderLineInsert, userID string) error {
	if s.devices == nil {
		// 没配读端就不下单：退化成「那就用请求里那份」正是这条路要堵的东西（见 resolveDevice）。
		return fmt.Errorf("%w: device reader is not configured", ErrDrinkLookupUnavailable)
	}
	if s.plans == nil {
		return fmt.Errorf("%w: membership plan reader is not configured", ErrMemberPriceUnavailable)
	}
	// 资格一单问一次，不是一行问一次：会员资格长在人身上，与有几杯饮品无关。
	entitlement, err := s.plans.Entitlement(ctx, userID)
	if err != nil {
		// 读端说的「会员服务没答上来」原样往上传，理由与 applyMembershipPlan 那处一样：
		// 再包一层会让同一句话在响应体里出现两次。
		if errors.Is(err, ErrMemberPriceUnavailable) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrMemberPriceUnavailable, err)
	}

	for _, line := range lines {
		if line.LineType != model.LineTypeDrink {
			continue
		}
		drink, found, err := s.devices.GetDrink(ctx, derefString(line.ItemID))
		if err != nil {
			if errors.Is(err, ErrDrinkLookupUnavailable) {
				return err
			}
			return fmt.Errorf("%w: %v", ErrDrinkLookupUnavailable, err)
		}
		if !found {
			return ErrDrinkNotFound
		}
		// 已下架的不卖。**这条是下单这条路独有的判断**：设备回调那条路读的是同一条饮品的
		// 同一个 status，但它有意不拦（钱已经在机器上收过了，拦了就是丢单）。分歧点不在
		// status 那一列，在「钱收没收到」——所以判断留在本服务，咖啡机域只回答事实。
		if drink.Status != drinkStatusOnShelf {
			return ErrDrinkOffShelf
		}
		// 这一杯得挂在订单那台设备上。空 device_id 的饮品行（库里真有没挂设备的遗留行）
		// 不拦：没有可比的设备，不是「挂错了设备」。
		if line.DeviceID != nil && drink.DeviceID != "" && drink.DeviceID != *line.DeviceID {
			return ErrDrinkDeviceMismatch
		}

		originalUnitPrice := drink.Price
		unitPrice := originalUnitPrice
		if entitlement.GrantsMemberPrice && drink.VipPrice > 0 && drink.VipPrice < originalUnitPrice {
			unitPrice = drink.VipPrice
		}

		// 单价差 × 数量：price_discount_amount 是**行级**金额，不是单价差。001 的列注释
		// 写得很直白——「这个会员价一共省了多少」= sum(price_discount_amount)，而两格
		// 单价的差只说明「一杯省多少」，另一格 unit_price 明确不参与任何恒等式。
		//
		// 少乘这一次数量**撞不上任何约束**，这正是它活下来的原因：下面两条式子同时少算
		// 同一笔钱，order_lines_discount_breakdown 与 order_lines_payable_matches 照样
		// 成立，结账页那一行也看不出异常。买两杯会员价拿铁，用户被多收一份差价（原价
		// 1800 会员价 1500 → 应付 3300，而正确值是 3000）。
		originalAmount := originalUnitPrice * int64(line.Quantity)
		priceDiscount := (originalUnitPrice - unitPrice) * int64(line.Quantity)

		// item_code 存**机器报的那个编号**（product_num）：事后拿机器流水来对账时，唯一能
		// 对上的就是它。与设备单那条路存机器报的编号同一条理由，只是那边编号来自报文、
		// 这边来自目录。目录里没编号（后台手工建、不参与同步的饮品）就留空。
		line.ItemCode = drink.ProductNum
		line.ItemName = drink.Name
		line.ItemImage = drink.Image
		line.OriginalUnitPrice = originalUnitPrice
		line.UnitPrice = unitPrice
		line.PriceDiscountAmount = priceDiscount
		// 券抵多少仍是调用方给的（见 CreateOrderLine），而且它本来就是**行级**的（buildLine
		// 与加购行都这么用）。所以这里只重算「价格优惠进来了之后」的那两个数——恒等式在库上
		// 还有一道 CHECK，算错了会以 23514 收场。
		line.DiscountAmount = priceDiscount + line.CouponDiscountAmount
		line.PayableAmount = originalAmount - line.DiscountAmount
		if line.PayableAmount < 0 {
			// buildLine 那条校验在饮品行上跳过了（那时还不知道原价），在这里补上：券抵得比
			// 这一行还贵。库上 order_lines_payable_matches 也会拒，但回一句人话更好。
			return ErrDiscountExceedsLine
		}
	}
	return nil
}

// applyMembershipPlan 把会员行的价格、名称与套餐快照填上——**全部来自会员域**。
//
// 这是「下单买会员」这条路上唯一一处跨服务的读，放在这里而不是 buildLine 里，是因为
// buildLine 是个纯函数（不碰网络、不碰时钟），而它承担的校验与算钱正是最需要能单独测的部分。
//
// # 为什么非要问一次
//
// 那份快照决定三件事：用户付多少钱（price_cents → 行的 original_unit_price 与
// payable_amount）、买到多长（period / period_count）、会员价怎么来（member_price_mode）。
// 三件都是会员域的事实。收客户端填的那一份就等于让客户端定价——一份 originalUnitPrice=1、
// 快照写着年卡的请求能花一分钱开一年会员，而这在库上看起来完全正常。
//
// # 拿回来的是「此刻」的一份拷贝
//
// 它落进订单行就不再变：套餐改价、改时长、下架都不影响已经卖出去的那一单，而付款之后
// order.paid 带回来的也正是这一份（见 repository.SettlePayment）——会员域据此开通，
// 它读的同样是快照，不回头现查套餐。往返一圈，价格与时长只有一个来源。
func (s *OrderService) applyMembershipPlan(ctx context.Context, lines []*repository.OrderLineInsert, planID string) error {
	if s.plans == nil {
		// 没配读端就不下单：退化成「那就用请求里那份」正是这条路要堵的东西。与设备那条
		// 同一个口径（见 resolveDevice）。
		return fmt.Errorf("%w: membership plan reader is not configured", ErrMembershipPlanUnavailable)
	}
	plan, found, err := s.plans.Get(ctx, planID)
	if err != nil {
		// 读端说的「会员服务没答上来」原样往上传：它就是这一层要说的话，再包一层会变成
		// 「membership service is unavailable: membership service is unavailable」——而这句话
		// 会原样进 503 响应体的 errorMessage。其余错误（读端自己坏了之类）才需要带上原因。
		if errors.Is(err, ErrMembershipPlanUnavailable) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrMembershipPlanUnavailable, err)
	}
	if !found {
		return ErrMembershipPlanNotFound
	}
	snapshot, err := json.Marshal(dto.MembershipPlanSnapshot{
		PlanID:                      plan.ID,
		PlanCode:                    plan.Code,
		PlanName:                    plan.Name,
		PriceCents:                  plan.PriceCents,
		Period:                      plan.Period,
		PeriodCount:                 plan.PeriodCount,
		AutoRenew:                   plan.AutoRenew,
		MemberPriceMode:             plan.MemberPriceMode,
		MemberPriceCouponTemplateID: plan.MemberPriceCouponTemplateID,
		MemberPriceCouponsPerPeriod: plan.MemberPriceCouponsPerPeriod,
	})
	if err != nil {
		// 入参全是标量，编不出来只可能是代码写错了（与仓储的 mustJSON 同一条判断）。
		return fmt.Errorf("encode membership plan snapshot: %w", err)
	}

	for _, line := range lines {
		if line.LineType != model.LineTypeMembership {
			continue
		}
		// item_id 是套餐的值引用，编码与名称是下单这一刻的副本。item_image 留空：套餐没有
		// 图片这一说，而客户端给的图我们不认。
		line.ItemID = &plan.ID
		line.ItemCode = plan.Code
		line.ItemName = plan.Name
		// unit_price 与 original_unit_price 同值：会员套餐没有「标价」与「成交价」之分
		// （会员价那条优惠是给饮品的，不是给会员套餐本身的），优惠额一律为零。
		line.OriginalUnitPrice = plan.PriceCents
		line.UnitPrice = plan.PriceCents
		line.PriceDiscountAmount = 0
		line.CouponDiscountAmount = 0
		line.DiscountAmount = 0
		line.PayableAmount = plan.PriceCents
		line.MembershipPlanSnapshot = snapshot
	}
	return nil
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

// userMatches 判断这一单是不是这个调用方的。
//
// nil（设备单没有用户，见 order/005）**不等于任何调用方**，包括空串：它意味着这张单谁都
// 不属于，不是「谁都能看/能付/能取消」。写成 `order.UserID == nil || *order.UserID != caller`
// 是同一件事，但三处调用点各写一遍迟早会有一处漏掉 nil 判断——漏掉的那处就是一次越权
// （nil 与空串调用方相等，后台那条路正是空串）。
func userMatches(orderUserID *string, callerID string) bool {
	return orderUserID != nil && *orderUserID == callerID
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
