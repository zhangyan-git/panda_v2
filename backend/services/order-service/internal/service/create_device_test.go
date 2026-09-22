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

// 这一层测的是设备单那条路上**不碰网络、不碰库**的那部分：金额怎么算、门店与设备从哪来、
// 两个 NotFound 分不分得开、重投是不是真的不报错。出事的样子是「机器报了 5 块、订单记成
// 3 块」或者「钱收了、单没落」，而这两种在库里都看不出来。

const (
	testDeviceSerial = "SN-0001"
	testDrinkCode    = "P001"
	testDrinkImage   = "https://cdn.example.invalid/latte.png"
	testThirdPartyNo = "PARTNER-20260916-0001"
)

// fakeDeviceReader 是一台机器与它上面的一杯饮品，以及两次读各自的结论。
//
// 它也是扣余额那一路的替身（取货码那条路）：那一格记下扣减的参数与它该回什么结论。
type fakeDeviceReader struct {
	device      *client.Device
	deviceFound bool
	deviceErr   error
	drink       *client.Drink
	drinkFound  bool
	drinkErr    error

	// deductErr 非 nil 时扣减失败；replayed 为 true 时那一笔已经扣过了（applied=false）。
	deductErr error
	replayed  bool
	// replayAmount 是重投时咖啡机域回出来的**当初扣掉的那一笔**（client.DeductBalanceResult.Amount）。
	// 为 0 表示对面没给这一格（版本错配），此时本服务按当前价算——老行为。
	replayAmount int64
	gotDeduct    client.DeductBalanceInput
	deductCalls  int

	// catalog 是「按**我们的 uuid** 取一杯」那一次（小程序下单定价那条路）的答案。它与上面
	// 那一杯分开：两条路收的钥匙不同（那里是机器报的编号，这里是主键），用例要能分别给它们
	// 不同的结论——把一份答案同时喂给两条路，等于没法测「A 店的饮品配 B 店的设备」。
	catalog      *client.Drink
	catalogFound bool
	catalogErr   error

	gotSerial    string
	gotDeviceID  string
	gotDrinkID   string
	gotDrinkCode string
	gotCatalogID string
	deviceCalls  int
	drinkCalls   int
	catalogCalls int
}

// DeductBalance 默认回「这一次真的扣了」（applied=true）。用例要造重投时把 replayed 置为 true。
//
// 金额那一格照真实语义填：首次扣减回本次的金额（咖啡机域回的就是它），重投回 replayAmount
// （为 0 时表示对面没给这一格）。
func (f *fakeDeviceReader) DeductBalance(_ context.Context, in client.DeductBalanceInput) (*client.DeductBalanceResult, error) {
	f.deductCalls++
	f.gotDeduct = in
	if f.deductErr != nil {
		return nil, f.deductErr
	}
	amount := in.Amount
	if f.replayed {
		amount = f.replayAmount
	}
	return &client.DeductBalanceResult{BalanceAfter: 5000, Applied: !f.replayed, Amount: amount}, nil
}

// Get 是按**我们的 uuid** 读设备（小程序下单那条路）。它与 GetBySerial 用同一组答案：
// 用例里那一台机器只有一个，两条路收的钥匙不同（这里是主键，那里是机器序列号）。
func (f *fakeDeviceReader) Get(_ context.Context, deviceID string) (*client.Device, bool, error) {
	f.deviceCalls++
	f.gotDeviceID = deviceID
	return f.device, f.deviceFound, f.deviceErr
}

func (f *fakeDeviceReader) GetBySerial(_ context.Context, serial string) (*client.Device, bool, error) {
	f.deviceCalls++
	f.gotSerial = serial
	return f.device, f.deviceFound, f.deviceErr
}

func (f *fakeDeviceReader) GetDeviceDrink(_ context.Context, deviceID, drinkCode string) (*client.Drink, bool, error) {
	f.drinkCalls++
	f.gotDrinkID, f.gotDrinkCode = deviceID, drinkCode
	return f.drink, f.drinkFound, f.drinkErr
}

func (f *fakeDeviceReader) GetDrink(_ context.Context, drinkID string) (*client.Drink, bool, error) {
	f.catalogCalls++
	f.gotCatalogID = drinkID
	return f.catalog, f.catalogFound, f.catalogErr
}

// fakeDeviceRepo 收下这一单真正要写进去的东西，什么都不落库。
type fakeDeviceRepo struct {
	Repository

	calls   int
	got     repository.CreateDeviceOrderParams
	created bool

	// existing / existingErr 是「按对方单号查既有单」那一次的答案。默认 (nil, nil)：
	// 绝大多数用例都是这个单号第一次来。要造「单号被另一类设备单占了」的用例自己塞 existing。
	existing    *repository.DeviceOrderByThirdPartyNo
	existingErr error
	findCalls   int
}

func (f *fakeDeviceRepo) FindDeviceOrderByThirdPartyNo(_ context.Context, _ string) (*repository.DeviceOrderByThirdPartyNo, error) {
	f.findCalls++
	return f.existing, f.existingErr
}

// CreateDeviceOrder 在 created=false 时回的是**既有那一张**（单号也回它的），与真实仓储撞上
// 唯一索引后的行为一致：调用方拿到的永远是「这个对方单号对应的那一张单」，不是这一轮新编的号。
func (f *fakeDeviceRepo) CreateDeviceOrder(_ context.Context, p repository.CreateDeviceOrderParams) (*repository.CreateDeviceOrderResult, bool, error) {
	f.calls++
	f.got = p
	if !f.created && f.existing != nil {
		return &repository.CreateDeviceOrderResult{
			OrderID: f.existing.OrderID,
			OrderNo: f.existing.OrderNo,
			Created: false,
		}, false, nil
	}
	return &repository.CreateDeviceOrderResult{OrderID: "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e", OrderNo: p.OrderNo, Created: f.created}, f.created, nil
}

// deviceOrderFixture 是一台已挂点位、在售一杯 18 元拿铁的机器。
func deviceOrderFixture() (*fakeDeviceReader, *fakeDeviceRepo) {
	return &fakeDeviceReader{
			device:      &client.Device{ID: "3f0a6a1e-1111-4222-8333-444455556666", StoreID: "9a8b7c6d-1111-4222-8333-444455556666", SerialUnique: testDeviceSerial, Status: "active"},
			deviceFound: true,
			// 图要有值：设备单与取货码单都要把它落进订单行，夹具留空的话那两条断言就是
			// 在比两个空串，永远绿。
			drink:      &client.Drink{ID: "5c4b3a29-1111-4222-8333-444455556666", OriginID: "origin-1", Name: "拿铁", Image: testDrinkImage, Price: 1800},
			drinkFound: true,
		},
		&fakeDeviceRepo{created: true}
}

func newDeviceOrderService(devices DeviceReader, repo Repository) *OrderService {
	return New(repo, devices, nil, nil, nil, nil, Options{Now: func() time.Time {
		return time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	}})
}

func fullDeviceOrderInput() CreateDeviceOrderInput {
	return CreateDeviceOrderInput{
		ThirdPartyOrderNo: testThirdPartyNo,
		DeviceSerial:      testDeviceSerial,
		DrinkCode:         testDrinkCode,
		Amount:            1500,
	}
}

// TestDeviceOrderAmounts 是这条路唯一一处算钱的地方，所以逐种成交情形都钉住。
//
// 三件事一起判：三个数各自等于什么、payable = original − discount 与
// discount = price + coupon（coupon 恒 0）这两条恒等式成立、三个数都非负——
// 库里那几条 CHECK 就是按这三件事写的。
func TestDeviceOrderAmounts(t *testing.T) {
	cases := []struct {
		name                        string
		deviceAmount, catalogPrice  int64
		original, discount, payable int64
	}{
		{name: "device price below the catalog price", deviceAmount: 1500, catalogPrice: 1800, original: 1800, discount: 300, payable: 1500},
		{name: "device price equals the catalog price", deviceAmount: 1800, catalogPrice: 1800, original: 1800, discount: 0, payable: 1800},
		{
			// 设备报得比目录价还贵：订单上留的是设备那个价，优惠为 0。目录价在这一单里
			// 看不出来——这是已知失真，见 deviceOrderAmounts 的说明。
			name: "device price above the catalog price", deviceAmount: 2000, catalogPrice: 1800,
			original: 2000, discount: 0, payable: 2000,
		},
		{name: "device reported zero so the catalog price is used", deviceAmount: 0, catalogPrice: 1800, original: 1800, discount: 0, payable: 1800},
		{name: "device reported a negative amount", deviceAmount: -100, catalogPrice: 1800, original: 1800, discount: 0, payable: 1800},
		{name: "both are zero", deviceAmount: 0, catalogPrice: 0, original: 0, discount: 0, payable: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original, discount, payable, err := deviceOrderAmounts(tc.deviceAmount, tc.catalogPrice)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if original != tc.original || discount != tc.discount || payable != tc.payable {
				t.Fatalf("amounts = (%d, %d, %d), want (%d, %d, %d)",
					original, discount, payable, tc.original, tc.discount, tc.payable)
			}
			if payable != original-discount {
				t.Errorf("payable %d != original %d - discount %d", payable, original, discount)
			}
			if original < 0 || discount < 0 || payable < 0 {
				t.Errorf("negative amount written down: (%d, %d, %d)", original, discount, payable)
			}
		})
	}
}

// TestDeviceOrderAmountsRejectsABrokenCatalogRecord：目录价是负数时不出单，也不要造一张
// 负金额的单——那是饮品库那一行坏了，不是「这一杯卖负钱」。
func TestDeviceOrderAmountsRejectsABrokenCatalogRecord(t *testing.T) {
	if _, _, _, err := deviceOrderAmounts(0, -1); !errors.Is(err, ErrDrinkLookupUnavailable) {
		t.Fatalf("err = %v, want %v", err, ErrDrinkLookupUnavailable)
	}
}

// TestCreateDeviceOrderRecordsWhatTheMachineSaid 是这条路的正例：一台机器报上来的一杯，
// 落成一张已支付、没有用户、点位来自设备的订单，机器报的编号进 item_code。
func TestCreateDeviceOrderRecordsWhatTheMachineSaid(t *testing.T) {
	devices, repo := deviceOrderFixture()
	_, err := newDeviceOrderService(devices, repo).CreateDeviceOrder(context.Background(), fullDeviceOrderInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("repository calls = %d, want 1", repo.calls)
	}
	if devices.gotSerial != testDeviceSerial {
		t.Errorf("device lookup = %q, want the serial %q", devices.gotSerial, testDeviceSerial)
	}
	// 饮品是按**设备 uuid**（不是序列号）加机器报的编号查的：序列号只是进入这条路的那把钥匙。
	if devices.gotDrinkID != devices.device.ID || devices.gotDrinkCode != testDrinkCode {
		t.Errorf("drink lookup = (%q, %q), want (%q, %q)", devices.gotDrinkID, devices.gotDrinkCode, devices.device.ID, testDrinkCode)
	}

	got := repo.got
	if got.Source != model.SourceDevice {
		t.Errorf("source = %q, want %q", got.Source, model.SourceDevice)
	}
	if got.StoreID == nil || *got.StoreID != devices.device.StoreID {
		t.Errorf("storeId = %v, want the device's store %q", got.StoreID, devices.device.StoreID)
	}
	if got.DeviceID == nil || *got.DeviceID != devices.device.ID || got.DeviceNo != testDeviceSerial {
		t.Errorf("device = (%v, %q), want (%q, %q)", got.DeviceID, got.DeviceNo, devices.device.ID, testDeviceSerial)
	}
	if got.ThirdPartyOrderNo != testThirdPartyNo {
		t.Errorf("thirdPartyOrderNo = %q, want %q", got.ThirdPartyOrderNo, testThirdPartyNo)
	}
	if got.PaymentMethod != deviceOrderPaymentMethod {
		t.Errorf("paymentMethod = %q, want %q", got.PaymentMethod, deviceOrderPaymentMethod)
	}
	if !strings.HasPrefix(got.OrderNo, "3CYM") {
		t.Errorf("orderNo = %q, want the 3CYM prefix the acquirer requires", got.OrderNo)
	}
	if got.FulfillmentStatus != model.FulfillmentPending {
		t.Errorf("fulfillmentStatus = %q, want %q", got.FulfillmentStatus, model.FulfillmentPending)
	}
	if !got.PaidAt.Equal(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("paidAt = %v, want the time the callback arrived", got.PaidAt)
	}
	// 金额：设备报 1500、目录价 1800 → 标价优惠 300，应付就是设备报的那个数。
	if got.OriginalAmount != 1800 || got.DiscountAmount != 300 || got.PayableAmount != 1500 {
		t.Errorf("amounts = (%d, %d, %d), want (1800, 300, 1500)", got.OriginalAmount, got.DiscountAmount, got.PayableAmount)
	}

	line := got.Line
	if line == nil {
		t.Fatal("order line is missing")
	}
	if line.LineType != model.LineTypeDrink || line.LineNo != 1 || line.Quantity != 1 {
		t.Errorf("line = (%q, %d, %d), want (drink, 1, 1)", line.LineType, line.LineNo, line.Quantity)
	}
	if line.ItemID == nil || *line.ItemID != devices.drink.ID || line.ItemName != devices.drink.Name {
		t.Errorf("line item = (%v, %q), want (%q, %q)", line.ItemID, line.ItemName, devices.drink.ID, devices.drink.Name)
	}
	// item_code 存的是**机器报的那个编号**：事后拿机器流水来对时，对得上的是它。
	if line.ItemCode != testDrinkCode {
		t.Errorf("itemCode = %q, want the code the machine reported (%q)", line.ItemCode, testDrinkCode)
	}
	// 名称与图是同一次目录读带回来的，两个都要落：后台列表上这一杯得有名字也有图，
	// 少落一个不会报错，只会让那一行看起来像另一个商品。
	if line.ItemName != devices.drink.Name || line.ItemImage != devices.drink.Image {
		t.Errorf("line snapshot = (%q, %q), want (%q, %q)",
			line.ItemName, line.ItemImage, devices.drink.Name, devices.drink.Image)
	}
	if line.CouponID != nil || line.CouponDiscountAmount != 0 || line.CampaignID != nil {
		t.Error("a device order has no coupon and no campaign; both were written")
	}
	// 行与主表的恒等式必须同时成立，否则库上的 CHECK 会拒掉这一单。
	if line.PayableAmount != line.OriginalUnitPrice*int64(line.Quantity)-line.DiscountAmount {
		t.Errorf("line payable %d does not match original %d - discount %d", line.PayableAmount, line.OriginalUnitPrice, line.DiscountAmount)
	}
	if line.DiscountAmount != line.PriceDiscountAmount+line.CouponDiscountAmount {
		t.Errorf("line discount %d != price %d + coupon %d", line.DiscountAmount, line.PriceDiscountAmount, line.CouponDiscountAmount)
	}
	if got.OriginalAmount != line.OriginalUnitPrice || got.PayableAmount != line.PayableAmount {
		t.Error("the order totals disagree with the only line on it")
	}
	// 门店名读设备那一条读不到（设备不带点位名），留空是如实的，不是丢字段。
	if got.StoreName != "" {
		t.Errorf("storeName = %q, want empty: the device read carries no store name", got.StoreName)
	}
}

// TestCreateDeviceOrderMarksAFailedBrew：出饮失败也建单（钱收了），履约汇总标 failed。
func TestCreateDeviceOrderMarksAFailedBrew(t *testing.T) {
	devices, repo := deviceOrderFixture()
	in := fullDeviceOrderInput()
	in.BrewFailed = true
	if _, err := newDeviceOrderService(devices, repo).CreateDeviceOrder(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.got.FulfillmentStatus != model.FulfillmentFailed {
		t.Errorf("fulfillmentStatus = %q, want %q", repo.got.FulfillmentStatus, model.FulfillmentFailed)
	}
}

// TestCreateDeviceOrderKeepsTheTwoMissesApart：设备没有与饮品没有是两个结论，各自的档也不同
// （前者 NotFound→404「没登记」，后者 InvalidArgument→400「改编号」，见 rpc 的分档表），
// 而且都不能变成 5xx。
func TestCreateDeviceOrderKeepsTheTwoMissesApart(t *testing.T) {
	t.Run("device not found", func(t *testing.T) {
		devices, repo := deviceOrderFixture()
		devices.deviceFound = false
		_, err := newDeviceOrderService(devices, repo).CreateDeviceOrder(context.Background(), fullDeviceOrderInput())
		if !errors.Is(err, ErrDeviceNotFound) {
			t.Fatalf("err = %v, want %v", err, ErrDeviceNotFound)
		}
		if devices.drinkCalls != 0 {
			t.Error("a missing device still went looking for a drink")
		}
		if repo.calls != 0 {
			t.Error("a missing device still wrote an order")
		}
	})
	t.Run("drink not found on this device", func(t *testing.T) {
		devices, repo := deviceOrderFixture()
		devices.drinkFound = false
		_, err := newDeviceOrderService(devices, repo).CreateDeviceOrder(context.Background(), fullDeviceOrderInput())
		if !errors.Is(err, ErrDrinkNotFound) {
			t.Fatalf("err = %v, want %v", err, ErrDrinkNotFound)
		}
		if errors.Is(err, ErrDeviceNotFound) {
			t.Error("the drink was reported as a missing device")
		}
		if repo.calls != 0 {
			t.Error("an unknown drink still wrote an order")
		}
	})
}

// TestCreateDeviceOrderReplaysInsteadOfFailing：对方重投时返回既有那张单、created=false，
// **不报错**——报错会让它一直重投下去。
func TestCreateDeviceOrderReplaysInsteadOfFailing(t *testing.T) {
	devices, repo := deviceOrderFixture()
	repo.created = false
	result, err := newDeviceOrderService(devices, repo).CreateDeviceOrder(context.Background(), fullDeviceOrderInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Created {
		t.Error("created = true, want false on an idempotent replay")
	}
	if result.OrderID == "" || result.OrderNo == "" {
		t.Error("a replay must return the existing order")
	}
}

// TestCreateDeviceOrderRejectsBadShapeWithoutTouchingAnything：三个必填缺一个就在本地停住，
// 既不问下游也不写库——一次注定失败的往返不该发生（钱已经收了，对方要的是快的那个答复）。
func TestCreateDeviceOrderRejectsBadShapeWithoutTouchingAnything(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateDeviceOrderInput)
		want   error
	}{
		{name: "missing third party order no", mutate: func(in *CreateDeviceOrderInput) { in.ThirdPartyOrderNo = "  " }, want: ErrThirdPartyOrderNoRequired},
		{name: "missing device serial", mutate: func(in *CreateDeviceOrderInput) { in.DeviceSerial = "" }, want: ErrDeviceSerialRequired},
		{name: "missing drink code", mutate: func(in *CreateDeviceOrderInput) { in.DrinkCode = "" }, want: ErrDrinkCodeRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devices, repo := deviceOrderFixture()
			in := fullDeviceOrderInput()
			tc.mutate(&in)
			_, err := newDeviceOrderService(devices, repo).CreateDeviceOrder(context.Background(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !IsValidationError(err) {
				t.Errorf("%v is not in ValidationErrors; partner-service would see it as our fault", tc.want)
			}
			if devices.deviceCalls != 0 || devices.drinkCalls != 0 || repo.calls != 0 {
				t.Error("a bad shape reached the network or the database")
			}
		})
	}
}

// TestCreateDeviceOrderLeavesTheStoreEmptyWhenTheDeviceHasNone：设备还没挂点位时可以建单，
// 订单上没有点位是如实记录（同 resolveDevice 的处理），不是丢字段。
func TestCreateDeviceOrderLeavesTheStoreEmptyWhenTheDeviceHasNone(t *testing.T) {
	devices, repo := deviceOrderFixture()
	devices.device.StoreID = ""
	if _, err := newDeviceOrderService(devices, repo).CreateDeviceOrder(context.Background(), fullDeviceOrderInput()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.got.StoreID != nil {
		t.Errorf("storeId = %q, want nil for a device that is not deployed anywhere", *repo.got.StoreID)
	}
}

// TestUserMatchesNeverMatchesAnOrderWithoutAUser 是归属判定的那一格：设备单没有用户，
// 它**不是**「谁都能看/能付/能取消」——nil 与任何调用方（包括后台用的空串）都不相等。
func TestUserMatchesNeverMatchesAnOrderWithoutAUser(t *testing.T) {
	if userMatches(nil, "") {
		t.Error("an order without a user matched the empty caller id")
	}
	if userMatches(nil, "11111111-1111-1111-1111-111111111111") {
		t.Error("an order without a user matched a real caller")
	}
	if !userMatches(userIDPtr(testUserID), testUserID) {
		t.Error("an order with a user did not match its own user")
	}
	if userMatches(userIDPtr(testUserID), "") {
		t.Error("an order with a user matched the empty caller id")
	}
}
