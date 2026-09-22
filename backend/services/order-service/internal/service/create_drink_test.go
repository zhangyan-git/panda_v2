package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// 这一层测的是「小程序下单买一杯饮品」这条路上**不碰网络、不碰库**的那部分：这杯卖多少钱、
// 名字是什么、什么时候不卖。
//
// 出事的样子与会员行那条路是同一个：一条客户端随手编的请求能一分钱买走一杯咖啡，而库里
// 看起来完全正常——行上写着「美式 12800」，或者更糟，写着「美式 1」。

const (
	testCatalogDrinkID = "5c4b3a29-2222-4333-8444-555566667777"
	testCatalogDevice  = "3f0a6a1e-1111-4222-8333-444455556666"
	testCatalogStore   = "9a8b7c6d-1111-4222-8333-444455556666"
	testCatalogIdemKey = "idem-drink-1"
)

// catalogDrinkFixture 是一台已挂点位、在售一杯目录价 1800 / 会员价 1500 的拿铁。
//
// 目录那份答案填在 catalog 上（按 uuid 取），不是 drink（按机器编号取）：两条路各收各的
// 钥匙，把同一份答案喂给两边就没法测「这一杯不在这台设备上」。
func catalogDrinkFixture() *fakeDeviceReader {
	return &fakeDeviceReader{
		device: &client.Device{
			ID: testCatalogDevice, StoreID: testCatalogStore,
			SerialUnique: testDeviceSerial, Status: "active",
		},
		deviceFound: true,
		catalog: &client.Drink{
			ID: testCatalogDrinkID, Name: "拿铁", ProductNum: "P001",
			Image: "https://cdn.example.com/latte.png",
			Price: 1800, VipPrice: 1500, Status: "on_shelf",
			DeviceID: testCatalogDevice,
		},
		catalogFound: true,
	}
}

// drinkOrder 是「客户端想花一分钱买一杯拿铁」那一次请求。
//
// 请求里所有能填的价格与商品快照都填成假的（1 分钱、假编码、假名字、假图），itemId 才是
// 唯一应当被采信的值。断言的核心就是这一点：那些假值一个都不许落到订单行上。
func drinkOrder(itemID string) dto.CreateOrderRequest {
	deviceID := testCatalogDevice
	one := int64(1)
	return dto.CreateOrderRequest{
		Source:   model.SourceMiniapp,
		DeviceID: &deviceID,
		Lines: []dto.CreateOrderLine{{
			LineType:            model.LineTypeDrink,
			ItemID:              &itemID,
			ItemCode:            "FAKE-CODE",
			ItemName:            "假美式",
			ItemImage:           "https://cdn.example.com/fake.png",
			Quantity:            1,
			OriginalUnitPrice:   one,
			UnitPrice:           one,
			PriceDiscountAmount: one,
		}},
	}
}

func newDrinkService(devices DeviceReader, plans MembershipPlanReader) (*fakeCreateRepo, *OrderService) {
	repo := &fakeCreateRepo{}
	// wallets 为 nil：下单不发起支付（见 newCreateService）。
	return repo, New(repo, devices, nil, plans, nil, nil, Options{Now: func() time.Time {
		return time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	}})
}

func createDrinkOrder(t *testing.T, svc *OrderService, itemID string) error {
	t.Helper()
	_, _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID:         testUserID,
		IdempotencyKey: testCatalogIdemKey,
		Request:        drinkOrder(itemID),
	})
	return err
}

// TestCreateOrderPricesADrinkLineFromTheCatalog 是这条路的核心不变量。
//
// 请求里那套「一分钱拿铁」**一个字段都不许**落到订单上：编码、名称、图片、原价、成交价、
// 优惠额与订单合计全部只能来自目录那一次回答。断言的是落库前那一份 params，不是响应体
// ——响应体是从仓储回来的，仓储写进去什么它说什么，看不出源头。
func TestCreateOrderPricesADrinkLineFromTheCatalog(t *testing.T) {
	devices := catalogDrinkFixture()
	plans := &stubPlanReader{}
	repo, svc := newDrinkService(devices, plans)

	if err := createDrinkOrder(t, svc, testCatalogDrinkID); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("仓储调用次数 = %d, want 1", repo.calls)
	}
	// 问的是请求里那个 itemId，一字不差。
	if devices.gotCatalogID != testCatalogDrinkID {
		t.Errorf("问目录的 id = %q, want %q", devices.gotCatalogID, testCatalogDrinkID)
	}
	// 会员资格问的是**下单的人**，不是某一台设备、也不是某一杯。
	if plans.gotEntitlementUser != testUserID {
		t.Errorf("问会员资格的人 = %q, want %q", plans.gotEntitlementUser, testUserID)
	}

	line := repo.got.Lines[0]
	for _, field := range []struct {
		name string
		got  int64
	}{
		{"行的目录价", line.OriginalUnitPrice},
		{"行的成交单价", line.UnitPrice},
		{"行的应付额", line.PayableAmount},
		{"订单的合计原价", repo.got.Order.OriginalAmount},
		{"订单的应付额", repo.got.Order.PayableAmount},
	} {
		if field.got != 1800 {
			t.Errorf("%s = %d, want 目录价 1800（客户端填了 1 分钱）", field.name, field.got)
		}
	}
	if line.ItemCode != "P001" || line.ItemName != "拿铁" || line.ItemImage != "https://cdn.example.com/latte.png" {
		t.Errorf("行的编码/名称/图片 = %q/%q/%q, want 目录里那一份",
			line.ItemCode, line.ItemName, line.ItemImage)
	}
	if line.ItemID == nil || *line.ItemID != testCatalogDrinkID {
		t.Errorf("行的 itemId = %v, want %q", line.ItemID, testCatalogDrinkID)
	}
	// 非会员：原价即成交价，优惠为 0。
	if line.PriceDiscountAmount != 0 || line.DiscountAmount != 0 || repo.got.Order.DiscountAmount != 0 {
		t.Errorf("非会员的优惠额 = %d/%d/%d, want 全 0",
			line.PriceDiscountAmount, line.DiscountAmount, repo.got.Order.DiscountAmount)
	}
	// 行的设备是订单上那台机器的副本：饮品行就是「某台设备上的一杯」。
	if line.DeviceID == nil || *line.DeviceID != testCatalogDevice {
		t.Errorf("行的 deviceId = %v, want %q", line.DeviceID, testCatalogDevice)
	}
	if repo.got.Order.FulfillmentStatus != model.FulfillmentPending {
		t.Errorf("履约状态 = %q, want %q（有饮品要出杯）",
			repo.got.Order.FulfillmentStatus, model.FulfillmentPending)
	}
}

// TestCreateOrderDrinkMemberPrice 逐种会员价情形钉住成交价。
//
// 三条例外（没配会员价、会员价不低于原价、会员域说这个人不享会员价）全部**按原价卖**：
// 多收的那一次是可退的，0 元卖出去的那一次不是。
func TestCreateOrderDrinkMemberPrice(t *testing.T) {
	cases := []struct {
		name     string
		vipPrice int64
		grants   bool
		wantUnit int64
	}{
		{name: "年度会员按会员价", vipPrice: 1500, grants: true, wantUnit: 1500},
		{name: "不是会员按原价", vipPrice: 1500, grants: false, wantUnit: 1800},
		{
			// 目录没给这一杯配会员价。0 在这里是「没配」，不是「会员价 0 元」。
			name: "没有会员价按原价", vipPrice: 0, grants: true, wantUnit: 1800,
		},
		{
			// 坏数据（后台的写接口不拦这个）：照它算会得出一个负的优惠额，而库上非负。
			name: "会员价不低于原价按原价", vipPrice: 1800, grants: true, wantUnit: 1800,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devices := catalogDrinkFixture()
			devices.catalog.VipPrice = tc.vipPrice
			repo, svc := newDrinkService(devices, &stubPlanReader{
				entitlement: &client.MemberPriceEntitlement{GrantsMemberPrice: tc.grants},
			})

			if err := createDrinkOrder(t, svc, testCatalogDrinkID); err != nil {
				t.Fatalf("CreateOrder: %v", err)
			}
			line := repo.got.Lines[0]
			if line.UnitPrice != tc.wantUnit {
				t.Errorf("成交单价 = %d, want %d", line.UnitPrice, tc.wantUnit)
			}
			// 原价那一格永远是目录价：会员价是优惠，不是「原来就卖这么贵」。
			if line.OriginalUnitPrice != 1800 {
				t.Errorf("目录价 = %d, want 1800", line.OriginalUnitPrice)
			}
			wantDiscount := int64(1800 - tc.wantUnit)
			if line.PriceDiscountAmount != wantDiscount || line.DiscountAmount != wantDiscount {
				t.Errorf("优惠额 = %d/%d, want %d",
					line.PriceDiscountAmount, line.DiscountAmount, wantDiscount)
			}
			if repo.got.Order.PayableAmount != tc.wantUnit {
				t.Errorf("订单应付额 = %d, want %d", repo.got.Order.PayableAmount, tc.wantUnit)
			}
			// 库上那条恒等式：payable = original − discount。
			if repo.got.Order.PayableAmount != repo.got.Order.OriginalAmount-repo.got.Order.DiscountAmount {
				t.Errorf("payable %d ≠ original %d − discount %d",
					repo.got.Order.PayableAmount, repo.got.Order.OriginalAmount, repo.got.Order.DiscountAmount)
			}
		})
	}
}

// TestCreateOrderDrinkVerdicts 覆盖「不卖这一杯」的五种结论，以及它们的档位。
//
// 分开判的不是措辞而是**调用方该做什么**：目录里没有（换一杯）、下架（换一杯）、
// 不在这台设备上（报文/请求错了）、问不到（稍后重试）。
func TestCreateOrderDrinkVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*fakeDeviceReader)
		plans   *stubPlanReader
		devices DeviceReader
		want    error
		// wantValidation 表示它该不该被当成「请求不合法」（HTTP 400）。
		wantValidation bool
	}{
		{
			name:   "目录里没有这一杯",
			mutate: func(d *fakeDeviceReader) { d.catalogFound = false },
			want:   ErrDrinkNotFound,
		},
		{
			name:   "这一杯已下架",
			mutate: func(d *fakeDeviceReader) { d.catalog.Status = "off_shelf" },
			want:   ErrDrinkOffShelf,
		},
		{
			// 「某台设备上的一杯」——用 A 店的饮品配 B 店的设备下单，价格与机器对不上而
			// 订单看起来完全合法。
			name:           "这一杯不在这台设备上",
			mutate:         func(d *fakeDeviceReader) { d.catalog.DeviceID = "6d5c4b3a-9999-4888-8777-666655554444" },
			want:           ErrDrinkDeviceMismatch,
			wantValidation: true,
		},
		{
			// 目录里没挂设备的遗留行：没有可比的设备，不是「挂错了设备」。
			name:   "目录里那一杯还没挂设备",
			mutate: func(d *fakeDeviceReader) { d.catalog.DeviceID = "" },
			want:   nil,
		},
		{
			// 没见过的状态一律按不卖（default-deny）：词表将来多一档时，不会变成一次无声的放行。
			name:   "状态是将来才有的第三档",
			mutate: func(d *fakeDeviceReader) { d.catalog.Status = "hidden" },
			want:   ErrDrinkOffShelf,
		},
		{
			name:   "目录服务没答上来",
			mutate: func(d *fakeDeviceReader) { d.catalogErr = errors.New("connection refused") },
			want:   ErrDrinkLookupUnavailable,
		},
		{
			name:  "会员服务没答上来",
			plans: &stubPlanReader{entitlementErr: ErrMemberPriceUnavailable},
			want:  ErrMemberPriceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devices := catalogDrinkFixture()
			if tc.mutate != nil {
				tc.mutate(devices)
			}
			plans := tc.plans
			if plans == nil {
				plans = &stubPlanReader{}
			}
			repo, svc := newDrinkService(devices, plans)

			err := createDrinkOrder(t, svc, testCatalogDrinkID)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if repo.calls != 0 {
				t.Fatalf("结论还没拿到就落了单：仓储调用 %d 次", repo.calls)
			}
			if !IsValidationError(err) && tc.wantValidation {
				t.Errorf("%v 该被当成请求不合法（400）", err)
			}
			// 下架与我们答不上来这两种**不该**落进「请求不合法」：请求一个字都没写错。
			// 混进去会让客户端把「运营下架了」读成「你手里的链接是坏的」，或者把一次下游
			// 抖动读成用户填错了东西。
			if IsValidationError(err) && !tc.wantValidation {
				t.Errorf("%v 不该被当成请求不合法（400）", err)
			}
		})
	}
}

// TestCreateOrderChargesTheMemberPricePerCup 钉住「优惠额是行级金额，不是单价差」。
//
// 少乘这一次数量不撞任何约束：discount = price + coupon 与 payable = 原价 × 数量 − discount
// 两条式子同时少算同一笔钱，照样自洽，结账页那一行也看不出异常。只有把数量写成 2 才看得见
// ——买两杯，用户被多收一份差价（1800/1500 时是 3300 而不是 3000）。
func TestCreateOrderChargesTheMemberPricePerCup(t *testing.T) {
	devices := catalogDrinkFixture()
	repo, svc := newDrinkService(devices, &stubPlanReader{
		entitlement: &client.MemberPriceEntitlement{GrantsMemberPrice: true},
	})

	request := drinkOrder(testCatalogDrinkID)
	request.Lines[0].Quantity = 2
	if _, _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: testUserID, IdempotencyKey: testCatalogIdemKey, Request: request,
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	line := repo.got.Lines[0]
	// 每杯省 300，两杯省 600。单价那两格仍是**每杯**的价，不跟着数量走。
	if line.OriginalUnitPrice != 1800 || line.UnitPrice != 1500 {
		t.Errorf("原价/成交价 = %d/%d, want 1800/1500（单价不随数量变）",
			line.OriginalUnitPrice, line.UnitPrice)
	}
	if line.PriceDiscountAmount != 600 {
		t.Errorf("会员价优惠 = %d, want 600（每杯 300 × 2 杯）", line.PriceDiscountAmount)
	}
	if line.PayableAmount != 3000 {
		t.Errorf("行应付额 = %d, want 3000（1800 × 2 − 600）", line.PayableAmount)
	}
	if repo.got.Order.PayableAmount != 3000 {
		t.Errorf("订单应付额 = %d, want 3000", repo.got.Order.PayableAmount)
	}
	// 库上那两条恒等式。
	if line.PayableAmount != line.OriginalUnitPrice*int64(line.Quantity)-line.DiscountAmount {
		t.Error("payable ≠ 原价 × 数量 − discount")
	}
	if line.DiscountAmount != line.PriceDiscountAmount+line.CouponDiscountAmount {
		t.Error("discount ≠ price + coupon")
	}
}

// TestCreateOrderFailsClosedWithoutADrinkReader 是这条路最重要的一条兜底：
// 读端没配齐时**绝不退化成「那就用请求里那个价」**。
//
// 没有目录读端那一格回的其实是「问不到设备」：设备校验排在目录之前（见 CreateOrder 的顺序），
// 所以那条路根本走不到目录这里。这里如实断言先撞上的那一档——顺着写一个 DrinkLookup 的期望
// 会让人以为这两条顺序可以互换。
//
// plans 为 nil 那一格才是这一刀新加的：饮品行的定价要问会员域一句，问不到就该停在这——
// 退回原价继续下单会把一次下游抖动变成「悄悄按原价卖给了会员」，用户不会知道，我们也不会。
func TestCreateOrderFailsClosedWithoutADrinkReader(t *testing.T) {
	cases := []struct {
		name    string
		devices DeviceReader
		plans   MembershipPlanReader
		want    error
	}{
		{
			name:    "没有设备读端",
			devices: nil, plans: &stubPlanReader{},
			want: ErrDeviceLookupUnavailable,
		},
		{
			name:    "没有会员读端",
			devices: DeviceReader(catalogDrinkFixture()),
			plans:   nil,
			want:    ErrMemberPriceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, svc := newDrinkService(tc.devices, tc.plans)
			err := createDrinkOrder(t, svc, testCatalogDrinkID)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if repo.calls != 0 {
				t.Fatalf("读端都没配齐却落了单：仓储调用 %d 次", repo.calls)
			}
			if IsValidationError(err) {
				t.Errorf("%v 不该被当成请求不合法：它是我们这侧答不上来（503）", err)
			}
		})
	}
}

// TestCreateOrderRejectsBadDrinkLinesWithoutAskingAnyone 覆盖请求形状。
//
// 每一条都断言**谁都没问过**：形状不对就该在本地停住。itemId 不是 uuid 那次尤其明显——
// 明知会拿回一个 InvalidArgument 还要跑一趟，是把一次必然失败的往返挂在用户的等待上。
func TestCreateOrderRejectsBadDrinkLinesWithoutAskingAnyone(t *testing.T) {
	blank := "   "
	notUUID := "LATTE-001"

	cases := []struct {
		name   string
		itemID *string
		want   error
	}{
		{name: "没给 itemId", itemID: nil, want: ErrDrinkItemIDRequired},
		{name: "itemId 是空白", itemID: &blank, want: ErrDrinkItemIDRequired},
		{name: "itemId 不是 uuid", itemID: &notUUID, want: ErrDrinkItemIDInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devices := catalogDrinkFixture()
			plans := &stubPlanReader{}
			repo, svc := newDrinkService(devices, plans)

			request := drinkOrder(testCatalogDrinkID)
			request.Lines[0].ItemID = tc.itemID
			_, _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
				UserID:         testUserID,
				IdempotencyKey: testCatalogIdemKey,
				Request:        request,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if devices.catalogCalls != 0 || plans.entitlementCalls != 0 || repo.calls != 0 {
				t.Errorf("形状不对却问了人：目录 %d 次、会员 %d 次、仓储 %d 次",
					devices.catalogCalls, plans.entitlementCalls, repo.calls)
			}
		})
	}
}

// TestCreateOrderKeepsTheCouponOnTopOfTheCatalogPrice 钉住这一刀之后仅存的那一处
// 「调用方定价」：券抵多少仍由请求给，但它只减应付额，不动原价与会员价那两格。
//
// 恒等式（库上的 order_lines_discount_breakdown）是：
//
//	discount = price_discount + coupon_discount
//	payable  = 目录价 × 数量 − discount
func TestCreateOrderKeepsTheCouponOnTopOfTheCatalogPrice(t *testing.T) {
	devices := catalogDrinkFixture()
	repo, svc := newDrinkService(devices, &stubPlanReader{
		entitlement: &client.MemberPriceEntitlement{GrantsMemberPrice: true},
	})

	couponID := "7c6b5a49-1111-4222-8333-444455556666"
	request := drinkOrder(testCatalogDrinkID)
	request.Lines[0].CouponID = &couponID
	request.Lines[0].CouponDiscountAmount = 200
	if _, _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID: testUserID, IdempotencyKey: testCatalogIdemKey, Request: request,
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	line := repo.got.Lines[0]
	// 目录价 1800、会员价 1500 → 会员价优惠 300；券再抵 200。
	if line.OriginalUnitPrice != 1800 || line.UnitPrice != 1500 {
		t.Errorf("原价/成交价 = %d/%d, want 1800/1500", line.OriginalUnitPrice, line.UnitPrice)
	}
	if line.PriceDiscountAmount != 300 {
		t.Errorf("会员价优惠 = %d, want 300（券不该混进这一格）", line.PriceDiscountAmount)
	}
	if line.DiscountAmount != 500 {
		t.Errorf("总优惠 = %d, want 300 + 200 = 500", line.DiscountAmount)
	}
	if line.PayableAmount != 1300 {
		t.Errorf("行应付额 = %d, want 1800 − 500 = 1300", line.PayableAmount)
	}
	if repo.got.Order.PayableAmount != 1300 {
		t.Errorf("订单应付额 = %d, want 1300", repo.got.Order.PayableAmount)
	}
}
