package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// pickupOrderPaymentMethod 是取货码单的支付方式标签。
//
// 与刷卡机那条（card_pay）分开：两张单的钱来源不同，一张是机器收的现金/刷卡，一张是**这台
// 机器上的咖啡余额**。后台按 payment_method 筛的时候，这两件事必须分得开——把它们并成一句
// 「设备单」会让「这台机器这个月卖了多少」和「它的余额被扣了多少」变成同一个数。
//
// 它同样**不是** payment-service 的渠道 ID：这一单没有支付单，钱的来源是我们自己的设备余额，
// 而扣减的凭据在 device_balance_ledger 上（reference_type = pickup_code）。
const pickupOrderPaymentMethod = "pickup_code"

// pickupOrderReason 是取货码单建单时写进状态流水的原因，也是订单备注的前缀。
const pickupOrderReason = "取货码购买"

// CreatePickupOrderInput 是一次取货码建单的全部输入。
//
// 这里没有金额，也没有 UserID：金额由我们定价（见 pickupOrderPrice），而这一单不属于任何
// 用户——钱从设备余额里扣，机器前面那个人在我们库里没有账号（orders.user_id 写 NULL）。
type CreatePickupOrderInput struct {
	// ThirdPartyOrderNo 是对方单号。它在这条路上**有两个身份**：余额流水的 request_id 与
	// 订单表的 third_party_order_no。两处必须是同一个值（见 CreatePickupOrder 的说明）。
	ThirdPartyOrderNo string
	// DeviceSerial 是机器自己认识的那个编号（devices.serial_unique）。钱扣的就是这台设备。
	DeviceSerial string
	// DrinkCode 是机器报的饮品编号，交给 GetDeviceDrink 去认是哪一杯，价格也从那一行来。
	DrinkCode string
	// PickupPassword 是顾客敲的那个取货码。**原样转下去**，比对在咖啡机域的扣减事务里做。
	//
	// 这里不 trim 也不判空：它是顾客敲进去的一串字符，前后有没有空格只有比对那一侧知道；
	// 而「空码」与「错码」在那边本来就回同一个结论。
	PickupPassword string
	Remark         string
}

// CreatePickupOrderResult 是建单结果。
type CreatePickupOrderResult struct {
	OrderID string
	OrderNo string
	// Created=false 表示这次命中的是已有的那一张（对方重投），什么都没写。
	Created bool
}

// CreatePickupOrder 落一笔取货码订单（方案 §四的第二条路）：钱从**这台设备的咖啡余额**里扣。
//
// # 与刷卡机那条的三处不同
//
//  1. **定价在我们这边**。报文里没有金额，价格取这一杯的取货码价、为 0 回落目录价、两个都是
//     0 就拒（pickupOrderPrice）。刷卡机那条相反（设备报价优先）——差别不在口味上：那边钱是
//     机器收的，我们手里没有可核对的基准，只能记既成事实；这边的钱是我们从自己账上扣的，
//     价格必须是我们自己认的。
//  2. **先扣钱、后建单**。扣减在咖啡机域与写一行余额流水是同一个事务（它是那条流水唯一的
//     写入方）。顺序反过来的话，中间断了是「建了单没扣钱」——白送一杯；现在的顺序断了只是
//     「扣了没建单」，而那一笔在流水上留了痕，拿对方单号能补出来。
//  3. **验证码校验不在本服务**。原样转给扣减那一侧比（它持有那一列，而且比对落在设备行的行锁
//     之内）。在这里比会多出一个 TOCTOU 窗口：读码与扣钱之间码可能被后台改掉；也会让「码对
//     不对」与「钱够不够」变成两次各自成立的判断，而它们必须在同一个事务里一次定下来。
//
// # 一个键，两个身份——这是幂等的全部
//
// ThirdPartyOrderNo 同时是余额流水的 request_id 与订单的 third_party_order_no：
//
//	扣减侧  device_balance_ledger_one_per_request（request_id 非空的唯一索引）
//	建单侧  orders_third_party_order_no_key（third_party_order_no 非空的唯一索引）
//
// 两次幂等都命中时，这一次重投什么都不写，返回既有那张单、created=false，**不报错**——对方
// 重投是回调类接口的常态。两个索引必须用同一个值：拆成两个值的话，重投可能只撞上一个，
// 于是要么扣两次钱、要么建两张单。空值在这里挡下——两个部分唯一索引都只索引该列**非空**的
// 行，空串会从它们下面溜过去，而绕过索引的重投是查不出来的那种错。
//
// # 那个单号还有一个邻居：刷卡机那条路
//
// 两条设备接口共用**同一个单号空间**（唯一索引只认单号，不认它来自哪条路）。所以动钱之前先
// 按单号查一次，查到的那张单不是取货码单就回 ErrThirdPartyOrderNoTaken——检查放在扣款之前
// 是必需的：放到建单那一步再发现的代价是钱扣了、而这一笔挂在一张与它无关的订单上。
//
// # 重投建出来的单，金额按**流水**上那一笔
//
// 重投命中幂等时一分钱都不会再扣，所以金额不能照当前价算：两次之间这一杯可能被改过价，
// 那样落出来的单只动过 1400 却记着 2000，差额在库里没有任何一处能解释。金额取自扣减那一次的
// 回答（它带回了当初那一笔的金额），目录价与优惠仍按当前目录价重算——那是展示用的标价。
//
// # 设备不判 status
//
// 与刷卡机那条同一条口径：这台机器此刻是不是被我们停用了，与「刚才在它上面发生过一笔交易」
// 无关。停用的机器该由运维处理，而这一笔是已经发生的事实——按 status 拒收会让钱扣了、
// 单没了，那比停用本身糟得多。
//
// # 不发事件
//
// 理由与刷卡机那条相同（见 repository.CreateDeviceOrder）：出杯已经在机器上发生过了，
// order.paid 是「该去做这一杯」的触发点，对着一条已经出过杯的订单发它等于让机器再出一杯。
func (s *OrderService) CreatePickupOrder(ctx context.Context, in CreatePickupOrderInput) (*CreatePickupOrderResult, error) {
	in.ThirdPartyOrderNo = strings.TrimSpace(in.ThirdPartyOrderNo)
	in.DeviceSerial = strings.TrimSpace(in.DeviceSerial)
	in.DrinkCode = strings.TrimSpace(in.DrinkCode)
	if in.ThirdPartyOrderNo == "" {
		// 没有幂等键这条路就不成立：它挡的是**扣两次**与**建两张单**，而这两件事都是钱上的错。
		// 空串还会绕过流水表那条 `request_id <> ''` 的部分唯一索引，数据库那一层兜不住。
		return nil, ErrThirdPartyOrderNoRequired
	}
	if in.DeviceSerial == "" {
		return nil, ErrDeviceSerialRequired
	}
	if in.DrinkCode == "" {
		return nil, ErrDrinkCodeRequired
	}
	if s.devices == nil {
		// 与 CreateDeviceOrder 同一条：没有可用的读端就不建单。这条路上更严重——退化成
		// 「那就按请求里的信息来」在这里意味着没有设备可扣、也没有价格可算。
		return nil, fmt.Errorf("%w: device reader is not configured", ErrDeviceLookupUnavailable)
	}

	device, found, err := s.devices.GetBySerial(ctx, in.DeviceSerial)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDeviceLookupUnavailable, err)
	}
	if !found {
		return nil, ErrDeviceNotFound
	}
	drink, found, err := s.devices.GetDeviceDrink(ctx, device.ID, in.DrinkCode)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDrinkLookupUnavailable, err)
	}
	if !found {
		return nil, ErrDrinkNotFound
	}

	price, err := pickupOrderPrice(drink.PickupCodePrice, drink.Price)
	if err != nil {
		return nil, err
	}
	// 复用刷卡机那条算金额的函数：这一单的「成交价」是我们定出来的 price，目录价还是目录价。
	// 两者之差记成标价优惠，于是库里那几条恒等式（payable = original − discount 等）照旧成立。
	original, discount, payable, err := deviceOrderAmounts(price, drink.Price)
	if err != nil {
		return nil, err
	}

	// —— 这个单号有没有被另一类设备单占着（**在动钱之前**判）——
	//
	// 两条设备接口（刷卡机 sync-order 与取货码 pickup）共用同一个单号空间：orders 上那条唯一
	// 索引只认 third_party_order_no，不知道它是从哪条路来的。合作方把一个单号用两次时，最早
	// 发现这件事的地方是本服务——而**必须在这里发现**：扣款在下面那条语句里，等建单那一步
	// 撞上冲突再拒，钱已经扣出去了，而那一笔从此挂在一张与它无关的订单上。
	//
	// 判据是那一张单的 payment_method 与这一条路的标签是否相同。查不到（绝大多数请求）照常
	// 往下走；查到同类说明这是正常重投，也照常往下走——重投随后由扣减与建单各自的幂等兜住。
	if existing, err := s.repository.FindDeviceOrderByThirdPartyNo(ctx, in.ThirdPartyOrderNo); err != nil {
		// 问不到订单库：这一单**还没动钱**，回错误让合作方稍后重投（与后面建单失败同一档）。
		return nil, mapWriteError(err)
	} else if existing != nil && existing.PaymentMethod != pickupOrderPaymentMethod {
		return nil, fmt.Errorf("%w: %s", ErrThirdPartyOrderNoTaken, in.ThirdPartyOrderNo)
	}

	// —— 先扣钱 ——
	//
	// applied=false 不是失败：这个对方单号已经扣过了，本次一个字段都没写。调用方要接着把订单
	// 建出来，所以这里不把它当错误。
	deduction, err := s.devices.DeductBalance(ctx, client.DeductBalanceInput{
		DeviceID:       device.ID,
		Amount:         payable,
		RequestID:      in.ThirdPartyOrderNo,
		PickupPassword: in.PickupPassword,
		Remark:         pickupOrderRemark(in.Remark),
	})
	if err != nil {
		return nil, err
	}

	// 重投时**按流水上的金额建单**，不按这一次算出来的价。
	//
	// 场景：第一次扣了 1400、建单失败 → 期间这一杯被改价成 2000 → 合作方重投。重投命中幂等，
	// 一分钱都不会再扣，但下面若照当前价算，这一单会落成 2000——钱只动过 1400，那 600 分的
	// 差额在库里没有任何一处能解释（订单、支付、流水三者对不上）。
	//
	// deduction.Amount 是**这一次实际扣掉的**：首次扣减就是上面那个 payable（相等，不进这个
	// 分支）；命中幂等时是当初那一次从流水上读回来的金额。0 表示对面没给这一格（咖啡机域还是
	// 加这格之前的版本），此时保持按当前价算——那是这一格出现之前的老行为，不是一次新的失败。
	if deduction.Amount > 0 && deduction.Amount != payable {
		// 目录价与优惠按**当前**目录价重算（那是展示用的标价与优惠，改价之后本来就该是新的），
		// 只有 payable 这一格必须等于钱真的动了多少。
		if original, discount, payable, err = deviceOrderAmounts(deduction.Amount, drink.Price); err != nil {
			return nil, err
		}
	}

	// —— 后建单 ——
	now := s.now().UTC()
	// 取货码那条路上出杯是机器自己做的，我们这里没有「出饮失败」这个输入（报文里也没有），
	// 所以履约汇总恒为 pending：这一单要出的那一杯由机器出，订单这一层等履约事件来改它。
	storeID := normalizedID(&device.StoreID)

	result, created, err := s.repository.CreateDeviceOrder(ctx, repository.CreateDeviceOrderParams{
		ThirdPartyOrderNo: in.ThirdPartyOrderNo,
		OrderNo:           generateOrderNo(now),
		Source:            model.SourceDevice,
		FulfillmentStatus: model.FulfillmentPending,
		StoreID:           storeID,
		DeviceID:          &device.ID,
		DeviceNo:          device.SerialUnique,
		OriginalAmount:    original,
		DiscountAmount:    discount,
		PayableAmount:     payable,
		PaymentMethod:     pickupOrderPaymentMethod,
		// paid_at 是收到回调的时刻。与刷卡机那条一样，这是本批的已知缺口：机器上真正扣钱的
		// 那一刻我们没有，两时刻之间只隔一次网络往返。
		PaidAt: now,
		Remark: pickupOrderRemark(in.Remark),
		// 状态流水那一行写的是「取货码购买」。复用刷卡机那条 SQL 时**这一格必须换掉**：
		// 记成「设备刷卡购买」会让后台的状态流水讲错这一单是怎么来的（钱不是刷的卡）。
		TransitionReason: pickupOrderReason,
		Line: &repository.OrderLineInsert{
			LineNo:   1,
			LineType: model.LineTypeDrink,
			ItemID:   &drink.ID,
			// item_code 存机器报的那个编号：事后拿机器流水来对时，对得上的是它。
			ItemCode: in.DrinkCode,
			ItemName: drink.Name,
			// 与刷卡机那条同一句：名称与图来自同一次目录读，两个字段一起落。
			ItemImage:           drink.Image,
			Quantity:            1,
			OriginalUnitPrice:   original,
			UnitPrice:           payable,
			PriceDiscountAmount: discount,
			DiscountAmount:      discount,
			PayableAmount:       payable,
			// 与刷卡机那条一样：取货码单没有规格、选品、加购活动与套餐快照。四列都是
			// NOT NULL DEFAULT '{}'，而 INSERT 每列都写值，所以四列都显式写空对象。
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
	return &CreatePickupOrderResult{OrderID: result.OrderID, OrderNo: result.OrderNo, Created: created}, nil
}

// pickupOrderPrice 定这一单的钱（分）：取货码价优先，为 0 回落目录价，两个都是 0 就拒。
//
// 与老系统同一条规则（那边的 pickup_code_price → price → 报「金额太小」），差别只在最后一步：
// 老系统把它当成一次可以重试的失败，这里是一个明确的结论——这一杯没法定价，而这我们改不了
// 机器报的报文也改不了，只能去给饮品定价。
//
// 负数（任何一个）按饮品库那一行坏了处理，与 deviceOrderAmounts 对目录价的做法一致：不回退、
// 不取绝对值，而是当成「问不到价格」——这条路上没有别的价格可退，而落一张负金额的单要糟得多。
func pickupOrderPrice(pickupCodePrice, catalogPrice int64) (int64, error) {
	if pickupCodePrice < 0 || catalogPrice < 0 {
		return 0, fmt.Errorf("%w: drink price is negative", ErrDrinkLookupUnavailable)
	}
	if pickupCodePrice > 0 {
		return pickupCodePrice, nil
	}
	if catalogPrice == 0 {
		return 0, ErrDrinkNotPickupPriced
	}
	return catalogPrice, nil
}

// pickupOrderRemark 拼订单备注：固定前缀 + 对方给的备注。
//
// 前缀写死，理由与刷卡机那条相同：「这一单从哪来」不该由调用方决定。
func pickupOrderRemark(remark string) string {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return pickupOrderReason
	}
	return pickupOrderReason + "：" + remark
}
