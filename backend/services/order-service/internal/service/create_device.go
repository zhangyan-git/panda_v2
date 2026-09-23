package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// deviceOrderPaymentMethod 是设备单的支付方式标签。
//
// 与老系统对齐（那边刷卡机的支付方式写的就是它）。它**不是** payment-service 的渠道 ID：
// 这一单没有支付单，payment_method 在这里只是「钱是怎么收的」的一句说明，对账要靠刷卡机
// 那一侧的凭据，不靠本库这一列。
const deviceOrderPaymentMethod = "card_pay"

// deviceOrderReason 是设备单建单时写进状态流水的原因，也是订单备注的前缀。
const deviceOrderReason = "设备刷卡购买"

// CreateDeviceOrderInput 是一次设备单建单的全部输入。
//
// 这里**没有 UserID**：线下刷卡机上的这一单不属于任何用户（钱在机器上收过了），
// orders.user_id 写 NULL，见 migrations/order。也没有幂等键——这条路的幂等键是对方单号本身。
type CreateDeviceOrderInput struct {
	// ThirdPartyOrderNo 是对方单号，也是这条路唯一的幂等键。
	ThirdPartyOrderNo string
	// DeviceSerial 是机器自己认识的那个编号（devices.serial_unique）。门店与设备 uuid 都由
	// 它推出来，请求里没有让调用方直接给 store_id 的字段（见 resolveDevice 的同一条口径）。
	DeviceSerial string
	// DrinkCode 是机器报的饮品编号，交给 GetDeviceDrink 去认是哪一杯。
	DrinkCode string
	// Amount 是设备上报的成交金额（分）。为 0 时回退到饮品目录价（照老系统）。
	//
	// ⚠️ **不校验**，见 CreateDeviceOrder 里的说明。
	Amount int64
	// BrewFailed 是设备上报的出饮结果：true=出饮失败。失败也建单（钱收了），
	// 但订单的履约汇总标成 failed 留人工。
	BrewFailed bool
	Remark     string
}

// CreateDeviceOrderResult 是建单结果。
type CreateDeviceOrderResult struct {
	OrderID string
	OrderNo string
	// Created=false 表示这次命中的是已有的那一张（对方重投），什么都没写。
	Created bool
}

// CreateDeviceOrder 落一笔线下刷卡机的订单：钱已经在机器上收过了，这里把既成事实记下来。
//
// # 为什么是「直接落成已支付」
//
// 刷卡机是先收钱再出杯的，回调到达时那笔交易已经完成。按小程序那条路先落 pending_payment
// 再等支付结果，会造出一张永远等不到支付的单（我们这里根本没有那笔支付），关单扫描还会
// 把它当超时单关掉——而钱是真的收了。所以这里的 status 直接是 paid、paid_at 是收到回调的
// 时刻、payment_no 为空。
//
// # 顺序：设备 → 饮品 → 算钱 → 建单
//
// GetDeviceBySerial(序列号) → 设备（**点位由设备定**）→ GetDeviceDrink(设备 uuid, 机器报的
// 编号) → 饮品（目录价）→ 建单。与 resolveDevice 同一条口径：请求里没有点位，收调用方给的
// 点位等于让订单挂在一个设备并不在的点位上。
//
// 设备**不判 status**：这台机器此刻是不是被我们停用了，与「刚才在它上面发生过一笔交易」
// 无关。停用的机器该由运维去处理，而这一单是已经发生的事实——按 status 拒收会让钱收了、
// 单没了，那比停用本身糟得多。
//
// # 两个「没有」是两个不同的结论，而且分档也不一样
//
// 设备找不到（ErrDeviceNotFound）与这杯饮品找不到（ErrDrinkNotFound）分开报：partner-service
// 会把它们转达给合作方，而对方该做的事完全不同——前者是「这台机器没在你们那儿登记」（它
// 落在 NotFound→404），后者是「你报的这个编号在这台机器上不存在」（它落在 InvalidArgument→400，
// 见 rpc.createDeviceOrderError 的分档表）。
//
// 两者都**不是** 5xx：它们是「你问的这个东西没有」的确定答案，重发一百次也一样——区别只在于
// 前者要合作方去登记设备，后者要他改报文里的编号。
//
// # 金额由设备给，且**不校验**（已知缺口）
//
// 设备报的成交价原样采信，为 0 才回退到饮品目录价。**没有任何反向核对**：我们不比对
// 设备报的金额与饮品目录价是否相符，也没有对账兜底——老系统同此（`sync_order_handler.go`
// 的建单路径：拿设备给的价格直接落库），方案把「设备单金额对账」列为后续项。
//
// 后果说清楚：一台被做过手脚、或者自己算错了的机器可以报任意金额，而订单会如实记下它。
// 这条缺口的边界是「钱不是我们收的」——我们手里没有那笔交易的凭据，也就没有可核对的基准。
// 真要对账时，基准在刷卡机/厂商那边，不在本服务里。
//
// # 幂等
//
// 按对方单号幂等，靠 orders_third_party_order_no_key 这条部分唯一索引兜（不是先查后插，
// 见 repository.CreateDeviceOrder）。命中时**不报错**：对方重投是这条路的常态（网络重放、
// 机器自己重试），返回既有那张单、created=false。
//
// 命中时那一张单还必须是**同一类**设备单：单号空间与取货码那条共用，撞上一张取货码单时回
// ErrThirdPartyOrderNoTaken，而不是把它的单号当成你这一次的结果。这一条不在这里判——它要看
// 既有那一行的 payment_method，判据与写那一步在同一个事务里（见仓储）。
//
// # 不发事件
//
// 理由见 repository.CreateDeviceOrder 的说明：出杯已经在机器上发生过，order.paid 是
// 「该去做这一杯」的触发点，对着一条已经出过杯的订单发它等于让机器再出一杯。
func (s *OrderService) CreateDeviceOrder(ctx context.Context, in CreateDeviceOrderInput) (*CreateDeviceOrderResult, error) {
	in.ThirdPartyOrderNo = strings.TrimSpace(in.ThirdPartyOrderNo)
	in.DeviceSerial = strings.TrimSpace(in.DeviceSerial)
	in.DrinkCode = strings.TrimSpace(in.DrinkCode)
	if in.ThirdPartyOrderNo == "" {
		// 没有幂等键这条路就不成立：重投会变成第二张单，而钱只收了一次。
		return nil, ErrThirdPartyOrderNoRequired
	}
	if in.DeviceSerial == "" {
		return nil, ErrDeviceSerialRequired
	}
	if in.DrinkCode == "" {
		return nil, ErrDrinkCodeRequired
	}
	if s.devices == nil {
		// 与 resolveDevice 同一条：没有可用的读端就不建单——退化成「那就按请求里的信息来」
		// 在这里等于没有设备、没有饮品、没有价格。
		return nil, fmt.Errorf("%w: device reader is not configured", ErrDeviceLookupUnavailable)
	}

	device, found, err := s.devices.GetBySerial(ctx, in.DeviceSerial)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDeviceLookupUnavailable, err)
	}
	if !found {
		return nil, ErrDeviceNotFound
	}
	// 用设备 uuid 而不是序列号去查饮品：饮品挂在设备 uuid 上（drinks 那一行就是某台机器上
	// 的一杯），序列号只是进入这条路的那把钥匙。
	drink, found, err := s.devices.GetDeviceDrink(ctx, device.ID, in.DrinkCode)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDrinkLookupUnavailable, err)
	}
	if !found {
		return nil, ErrDrinkNotFound
	}

	original, discount, payable, err := deviceOrderAmounts(in.Amount, drink.Price)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	// 出饮失败的单照样建：钱收了，事实是「这一杯没出来」，不是「这一单不存在」。
	// 履约汇总交给人工——order_lines 不存任务状态，履约细节归 fulfillment-service。
	fulfillmentStatus := model.FulfillmentPending
	if in.BrewFailed {
		fulfillmentStatus = model.FulfillmentFailed
	}
	// 点位来自设备；设备还没挂点位时订单上没有点位（与 resolveDevice 的处理一致：可以下单，
	// 后台按点位筛时这一单不出现在任何点位下，这是如实记录）。
	// store_name 只能是空的：设备那条读不带点位名，我们不会为了一个展示字段再问一次点位域。
	storeID := normalizedID(&device.StoreID)

	result, created, err := s.repository.CreateDeviceOrder(ctx, repository.CreateDeviceOrderParams{
		ThirdPartyOrderNo: in.ThirdPartyOrderNo,
		OrderNo:           generateOrderNo(now),
		Source:            model.SourceDevice,
		FulfillmentStatus: fulfillmentStatus,
		StoreID:           storeID,
		DeviceID:          &device.ID,
		DeviceNo:          device.SerialUnique,
		OriginalAmount:    original,
		DiscountAmount:    discount,
		PayableAmount:     payable,
		PaymentMethod:     deviceOrderPaymentMethod,
		// paid_at 是收到回调的时刻，不是刷卡机上真正收钱的那一刻——回调里没有那个时间戳，
		// 这是本批的已知缺口。两时刻之间只隔一次网络往返，用它记账不会错到能影响对账的量级。
		PaidAt: now,
		Remark: deviceOrderRemark(in.Remark),
		// 状态流水那一行的原因：这条路上钱是在机器上刷卡收的（取货码那条用的是它自己那一句，
		// 见 CreateDeviceOrderParams.TransitionReason）。
		TransitionReason: deviceOrderReason,
		Line: &repository.OrderLineInsert{
			// 一单一行：设备单一次只卖一杯，所以 line_no 恒为 1。
			LineNo:   1,
			LineType: model.LineTypeDrink,
			// item_id 是那条饮品（值引用）；item_code 存**机器报的那个编号**——它是这一杯
			// 在下单当时的代号，也是事后拿机器流水来对时唯一能对上的东西。
			ItemID:   &drink.ID,
			ItemCode: in.DrinkCode,
			ItemName: drink.Name,
			// 名称与图是同一次目录读带回来的：后台列表上这一杯要能显形，只落名字不落图
			// 等于白读一个字段。
			ItemImage:         drink.Image,
			Quantity:          1,
			OriginalUnitPrice: original,
			UnitPrice:         payable,
			// 设备单没有券、没有会员行、没有加购活动，所以优惠整块来自「成交价与目录价的差」，
			// 记在标价优惠那一格（券优惠恒为 0，order_lines_coupon_only_on_drink 也要求这样）。
			PriceDiscountAmount: discount,
			DiscountAmount:      discount,
			PayableAmount:       payable,
			// 设备单没有规格、选品、加购活动与套餐快照。那四列是 NOT NULL DEFAULT '{}'，
			// 而 INSERT 每一列都写值——留 nil 会撞 NOT NULL（漏掉 campaign_snapshot 这条
			// 正是集成用例先抓出来的），所以四列都显式写空对象。
			Specs:                  jsonObject(nil),
			SelectionSnapshot:      jsonObject(nil),
			CampaignSnapshot:       jsonObject(nil),
			MembershipPlanSnapshot: jsonObject(nil),
			DeviceID:               &device.ID,
		},
	})
	if err != nil {
		return nil, mapWriteError(err)
	}
	return &CreateDeviceOrderResult{OrderID: result.OrderID, OrderNo: result.OrderNo, Created: created}, nil
}

// deviceOrderAmounts 算设备单的三个金额（分），并保证它们能过库上那几条恒等式。
//
//	payable  = 设备报的成交价；报了 0（或负数）就用饮品目录价
//	original = max(目录价, payable)
//	discount = original - payable
//
// 三个数都非负，且 payable = original − discount 与 discount = price + coupon（coupon 恒 0）
// 同时成立——库里 orders_payable_matches / order_lines_payable_matches /
// order_lines_discount_breakdown 逐条都是这么算的，算歪一个就是 23514。
//
// 「取两者较大者」是这里唯一一处判断，理由是它同时满足两种成交情形：
//
//   - 设备价**低于**目录价：original 留目录价，差额记成标价优惠。这一单看起来就是
//     「按目录价卖、优惠了 X」，与小程序那条路上会员价的记法一致，事后能解释清楚。
//   - 设备价**高于**目录价：original 只能等于 payable，优惠为 0。这里有一处**已知的失真**：
//     我们付出了一个比目录价还高的成交价，订单上却看不出「贵了」，因为 order_lines 上没有
//     一列能表达「负优惠」，而 original < payable 会直接违反 payable_matches。
//     为什么允许它发生：金额不校验是这条路的前提（见 CreateDeviceOrder），订单记的是机器的
//     事实、不是我们的定价。真要在库里表达它，得先改那几条恒等式的形状。
func deviceOrderAmounts(deviceAmount, catalogPrice int64) (original, discount, payable int64, err error) {
	if catalogPrice < 0 {
		// 目录价是负数说明饮品库那一行坏了。它不是「这一杯卖负钱」，而这条路上没有别的价格
		// 可回退——按「问不到价格」处理（503），让合作方稍后重投，而不是落一张负金额的单。
		return 0, 0, 0, fmt.Errorf("%w: drink catalog price is negative", ErrDrinkLookupUnavailable)
	}
	if deviceAmount < 0 {
		// 没有「倒找钱」的成交。回退到目录价而不是把负数记下来：负数过不了库上的 CHECK，
		// 而这一单的钱已经收了，不能因为设备报了个坏数字就把它丢掉。
		deviceAmount = 0
	}
	payable = deviceAmount
	if payable == 0 {
		payable = catalogPrice
	}
	original = catalogPrice
	if payable > original {
		original = payable
	}
	discount = original - payable
	return original, discount, payable, nil
}

// deviceOrderRemark 拼订单备注：固定前缀 + 设备给的备注。
//
// 前缀是写死的，因为「这一单从哪来」不该由调用方决定（partner-service 转达的是合作方的
// 文本，那部分只能当备注，不能当来源）。
func deviceOrderRemark(remark string) string {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return deviceOrderReason
	}
	return deviceOrderReason + "：" + remark
}
