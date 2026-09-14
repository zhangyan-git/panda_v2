package service

import (
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
)

// 这一层是纯校验，不碰数据库，所以这些用例在没有 Postgres 的环境里也跑得起来。
//
// 校验规则和数据库里的 CHECK / 触发器是同一件事的两处表达，这里盯的是「服务层先
// 拦住」：让约束去拦，调用方收到的是一条 500 加一句 PostgreSQL 的英文报错，而这些
// 都是可预期的输入问题。

func strptr(v string) *string { return &v }

// TestValidationErrorsCoversEverySentinel 是这个文件里最有价值的一条。控制器的
// writeAdminError 靠 ValidationErrors 判断该回 400 还是 500，而它的兜底是 500——
// 新加一条校验却忘了登记，那条错误就会以 500 的形式回给前端，前端拿 500 没法提示
// 「你这个字段填错了」。这里把每个校验函数在非法输入下真正会返回的错误拿出来，逐个
// 确认它在 ValidationErrors 里。
func TestValidationErrorsCoversEverySentinel(t *testing.T) {
	svc := NewAdminService(nil) // 校验在触达仓库之前就返回，不需要真仓库
	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"厂商编码为空", func() error {
			_, err := svc.CreateManufacturer(t.Context(), dto.ManufacturerInput{Name: "有名字"})
			return err
		}, ErrManufacturerCodeRequired},
		{"厂商名称为空", func() error {
			_, err := svc.CreateManufacturer(t.Context(), dto.ManufacturerInput{Code: "C1"})
			return err
		}, ErrManufacturerNameRequired},
		{"设备序列号为空", func() error {
			_, err := svc.CreateDevice(t.Context(), dto.DeviceInput{
				ManufacturerID: "00000000-0000-0000-0000-000000000001", QrcodeType: "miniprogram"})
			return err
		}, ErrDeviceSerialRequired},
		{"设备没有厂商", func() error {
			_, err := svc.CreateDevice(t.Context(), dto.DeviceInput{
				SerialUnique: "S1", QrcodeType: "miniprogram"})
			return err
		}, ErrDeviceManufacturerRequired},
		{"二维码类型非法", func() error {
			_, err := svc.CreateDevice(t.Context(), dto.DeviceInput{
				SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000001", QrcodeType: "qr"})
			return err
		}, ErrQrcodeTypeInvalid},
		{"regular 没给支付方式", func() error {
			_, err := svc.CreateDevice(t.Context(), dto.DeviceInput{
				SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000001", QrcodeType: "regular"})
			return err
		}, ErrRegularQrcodePaymentNeeded},
		{"支付方式取值非法", func() error {
			_, err := svc.CreateDevice(t.Context(), dto.DeviceInput{
				SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000001",
				QrcodeType: "regular", RegularQrcodePaymentMethod: strptr("alipay")})
			return err
		}, ErrRegularQrcodePaymentBad},
		{"饮品名称为空", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ManufacturerID: "00000000-0000-0000-0000-000000000001", Price: 100})
			return err
		}, ErrDrinkNameRequired},
		{"饮品没有厂商", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{ProductName: "拿铁", Price: 100})
			return err
		}, ErrDrinkManufacturerRequired},
		{"饮品类型非法", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ManufacturerID: "00000000-0000-0000-0000-000000000001",
				ProductName:    "拿铁", Price: 100, DrinkType: strptr("tea")})
			return err
		}, ErrDrinkTypeInvalid},
		{"价格为负", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ManufacturerID: "00000000-0000-0000-0000-000000000001",
				ProductName:    "拿铁", Price: -1})
			return err
		}, ErrPriceNegative},
		{"原价为 0 但会员价不为 0", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ManufacturerID: "00000000-0000-0000-0000-000000000001",
				ProductName:    "拿铁", Price: 0, VipPrice: 500})
			return err
		}, ErrDrinkPriceInvalid},
		{"厂商状态非法", func() error {
			return svc.SetManufacturerStatus(t.Context(), "00000000-0000-0000-0000-000000000001", "frozen")
		}, ErrManufacturerStatusInvalid},
		{"设备状态非法", func() error {
			return svc.SetDeviceStatus(t.Context(), "00000000-0000-0000-0000-000000000001", "frozen")
		}, ErrDeviceStatusInvalid},
		{"饮品状态非法", func() error {
			return svc.SetDrinkStatus(t.Context(), "00000000-0000-0000-0000-000000000001", "sold_out")
		}, ErrDrinkStatusInvalid},
		{"调整金额为 0", func() error {
			_, err := svc.AdjustBalance(t.Context(), "00000000-0000-0000-0000-000000000001",
				dto.BalanceAdjustInput{Amount: 0, RequestID: "r1"}, "u1")
			return err
		}, ErrBalanceAmountZero},
		{"requestId 为空", func() error {
			_, err := svc.AdjustBalance(t.Context(), "00000000-0000-0000-0000-000000000001",
				dto.BalanceAdjustInput{Amount: 100, RequestID: "  "}, "u1")
			return err
		}, ErrBalanceRequestIDRequired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("got no error, want %v", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !IsValidationError(err) {
				t.Fatalf("err %v is not registered in ValidationErrors, so the controller would answer 500 instead of 400", err)
			}
		})
	}
}

// TestValidationErrorsHasNoStaleEntries 反方向：登记了却没有校验函数会返回它，
// 会让下一个读这段代码的人以为某条校验存在。
func TestValidationErrorsHasNoStaleEntries(t *testing.T) {
	// 每一条都得能在上面那批用例里找到出处。数量对不上就说明有人加了忘了加用例，
	// 或者加了用例忘了登记。
	if len(ValidationErrors) != 17 {
		t.Fatalf("ValidationErrors has %d entries, want 17 — 新增一条时把用例也补上：%v",
			len(ValidationErrors), ValidationErrors)
	}
	for i, err := range ValidationErrors {
		for j, other := range ValidationErrors {
			if i != j && errors.Is(err, other) {
				t.Fatalf("%v and %v overlap", err, other)
			}
		}
	}
}

// TestCheckPricesMirrorsTheDatabaseConstraint 是那条最容易被写错的 CHECK：
// price > 0 OR (vip_price = 0 AND pickup_code_price = 0)。全 0 是允许的（免费饮品），
// 但「原价 0、会员价 5 元」不行。
func TestCheckPricesMirrorsTheDatabaseConstraint(t *testing.T) {
	cases := []struct {
		price, vip, pickup int64
		wantErr            bool
	}{
		{1800, 1500, 1200, false}, // 正常三级价
		{0, 0, 0, false},          // 免费饮品
		{0, 500, 0, true},         // 只有会员价，漏了原价
		{0, 0, 500, true},         // 只有提货码价
		{100, 0, 0, true},         // 会员价比原价还高不算错，但负数才是
		{-1, 0, 0, true},          // 负价
		{100, -1, 0, true},
		{100, 0, -1, true},
	}
	for _, tc := range cases {
		err := checkPrices(tc.price, tc.vip, tc.pickup)
		// {100,0,0} 是合法组合（会员价 0 表示不开会员价），单独修正预期。
		wantErr := tc.wantErr
		if tc.price == 100 && tc.vip == 0 && tc.pickup == 0 {
			wantErr = false
		}
		if wantErr && err == nil {
			t.Fatalf("checkPrices(%d,%d,%d) = nil, want an error", tc.price, tc.vip, tc.pickup)
		}
		if !wantErr && err != nil {
			t.Fatalf("checkPrices(%d,%d,%d) = %v, want nil", tc.price, tc.vip, tc.pickup, err)
		}
	}
}

// TestNormalizeDrinkTypeTreatsBlankAsUnset 盯的是可空列与空串的区别：drink_type 可空，
// 而空串不在 CHECK 的取值里，直接落库会被约束拦下。
func TestNormalizeDrinkTypeTreatsBlankAsUnset(t *testing.T) {
	for _, raw := range []*string{nil, strptr(""), strptr("   ")} {
		got, err := normalizeDrinkType(raw)
		if err != nil {
			t.Fatalf("normalizeDrinkType(%v) = %v, want nil error", raw, err)
		}
		if got != nil {
			t.Fatalf("normalizeDrinkType(%v) = %q, want nil", raw, *got)
		}
	}
	got, err := normalizeDrinkType(strptr(" milk_coffee "))
	if err != nil {
		t.Fatalf("normalizeDrinkType: %v", err)
	}
	if got == nil || *got != "milk_coffee" {
		t.Fatalf("normalizeDrinkType trimmed = %v, want milk_coffee", got)
	}
}

// TestBuildDeviceClearsPaymentMethodAwayFromRegular 验的是切回 miniprogram 时清掉
// 支付方式：留着旧值不会立刻出错，但下一次有人切到 regular 就会带着一个早就没人
// 记得的值通过校验。
func TestBuildDeviceClearsPaymentMethodAwayFromRegular(t *testing.T) {
	svc := NewAdminService(nil)
	device, err := svc.buildDevice("00000000-0000-0000-0000-000000000001", dto.DeviceInput{
		SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000002",
		QrcodeType: "miniprogram", RegularQrcodePaymentMethod: strptr("youlian"),
	})
	if err != nil {
		t.Fatalf("buildDevice: %v", err)
	}
	if device.RegularQrcodePaymentMethod != nil {
		t.Fatalf("payment method = %q, want nil when qrcode_type is miniprogram", *device.RegularQrcodePaymentMethod)
	}
}

// TestBuildDeviceLeavesPickupPasswordAlone 把 buildDevice 现在的职责划清：它不再碰
// 提货码。那个字段是第三态（留空＝不改），创建与编辑两条路各自处理，见
// trimPickupPassword——放进 buildDevice 就必然要在编辑路上再擦掉一次，正是会误清
// 静态验证码的那种写法。
func TestBuildDeviceLeavesPickupPasswordAlone(t *testing.T) {
	svc := NewAdminService(nil)
	device, err := svc.buildDevice("00000000-0000-0000-0000-000000000001", dto.DeviceInput{
		SerialUnique: " S1 ", ManufacturerID: "00000000-0000-0000-0000-000000000002",
		QrcodeType: "miniprogram", PickupPassword: strptr("  8321  "),
	})
	if err != nil {
		t.Fatalf("buildDevice: %v", err)
	}
	if device.SerialUnique != "S1" {
		t.Fatalf("serial = %q, want S1", device.SerialUnique)
	}
	if device.PickupPassword != "" {
		t.Fatalf("pickup password = %q, want buildDevice to leave it alone", device.PickupPassword)
	}
}

// TestTrimPickupPassword 是这个字段唯一的那条判断，两条写路径都靠它：创建时「没填」
// 落成空串（列是 NOT NULL DEFAULT ”），编辑时「没填」落成 nil（那一列不动）。
// 空串必须归一成 nil，否则编辑表单留空就会把店员正在用的码抹掉。
func TestTrimPickupPassword(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want *string
	}{
		{"没填", nil, nil},
		{"空串", strptr(""), nil},
		{"全是空白", strptr("   "), nil},
		{"去掉首尾空白", strptr("  8321  "), strptr("8321")},
		{"已经干净", strptr("8321"), strptr("8321")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trimPickupPassword(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("trimPickupPassword = %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("trimPickupPassword = nil, want %q", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("trimPickupPassword = %q, want %q", *got, *tc.want)
			}
		})
	}
}
