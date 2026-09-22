package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// 这条路上最容易出错的四件事，逐个钉住：
//
//   - **定价**：取货码价优先、为 0 回落目录价、两个都 0 就拒（不是「卖 0 元」）；
//   - **顺序**：钱先扣、单后建。反过来的话中间断了是白送一杯；
//   - **一个键两个身份**：流水上的 request_id 与订单上的 third_party_order_no 是同一个值，
//     拆开就是重投扣两次或建两张单；
//   - **扣款失败分档原样上抛**：码不对与余额不够必须保持是两条不同的结论。

const (
	testPickupPassword = "1357"
	testPickupRemark   = "机器前面那位要少冰"
)

// pickupOrderFixture 是一台已挂点位、在售一杯取货码价 1400 / 目录价 1800 的拿铁的机器。
func pickupOrderFixture() (*fakeDeviceReader, *fakeDeviceRepo) {
	devices, repo := deviceOrderFixture()
	devices.drink.PickupCodePrice = 1400
	return devices, repo
}

func fullPickupOrderInput() CreatePickupOrderInput {
	return CreatePickupOrderInput{
		ThirdPartyOrderNo: testThirdPartyNo,
		DeviceSerial:      testDeviceSerial,
		DrinkCode:         testDrinkCode,
		PickupPassword:    testPickupPassword,
		Remark:            testPickupRemark,
	}
}

// TestPickupOrderPrice 是这条路唯一的定价规则，逐种组合都钉住。
//
// 与刷卡机那条的差别值得单独说一句：那边是**设备报价优先**（钱是机器收的，我们记既成事实），
// 这边是**我们定价**（钱是我们从自己账上扣的）。所以报文里根本没有金额这一格，而这里的
// 每一个结论都会真的从设备余额里扣走那么多钱。
func TestPickupOrderPrice(t *testing.T) {
	cases := []struct {
		name            string
		pickup, catalog int64
		want            int64
		wantErr         error
	}{
		{name: "取货码价优先", pickup: 1400, catalog: 1800, want: 1400},
		{name: "取货码价为 0 时回落目录价", pickup: 0, catalog: 1800, want: 1800},
		{name: "两个价相等", pickup: 1800, catalog: 1800, want: 1800},
		{name: "取货码价比目录价高也认（我们认的就是这个价）", pickup: 2000, catalog: 1800, want: 2000},
		{name: "两个都为 0：没法定价，拒", pickup: 0, catalog: 0, wantErr: ErrDrinkNotPickupPriced},
		{name: "取货码价是负数：饮品库那一行坏了", pickup: -1, catalog: 1800, wantErr: ErrDrinkLookupUnavailable},
		{name: "目录价是负数：回退的那一格也坏了", pickup: 0, catalog: -1, wantErr: ErrDrinkLookupUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickupOrderPrice(tc.pickup, tc.catalog)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if got != 0 {
					t.Fatalf("price = %d, want 0 when it refuses", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("price = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCreatePickupOrderDeductsThenRecords 是这条路的正例，一次走完两件事：从设备余额里扣钱、
// 落一张 pickup_code 的已支付订单。
func TestCreatePickupOrderDeductsThenRecords(t *testing.T) {
	devices, repo := pickupOrderFixture()
	result, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Created {
		t.Error("created = false, want true on the first call")
	}

	// —— 扣钱那一侧 ——
	if devices.deductCalls != 1 {
		t.Fatalf("deduct calls = %d, want exactly 1", devices.deductCalls)
	}
	deduct := devices.gotDeduct
	if deduct.DeviceID != devices.device.ID {
		t.Errorf("deduct device = %q, want the uuid from the serial lookup (%q)", deduct.DeviceID, devices.device.ID)
	}
	// 扣的是**取货码价**，不是目录价：差价记成标价优惠，钱不能多扣。
	if deduct.Amount != 1400 {
		t.Errorf("deducted %d, want the pickup code price 1400", deduct.Amount)
	}
	// 一个键两个身份：这一格必须与订单上的对方单号逐字相同，否则重投会分别撞两个索引。
	if deduct.RequestID != testThirdPartyNo {
		t.Errorf("request id = %q, want the third party order no %q", deduct.RequestID, testThirdPartyNo)
	}
	// 验证码原样转下去：不 trim、不改写（比对在持有那一列的服务里）。
	if deduct.PickupPassword != testPickupPassword {
		t.Errorf("pickup password = %q, want it passed through unchanged", deduct.PickupPassword)
	}
	if deduct.Remark != pickupOrderReason+"："+testPickupRemark {
		t.Errorf("deduct remark = %q, want the order remark", deduct.Remark)
	}

	// —— 建单那一侧 ——
	if repo.calls != 1 {
		t.Fatalf("repository calls = %d, want 1", repo.calls)
	}
	got := repo.got
	if got.PaymentMethod != pickupOrderPaymentMethod {
		t.Errorf("paymentMethod = %q, want %q", got.PaymentMethod, pickupOrderPaymentMethod)
	}
	if got.ThirdPartyOrderNo != testThirdPartyNo {
		t.Errorf("thirdPartyOrderNo = %q, want %q", got.ThirdPartyOrderNo, testThirdPartyNo)
	}
	// 状态流水那一行说的是「取货码购买」：复用刷卡机那条 SQL 时这一格最容易漏。
	if got.TransitionReason != pickupOrderReason {
		t.Errorf("transition reason = %q, want %q", got.TransitionReason, pickupOrderReason)
	}
	if got.Remark != pickupOrderReason+"："+testPickupRemark {
		t.Errorf("remark = %q, want the pickup prefix plus what the vendor sent", got.Remark)
	}
	if got.Source != model.SourceDevice {
		t.Errorf("source = %q, want %q (来源仍是设备回调，区别在 payment_method 上)", got.Source, model.SourceDevice)
	}
	// 这一单不属于任何用户：钱从设备余额里扣，机器前面那个人在我们这儿没有账号。
	if got.FulfillmentStatus != model.FulfillmentPending {
		t.Errorf("fulfillmentStatus = %q, want %q", got.FulfillmentStatus, model.FulfillmentPending)
	}
	if !got.PaidAt.Equal(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("paidAt = %v, want the time the callback arrived", got.PaidAt)
	}
	// 金额：取货码价 1400、目录价 1800 → 标价优惠 400，应付就是扣走的那 1400。
	if got.OriginalAmount != 1800 || got.DiscountAmount != 400 || got.PayableAmount != 1400 {
		t.Errorf("amounts = (%d, %d, %d), want (1800, 400, 1400)", got.OriginalAmount, got.DiscountAmount, got.PayableAmount)
	}
	line := got.Line
	if line == nil {
		t.Fatal("order line is missing")
	}
	if line.ItemID == nil || *line.ItemID != devices.drink.ID || line.ItemCode != testDrinkCode {
		t.Errorf("line item = (%v, %q), want (%q, %q)", line.ItemID, line.ItemCode, devices.drink.ID, testDrinkCode)
	}
	// 名称与图取自那一次目录读（与刷卡机那条同一句）：这一步已经知道这一杯是什么，
	// 行上却只有名字没有图，后台那一行就成了半个商品。
	if line.ItemName != devices.drink.Name || line.ItemImage != devices.drink.Image {
		t.Errorf("line snapshot = (%q, %q), want (%q, %q)",
			line.ItemName, line.ItemImage, devices.drink.Name, devices.drink.Image)
	}
	if line.PayableAmount != line.OriginalUnitPrice*int64(line.Quantity)-line.DiscountAmount {
		t.Errorf("line payable %d does not match original %d - discount %d",
			line.PayableAmount, line.OriginalUnitPrice, line.DiscountAmount)
	}
	if line.DiscountAmount != line.PriceDiscountAmount+line.CouponDiscountAmount {
		t.Errorf("line discount %d != price %d + coupon %d", line.DiscountAmount, line.PriceDiscountAmount, line.CouponDiscountAmount)
	}
	// 行上的单价必须与真的扣走的那笔钱一致：库里那张单与流水上那一笔对不上，是这条路
	// 最难查的一种错（两边看起来都正常）。
	if line.UnitPrice != deduct.Amount || got.PayableAmount != deduct.Amount {
		t.Errorf("the order says %d but the device was charged %d", got.PayableAmount, deduct.Amount)
	}
}

// TestCreatePickupOrderFallsBackToTheCatalogPrice 是回落那一条的端到端：这一杯没配取货码价，
// 扣的是目录价，订单上也没有优惠。
func TestCreatePickupOrderFallsBackToTheCatalogPrice(t *testing.T) {
	devices, repo := pickupOrderFixture()
	devices.drink.PickupCodePrice = 0
	if _, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if devices.gotDeduct.Amount != 1800 {
		t.Errorf("deducted %d, want the catalog price 1800", devices.gotDeduct.Amount)
	}
	if repo.got.OriginalAmount != 1800 || repo.got.DiscountAmount != 0 || repo.got.PayableAmount != 1800 {
		t.Errorf("amounts = (%d, %d, %d), want (1800, 0, 1800)", repo.got.OriginalAmount, repo.got.DiscountAmount, repo.got.PayableAmount)
	}
}

// TestCreatePickupOrderRefusesAnUnpricedDrink 是「两个价都是 0」那一条：**一分钱都不许扣**，
// 也不许建单。
//
// 出事的样子很具体：把 0 当成成交价记下来，于是一杯免费的咖啡从设备余额里扣走 0 元、
// 订单落成 payable=0，而账面上看不出任何异常。
func TestCreatePickupOrderRefusesAnUnpricedDrink(t *testing.T) {
	devices, repo := pickupOrderFixture()
	devices.drink.PickupCodePrice = 0
	devices.drink.Price = 0
	_, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if !errors.Is(err, ErrDrinkNotPickupPriced) {
		t.Fatalf("err = %v, want %v", err, ErrDrinkNotPickupPriced)
	}
	if devices.deductCalls != 0 {
		t.Error("an unpriced drink still reached the device balance")
	}
	if repo.calls != 0 {
		t.Error("an unpriced drink still wrote an order")
	}
}

// TestCreatePickupOrderKeepsTheDeductConclusionsApart 是扣款那五种结论那一条：每一条都必须
// **原样**上抛，不能被并成一句。
//
// 尤其不能把「码不对」与「余额不够」合并：它们要在机器屏幕上变成两句不同的话；也不能把
// 「没问到」当成前面任何一条——那会让合作方去改一份没错的报文，或者以为这台机器没钱了。
func TestCreatePickupOrderKeepsTheDeductConclusionsApart(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "取货码不对", err: client.ErrDeviceBalancePasswordRejected},
		{name: "余额不够", err: client.ErrDeviceBalanceNotEnough},
		{name: "扣减被拒", err: client.ErrDeviceBalanceRejected},
		{name: "设备在两次调用之间没了", err: client.ErrDeviceBalanceDeviceMissing},
		{name: "没问到咖啡机域", err: client.ErrDeviceBalanceServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devices, repo := pickupOrderFixture()
			devices.deductErr = tc.err
			_, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want it to carry %v", err, tc.err)
			}
			// 钱没扣成就不许建单：建了单没扣钱是白送一杯，而这一单在库里看着完全正常。
			if repo.calls != 0 {
				t.Error("a failed deduction still wrote an order")
			}
		})
	}
}

// TestCreatePickupOrderReplaysSafely 是重投那一条：扣减命中幂等（applied=false）与建单命中
// 幂等（created=false）同时发生，两次调用一个字段都不改，也不报错。
func TestCreatePickupOrderReplaysSafely(t *testing.T) {
	devices, repo := pickupOrderFixture()
	devices.replayed = true
	repo.created = false

	result, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if err != nil {
		t.Fatalf("a replay must not be an error: %v", err)
	}
	if result.Created {
		t.Error("created = true, want false on an idempotent replay")
	}
	if result.OrderID == "" || result.OrderNo == "" {
		t.Error("a replay must return the existing order")
	}
	// 重投仍然会走一次扣减调用，但带的是**同一个** request_id——幂等由咖啡机域那一侧保证
	// （流水表上那个 request_id 非空的唯一索引），本服务不自己判重。
	if devices.gotDeduct.RequestID != testThirdPartyNo {
		t.Errorf("request id = %q, want the same key on a replay", devices.gotDeduct.RequestID)
	}
}

// TestCreatePickupOrderBooksTheAmountThatWasActuallyCharged 钉住重投时的金额：**钱只动过一次，
// 订单必须记那一笔**。
//
// 出事的样子：第一次扣了 1400、建单失败 → 期间这一杯被改价成 2000 → 合作方重投。重投命中幂等，
// 一分钱都不会再扣，但若照当前价算，订单会落成 2000——库里那 600 分的差额没有任何一处能解释
// （设备余额流水上是 1400，订单上是 2000）。所以金额取自扣减那一次的回答，而不是这一次算的价。
//
// 两个前提在这一条里都是真的：drink 的取货码价已经是 2000（改过价了），而咖啡机域回的
// replayAmount 是 1400（当初扣的那一笔）。
func TestCreatePickupOrderBooksTheAmountThatWasActuallyCharged(t *testing.T) {
	devices, repo := pickupOrderFixture()
	devices.replayed = true
	devices.replayAmount = 1400
	// 改价：现在这一杯的取货码价是 2000，而当初扣的是 1400。
	devices.drink.PickupCodePrice = 2000
	repo.created = false

	result, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Created {
		t.Error("created = true, want false on a replay")
	}
	// 这一次请求带下去的价确实是当前价（扣减那边命中幂等会忽略它）——这不是 bug，是重投的常态。
	if devices.gotDeduct.Amount != 2000 {
		t.Errorf("deduct amount = %d, want the current price 2000 on a replay", devices.gotDeduct.Amount)
	}
	// 但订单记的是**流水上那一笔**。目录价与优惠仍按当前目录价重算（那是展示用的标价）。
	if repo.got.PayableAmount != 1400 {
		t.Errorf("payable = %d, want the 1400 that was actually charged", repo.got.PayableAmount)
	}
	if repo.got.OriginalAmount != 1800 || repo.got.DiscountAmount != 400 {
		t.Errorf("amounts = (%d, %d), want (1800, 400) — 标价与优惠仍按当前目录价",
			repo.got.OriginalAmount, repo.got.DiscountAmount)
	}
	line := repo.got.Line
	if line == nil {
		t.Fatal("order line is missing")
	}
	if line.UnitPrice != 1400 || line.PayableAmount != 1400 {
		t.Errorf("line unit price = %d, payable = %d, want both 1400", line.UnitPrice, line.PayableAmount)
	}
}

// TestCreatePickupOrderFallsBackWhenTheAmountIsMissing 是上面那一条的另一半：咖啡机域还是加
// 「金额」这一格之前的版本时，那一格读到的是 0——按 0 处理成「拿不到当初的金额」，退回按当前价
// 算，而不是把这杯记成 0 元。
//
// 也就是说这个修复不会因为一次**没同步升级的部署**把订单金额改成 0：漏掉它只是回到修复之前的
// 行为（金额可能与流水不等），而不是造出一张 0 元的单。
func TestCreatePickupOrderFallsBackWhenTheAmountIsMissing(t *testing.T) {
	devices, repo := pickupOrderFixture()
	devices.replayed = true
	devices.replayAmount = 0
	devices.drink.PickupCodePrice = 2000
	repo.created = false

	if _, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.got.PayableAmount != 2000 {
		t.Errorf("payable = %d, want the current price 2000 when the charged amount is unknown", repo.got.PayableAmount)
	}
}

// TestCreatePickupOrderRefusesAThirdPartyOrderNoOfAnotherKind 钉住两条设备接口共用的那个单号
// 空间：这个单号已经属于一张**刷卡机**单时，取货码这条路必须拒绝，而且要在**动钱之前**拒绝。
//
// 出事的样子：扣款在下面的语句里发生，等建单那一步撞上唯一索引才发现单号早被占了——那时钱已经
// 从设备余额里扣走，而返回给合作方的是另一张单的单号。那笔扣款从此挂在一张与它无关的订单上，
// 翻遍库也查不出它是怎么来的。
func TestCreatePickupOrderRefusesAThirdPartyOrderNoOfAnotherKind(t *testing.T) {
	devices, repo := pickupOrderFixture()
	repo.existing = &repository.DeviceOrderByThirdPartyNo{
		OrderID:       "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		OrderNo:       "DEV202609160001",
		PaymentMethod: deviceOrderPaymentMethod,
	}
	_, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if !errors.Is(err, ErrThirdPartyOrderNoTaken) {
		t.Fatalf("err = %v, want %v", err, ErrThirdPartyOrderNoTaken)
	}
	// 这一条的全部意义：一分钱都没动，一张单都没写。
	if devices.deductCalls != 0 {
		t.Error("a taken order no still reached the device balance")
	}
	if repo.calls != 0 {
		t.Error("a taken order no still wrote an order")
	}
}

// TestCreatePickupOrderAcceptsItsOwnKindOnAReplay 是上一条的另一半：查到的既有单**是同类**时
// 照常往下走（那是正常重投，随后由扣减与建单各自的幂等兜住），不能一律拒掉。
func TestCreatePickupOrderAcceptsItsOwnKindOnAReplay(t *testing.T) {
	devices, repo := pickupOrderFixture()
	repo.existing = &repository.DeviceOrderByThirdPartyNo{
		OrderID:       "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		OrderNo:       "DEV202609160001",
		PaymentMethod: pickupOrderPaymentMethod,
	}
	devices.replayed = true
	devices.replayAmount = 1400
	repo.created = false

	result, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if err != nil {
		t.Fatalf("a replay of the same kind must not be an error: %v", err)
	}
	if result.Created {
		t.Error("created = true, want false on a replay")
	}
	if result.OrderNo != "DEV202609160001" {
		t.Errorf("orderNo = %q, want the existing one", result.OrderNo)
	}
}

// TestCreatePickupOrderStopsWhenTheOrderLookupFails：查「这个单号有没有被占」那一次失败时，
// 这一单**还没动钱**，所以停在原地（回错误让合作方稍后重投），而不是继续往下扣。
//
// 反过来（跳过这次检查、先扣了再说）的代价是一次可能白扣的钱——而这一次查询失败本来就说明
// 订单库这会儿答不上来，接着往下走是在赌它下一句能答对。
func TestCreatePickupOrderStopsWhenTheOrderLookupFails(t *testing.T) {
	devices, repo := pickupOrderFixture()
	repo.existingErr = errors.New("orders db is down")
	_, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if err == nil {
		t.Fatal("a failed lookup must be an error")
	}
	if devices.deductCalls != 0 || repo.calls != 0 {
		t.Error("a failed lookup still went on to charge and write")
	}
}

// TestCreatePickupOrderRejectsBadShapeWithoutTouchingAnything：三个必填（对方单号、序列号、
// 饮品编号）缺一个就在本地停住，既不问下游也不扣钱。
//
// **验证码不在这三个里面**：它是空的还是错的由咖啡机域判（那边两种回同一个结论），在这里
// 多判一次只会多一个说得不一样的地方。
func TestCreatePickupOrderRejectsBadShapeWithoutTouchingAnything(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreatePickupOrderInput)
		want   error
	}{
		{name: "没给幂等键", mutate: func(in *CreatePickupOrderInput) { in.ThirdPartyOrderNo = "   " }, want: ErrThirdPartyOrderNoRequired},
		{name: "没给序列号", mutate: func(in *CreatePickupOrderInput) { in.DeviceSerial = "" }, want: ErrDeviceSerialRequired},
		{name: "没给饮品编号", mutate: func(in *CreatePickupOrderInput) { in.DrinkCode = "" }, want: ErrDrinkCodeRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devices, repo := pickupOrderFixture()
			in := fullPickupOrderInput()
			tc.mutate(&in)
			_, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !IsValidationError(err) {
				t.Errorf("%v is not in ValidationErrors; partner-service would see it as our fault", tc.want)
			}
			if devices.deviceCalls != 0 || devices.drinkCalls != 0 || devices.deductCalls != 0 || repo.calls != 0 {
				t.Error("a bad shape reached the network or the database")
			}
		})
	}
}

// TestCreatePickupOrderPassesThePickupCodeThroughUntouched：验证码一个字符都不动——不 trim、
// 不判空。
//
// 这一条单独钉住，是因为「顺手 trim 一下」看起来永远是对的：而它把一个「码里带空格」的配置
// 悄悄改写成另一个意思，表现是「后台看着配了，现场就是打不开」。
func TestCreatePickupOrderPassesThePickupCodeThroughUntouched(t *testing.T) {
	devices, repo := pickupOrderFixture()
	in := fullPickupOrderInput()
	in.PickupPassword = " 1357 "
	if _, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if devices.gotDeduct.PickupPassword != " 1357 " {
		t.Errorf("pickup password = %q, want it passed through untrimmed", devices.gotDeduct.PickupPassword)
	}
}

// TestPickupOrderRemark 盯备注前缀：固定前缀 + 对方的备注；对方没给就只剩前缀。
func TestPickupOrderRemark(t *testing.T) {
	cases := []struct{ name, remark, want string }{
		{name: "带备注", remark: "少冰", want: pickupOrderReason + "：少冰"},
		{name: "只有空格", remark: "  ", want: pickupOrderReason},
		{name: "没给", remark: "", want: pickupOrderReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickupOrderRemark(tc.remark); got != tc.want {
				t.Fatalf("remark = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCreatePickupOrderKeepsTheTwoMissesApart 与刷卡机那条同一条规矩：设备没有是 404（去登记
// 设备），饮品没有是 400（换一个编号），两条都不能变成 5xx，而且都不能走到扣钱那一步。
func TestCreatePickupOrderKeepsTheTwoMissesApart(t *testing.T) {
	t.Run("设备没登记", func(t *testing.T) {
		devices, repo := pickupOrderFixture()
		devices.deviceFound = false
		_, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
		if !errors.Is(err, ErrDeviceNotFound) {
			t.Fatalf("err = %v, want %v", err, ErrDeviceNotFound)
		}
		if devices.drinkCalls != 0 || devices.deductCalls != 0 || repo.calls != 0 {
			t.Error("a missing device still went on")
		}
	})
	t.Run("这台机器上没有这个编号的饮品", func(t *testing.T) {
		devices, repo := pickupOrderFixture()
		devices.drinkFound = false
		_, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
		if !errors.Is(err, ErrDrinkNotFound) {
			t.Fatalf("err = %v, want %v", err, ErrDrinkNotFound)
		}
		if devices.deductCalls != 0 || repo.calls != 0 {
			t.Error("an unknown drink still reached the device balance")
		}
	})
}

// TestCreatePickupOrderTrimsWhatItPassesOn：三个字符串参数 trim 之后再用——机器报的编号
// 前后多一个空格只会在现场变成一次「这杯不存在」，而对方单号里多一个空格会让幂等键变掉。
func TestCreatePickupOrderTrimsWhatItPassesOn(t *testing.T) {
	devices, repo := pickupOrderFixture()
	in := CreatePickupOrderInput{
		ThirdPartyOrderNo: "  " + testThirdPartyNo + "  ",
		DeviceSerial:      "  " + testDeviceSerial + "  ",
		DrinkCode:         "  " + testDrinkCode + "  ",
	}
	if _, err := newDeviceOrderService(devices, repo).CreatePickupOrder(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if devices.gotSerial != testDeviceSerial || devices.gotDrinkCode != testDrinkCode {
		t.Errorf("lookups = (%q, %q), want the trimmed values", devices.gotSerial, devices.gotDrinkCode)
	}
	if repo.got.ThirdPartyOrderNo != testThirdPartyNo || devices.gotDeduct.RequestID != testThirdPartyNo {
		t.Errorf("keys = (%q, %q), want both to be the trimmed %q",
			repo.got.ThirdPartyOrderNo, devices.gotDeduct.RequestID, testThirdPartyNo)
	}
}

// TestCreatePickupOrderStopsWhenThereIsNoDeviceReader：没有可用的读端时明确失败，不退化成
// 「那就按请求里的信息来」——这条路上退化的意思是没设备可扣、也没价格可算。
func TestCreatePickupOrderStopsWhenThereIsNoDeviceReader(t *testing.T) {
	repo := &fakeDeviceRepo{created: true}
	_, err := newDeviceOrderService(nil, repo).CreatePickupOrder(context.Background(), fullPickupOrderInput())
	if !errors.Is(err, ErrDeviceLookupUnavailable) {
		t.Fatalf("err = %v, want %v", err, ErrDeviceLookupUnavailable)
	}
	if repo.calls != 0 {
		t.Error("a missing reader still wrote an order")
	}
}

// TestPickupOrderReasonDiffersFromTheCardOrder 是一句提醒用的话：两条路的备注前缀与状态流水
// 的原因**不能**共用一句，否则后台看流水时取货码的单会像是刷了卡。
func TestPickupOrderReasonDiffersFromTheCardOrder(t *testing.T) {
	if pickupOrderReason == deviceOrderReason {
		t.Fatalf("both paths write %q into the state log", pickupOrderReason)
	}
	if !strings.Contains(pickupOrderReason, "取货码") {
		t.Errorf("pickupOrderReason = %q, want it to say 取货码", pickupOrderReason)
	}
}
