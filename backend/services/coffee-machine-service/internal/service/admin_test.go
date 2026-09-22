package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// 这一层是纯校验，不碰数据库，所以这些用例在没有 Postgres 的环境里也跑得起来。
//
// 校验规则和数据库里的 CHECK / 触发器是同一件事的两处表达，这里盯的是「服务层先
// 拦住」：让约束去拦，调用方收到的是一条 500 加一句 PostgreSQL 的英文报错，而这些
// 都是可预期的输入问题。

func strptr(v string) *string { return &v }

// 这批用例里绝大多数只关心某一个字段，其余字段给一份合法值。设备 id 用一个固定的
// 合法 UUID——它在这里不会被拿去查库，只为通过形状校验。
const validDeviceID = "00000000-0000-0000-0000-0000000000aa"

// fakeStoreResolver 顶替 main 里那个真的 internal/client.StoreResolver。测的是本层
// 拿到三种回答之后各做什么（不存在 / 停用 / 问不到），而不是 gRPC 怎么发出去——那
// 是 internal/client 自己的事。
type fakeStoreResolver struct {
	status string
	found  bool
	err    error
	// asked 记下被问到的点位 id。有人把校验悄悄挪到 buildDevice 里、或者根本不调用
	// Resolve 的时候，光看返回值看不出来，要看这里。
	asked []string
}

func (f *fakeStoreResolver) Resolve(_ context.Context, storeID string) (string, bool, error) {
	f.asked = append(f.asked, storeID)
	return f.status, f.found, f.err
}

// activeStore 是「点位存在且可用」这个最常见回答，给那些不关心点位校验、只想过
// 别的字段的用例用。
func activeStore() *fakeStoreResolver {
	return &fakeStoreResolver{status: "active", found: true}
}

// fakeAdminRepo 顶替写仓储，只为「这次有没有换点位」这件事服务：current 是它回答的
// 「设备现在挂在哪儿」，updateCalls 记下有没有真的走到落库那一步。
//
// 内嵌接口是为了只实现这几条用例真正走到的方法——走到别的会 nil panic，那是故意的：
// 它说明用例越界了，不该悄悄返回个零值把事情糊过去。这样这个文件仍然不需要 Postgres。
type fakeAdminRepo struct {
	repository.AdminRepository
	current     *string
	updateCalls int
	// drink 记下最后一次饮品写入的入参。deviceId 是整行覆盖语义（不传就是「不挂
	// 设备」），这件事在返回值上看不出来，只能看落库前的那一份。
	drink *model.Drink
	// manufacturerID 是 DeviceManufacturerID 的回答，也就是「设备上挂的那个厂商」。
	manufacturerID string
}

func (f *fakeAdminRepo) DeviceStoreID(context.Context, string) (*string, error) {
	return f.current, nil
}

func (f *fakeAdminRepo) DeviceManufacturerID(context.Context, string) (string, error) {
	return f.manufacturerID, nil
}

func (f *fakeAdminRepo) CreateDrink(_ context.Context, d *model.Drink) (*model.Drink, error) {
	f.drink = d
	return d, nil
}

func (f *fakeAdminRepo) UpdateDevice(context.Context, *model.Device, *string) error {
	f.updateCalls++
	return nil
}

func (f *fakeAdminRepo) UpdateDrink(_ context.Context, d *model.Drink) error {
	f.drink = d
	return nil
}

// TestValidationErrorsCoversEverySentinel 是这个文件里最有价值的一条。控制器的
// writeAdminError 靠 ValidationErrors 判断该回 400 还是 500，而它的兜底是 500——
// 新加一条校验却忘了登记，那条错误就会以 500 的形式回给前端，前端拿 500 没法提示
// 「你这个字段填错了」。这里把每个校验函数在非法输入下真正会返回的错误拿出来，逐个
// 确认它在 ValidationErrors 里。
func TestValidationErrorsCoversEverySentinel(t *testing.T) {
	// 校验在触达仓库之前就返回，不需要真仓库；点位校验器给一个「存在且可用」的，
	// 这样这批用例只盯它们各自那一栏。
	svc := NewAdminService(nil, activeStore())
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
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{Price: 100})
			return err
		}, ErrDrinkNameRequired},
		{"新建饮品没有设备", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{ProductName: "拿铁", Price: 100})
			return err
		}, ErrDrinkDeviceRequired},
		{"deviceId 不是 uuid", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ProductName: "拿铁", Price: 100, DeviceID: strptr("设备一")})
			return err
		}, ErrDrinkDeviceInvalid},
		{"饮品类型非法", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ProductName: "拿铁", Price: 100, DrinkType: strptr("tea"), DeviceID: strptr(validDeviceID)})
			return err
		}, ErrDrinkTypeInvalid},
		{"价格为负", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ProductName: "拿铁", Price: -1, DeviceID: strptr(validDeviceID)})
			return err
		}, ErrPriceNegative},
		{"原价为 0 但会员价不为 0", func() error {
			_, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
				ProductName: "拿铁", Price: 0, VipPrice: 500, DeviceID: strptr(validDeviceID)})
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
		// 点位这两条以前不存在：store_id 是裸值引用，填什么都能存下去。
		//
		// 新建这条走不到仓库，拿 nil 仓储就够；编辑那条要先读「设备现在挂在哪儿」才能
		// 判断这次换没换点位，所以得有个仓储——下面那个 fakeAdminRepo。
		{"点位不存在", func() error {
			resolver := &fakeStoreResolver{found: false}
			_, err := NewAdminService(nil, resolver).CreateDevice(t.Context(), dto.DeviceInput{
				SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000001",
				QrcodeType: "miniprogram", StoreID: strptr("00000000-0000-0000-0000-0000000000ff")})
			return err
		}, ErrStoreNotFound},
		{"换到已停用的点位", func() error {
			resolver := &fakeStoreResolver{status: "disabled", found: true}
			// 设备原本挂在另一个点位上：这次是**换**点位，必须校验。
			repo := &fakeAdminRepo{current: strptr("00000000-0000-0000-0000-0000000000aa")}
			_, err := NewAdminService(repo, resolver).UpdateDevice(t.Context(),
				"00000000-0000-0000-0000-000000000001", dto.DeviceInput{
					SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000001",
					QrcodeType: "miniprogram", StoreID: strptr("00000000-0000-0000-0000-0000000000ff")})
			return err
		}, ErrStoreDisabled},
		{"从没挂点位到挂上已停用的点位", func() error {
			resolver := &fakeStoreResolver{status: "disabled", found: true}
			// 原来没挂（nil）：这是第一次挂，同样是换点位，必须校验。
			repo := &fakeAdminRepo{}
			_, err := NewAdminService(repo, resolver).UpdateDevice(t.Context(),
				"00000000-0000-0000-0000-000000000001", dto.DeviceInput{
					SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000001",
					QrcodeType: "miniprogram", StoreID: strptr("00000000-0000-0000-0000-0000000000ff")})
			return err
		}, ErrStoreDisabled},
		// 编辑时同样要挡住格式错的 deviceId：它与新建走的是同一列，绕开新建那条路
		// 一样能把 uuid 列的 22P02 变成 500。
		{"编辑时 deviceId 不是 uuid", func() error {
			_, err := svc.UpdateDrink(t.Context(), "00000000-0000-0000-0000-000000000002",
				dto.DrinkUpdateInput{ProductName: "拿铁", Price: 100, DeviceID: strptr("abc")})
			return err
		}, ErrDrinkDeviceInvalid},
		// 设备回调取饮品（gRPC GetDeviceDrink）的两个参数。它们只从 gRPC 进来，但登记进
		// ValidationErrors 是同一件事：不登记，rpc 层按 IsValidationError 判断时就会漏。
		{"取饮品没给设备", func() error {
			_, err := NewMasterDataService(nil).GetDeviceDrink(t.Context(), "", "1001")
			return err
		}, ErrDrinkLookupDeviceRequired},
		{"取饮品没给编号", func() error {
			_, err := NewMasterDataService(nil).GetDeviceDrink(t.Context(), validDeviceID, "  ")
			return err
		}, ErrDrinkLookupCodeRequired},
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
	if len(ValidationErrors) != 22 {
		t.Fatalf("ValidationErrors has %d entries, want 22 — 新增一条时把用例也补上：%v",
			len(ValidationErrors), ValidationErrors)
	}
	for i, err := range ValidationErrors {
		for j, other := range ValidationErrors {
			if i != j && errors.Is(err, other) {
				t.Fatalf("%v and %v overlap", err, other)
			}
		}
	}
	// 反方向里最要紧的一条：ErrStoreUnavailable 说的是「问不到」，不是「输入不对」。
	// 它要是被登记进来，控制器就会把一次商户服务故障回成 400，前端会去让用户改一个
	// 本来就对的 storeId。
	for _, registered := range ValidationErrors {
		if registered == ErrStoreUnavailable {
			t.Fatal("ErrStoreUnavailable 不是输入问题，控制器为它回 503；登进 ValidationErrors 会让它变成 400")
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

// TestCreateDrinkTakesManufacturerFromTheDevice 钉住「厂商跟着设备走」这条：新建饮品
// 时厂商不由调用方给，而是拿 deviceId 去设备上现取。
//
// dto.DrinkInput 里已经没有 manufacturerId 这个字段了，所以这条只能从落库的那一份上看
// ——接口层面「传不进来」是编译期的事，这里管的是服务层有没有真去问设备。
func TestCreateDrinkTakesManufacturerFromTheDevice(t *testing.T) {
	const deviceID = "00000000-0000-0000-0000-000000000001"
	const manufacturerID = "00000000-0000-0000-0000-0000000000ab"

	repo := &fakeAdminRepo{manufacturerID: manufacturerID}
	svc := NewAdminService(repo, activeStore())

	got, err := svc.CreateDrink(t.Context(), dto.DrinkInput{
		DeviceID: strptr(deviceID), ProductName: "拿铁", Price: 1800})
	if err != nil {
		t.Fatalf("CreateDrink: %v", err)
	}
	if repo.drink == nil {
		t.Fatal("没有走到落库")
	}
	if repo.drink.ManufacturerID != manufacturerID || got.ManufacturerID != manufacturerID {
		t.Fatalf("manufacturerId = %q/%q, want %q（应当取自设备）",
			repo.drink.ManufacturerID, got.ManufacturerID, manufacturerID)
	}
	if repo.drink.DeviceID == nil || *repo.drink.DeviceID != deviceID {
		t.Fatalf("deviceId = %v, want %s", repo.drink.DeviceID, deviceID)
	}
}

// TestUpdateDrinkCarriesDeviceAsAWholeRow 钉住编辑饮品时 deviceId 的语义：整行覆盖，
// 不传就是「从设备上摘下来」，不是「保持原样」。
//
// 这一条重要，是因为设备详情那一屏的改价、改排序走的正是这个接口：前端漏传 deviceId
// 就等于把这一行从设备上摘掉，而摘掉之后它在设备页上直接消失——看起来像「改个价把
// 饮品删了」。所以前端必须整行回传，这里把服务层的行为钉死，让它的代价是可预期的。
//
// 归一化顺带把 uuid 收敛成规范写法：库里存的是 uuid 类型，几种写法本来就等价，
// 提前统一可以让审计里的 before/after 比对不会因为大小写不同而显得「改过」。
func TestUpdateDrinkCarriesDeviceAsAWholeRow(t *testing.T) {
	const drinkID = "00000000-0000-0000-0000-000000000002"
	const deviceID = "00000000-0000-0000-0000-000000000001"

	repo := &fakeAdminRepo{}
	svc := NewAdminService(repo, activeStore())

	// 大写写法：应当被规范化成小写再落库。
	upper := strings.ToUpper(deviceID)
	if _, err := svc.UpdateDrink(t.Context(), drinkID, dto.DrinkUpdateInput{
		DeviceID: strptr(upper), ProductName: "拿铁", Price: 1800, Sort: 3}); err != nil {
		t.Fatalf("UpdateDrink: %v", err)
	}
	if repo.drink == nil {
		t.Fatal("没有走到落库")
	}
	if repo.drink.DeviceID == nil || *repo.drink.DeviceID != deviceID {
		t.Fatalf("deviceId = %v, want %s", repo.drink.DeviceID, deviceID)
	}
	if repo.drink.Price != 1800 || repo.drink.Sort != 3 {
		t.Fatalf("price/sort = %d/%d, want 1800/3", repo.drink.Price, repo.drink.Sort)
	}

	// 不传 → 摘下来。这是「整行覆盖」与前一条用例一起构成的一对：一个证明值被透传，
	// 一个证明缺值是 nil 而不是被当成空串。
	if _, err := svc.UpdateDrink(t.Context(), drinkID, dto.DrinkUpdateInput{
		ProductName: "拿铁", Price: 1800}); err != nil {
		t.Fatalf("UpdateDrink: %v", err)
	}
	if repo.drink.DeviceID != nil {
		t.Fatalf("deviceId = %v, want nil", repo.drink.DeviceID)
	}

	// 全是空白与不传等价：界面上清空输入框得到的是空串，不该被当成一个设备 id 送去
	// 撞 uuid 列。
	if _, err := svc.UpdateDrink(t.Context(), drinkID, dto.DrinkUpdateInput{
		DeviceID: strptr("   "), ProductName: "拿铁", Price: 1800}); err != nil {
		t.Fatalf("UpdateDrink: %v", err)
	}
	if repo.drink.DeviceID != nil {
		t.Fatalf("deviceId = %v, want nil（空白串应当在服务层就被归一成没填）", repo.drink.DeviceID)
	}
}

// TestCheckStore 圈住 checkStore 的每条出口。它是一个跨服务的值引用校验：本库没有
// 外键能拦 store_id，所以「这个点位存不存在、还能不能用」只能问商户服务。
//
// 每种回答各自该做什么是这次改动的核心判断，逐条钉住：
//   - 摘掉点位（nil）不用问，它没指向任何东西；
//   - 形状就不是 uuid 的：不必问。问也只会换来一个 500 级的错误，见下面那条用例；
//   - 存在且可用：放行；
//   - 不存在 / 已停用：这是输入问题，回 400 让调用方改参数；
//   - 问不到：**不是**输入问题，也绝不放行。放行等于校验只在商户服务正常时才生效。
func TestCheckStore(t *testing.T) {
	storeID := "00000000-0000-0000-0000-0000000000ff"
	cases := []struct {
		name        string
		storeID     *string
		resolver    *fakeStoreResolver
		wantErr     error
		wantAsks    int
		wantIsInput bool
	}{
		{"摘掉点位不校验", nil, activeStore(), nil, 0, false},
		// wantAsks 是 0 才是这条的重点：商户库那一列是 uuid，把 "abc" 递过去 Postgres
		// 抛 22P02 而不是 ErrNoRows，一路上来会变成 503「门店服务暂时不可用」。填错一个
		// id 却被告知对方服务挂了，重试永远不会好。所以要在问之前就认出它不是 id。
		{"storeId 不是 uuid", strptr("abc"), activeStore(), ErrStoreNotFound, 0, true},
		{"点位存在且可用", strptr(storeID), activeStore(), nil, 1, false},
		{"点位不存在", strptr(storeID), &fakeStoreResolver{found: false}, ErrStoreNotFound, 1, true},
		{"点位已停用", strptr(storeID), &fakeStoreResolver{status: "disabled", found: true}, ErrStoreDisabled, 1, true},
		{"商户服务问不到", strptr(storeID),
			&fakeStoreResolver{err: errors.New("connection refused")}, ErrStoreUnavailable, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewAdminService(nil, tc.resolver)
			err := svc.checkStore(t.Context(), tc.storeID)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("checkStore = %v, want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("checkStore = %v, want %v", err, tc.wantErr)
			}
			if len(tc.resolver.asked) != tc.wantAsks {
				t.Fatalf("resolver asked %v, want %d call(s)", tc.resolver.asked, tc.wantAsks)
			}
			// 被问到的那次问的必须是这个点位，不能是空串或别的 id。
			for _, asked := range tc.resolver.asked {
				if asked != storeID {
					t.Fatalf("resolver asked for %q, want %q", asked, storeID)
				}
			}
			// 这张表同时也是「哪几条该回 400」的声明：ErrStoreUnavailable 故意不在
			// ValidationErrors 里，控制器为它回 503。把这条预期写死在这里，改登记表时
			// 会在这里被拦下。
			if got := IsValidationError(err); got != tc.wantIsInput {
				t.Fatalf("IsValidationError(%v) = %v, want %v", err, got, tc.wantIsInput)
			}
		})
	}
}

// TestCheckStoreWithoutResolverFailsClosed 盯的是装配漏了的情况：点位校验器没接上，
// 而调用方带了 storeId。这时必须拒绝写入，不能当作「没有校验器就跳过校验」——那样
// 装配错误会静默降级成「点位不校验」，只在生产上才看得出来。
func TestCheckStoreWithoutResolverFailsClosed(t *testing.T) {
	svc := NewAdminService(nil, nil)
	err := svc.checkStore(t.Context(), strptr("00000000-0000-0000-0000-0000000000ff"))
	if err == nil {
		t.Fatal("checkStore with no resolver = nil, want an error")
	}
	if IsValidationError(err) {
		t.Fatalf("err = %v 被当成输入问题，但这其实是装配错误，该回 500", err)
	}
}

// TestUpdateDeviceSkipsStoreCheckWhenStoreUnchanged 盯的是这条规则本身：设备还挂在原来
// 那个点位上、这次保存没动它，就不该被点位的状态拦住。
//
// 场景是真实会发生的：门店把点位停用了，设备还在那儿。这时运营想改个提货码，旧行为会
// 回「点位已停用，不能挂载设备」——把人指到一个他根本没碰过的字段上，而且这台设备从此
// 再也改不动，除非先摘掉点位。校验该拦的是「把设备挂到一个停用的点位上」，不是
// 「设备一直挂在停用点位上还被编辑」。
func TestUpdateDeviceSkipsStoreCheckWhenStoreUnchanged(t *testing.T) {
	const deviceID = "00000000-0000-0000-0000-000000000001"
	const storeID = "00000000-0000-0000-0000-0000000000ff"

	// 点位是停用的，而设备本来就挂在它上面：表单原样提交，没有换点位。
	repo := &fakeAdminRepo{current: strptr(storeID)}
	resolver := &fakeStoreResolver{status: "disabled", found: true}
	svc := NewAdminService(repo, resolver)

	_, err := svc.UpdateDevice(t.Context(), deviceID, dto.DeviceInput{
		SerialUnique: "S1", ManufacturerID: "00000000-0000-0000-0000-000000000001",
		QrcodeType: "miniprogram", StoreID: strptr(storeID)})
	if err != nil {
		t.Fatalf("没换点位却被拦下：%v", err)
	}
	if len(resolver.asked) != 0 {
		t.Fatalf("没换点位还去问了商户服务：%v", resolver.asked)
	}
	if repo.updateCalls != 1 {
		t.Fatalf("落库被调了 %d 次，want 1", repo.updateCalls)
	}
}

// TestBuildDeviceClearsPaymentMethodAwayFromRegular 验的是切回 miniprogram 时清掉
// 支付方式：留着旧值不会立刻出错，但下一次有人切到 regular 就会带着一个早就没人
// 记得的值通过校验。
func TestBuildDeviceClearsPaymentMethodAwayFromRegular(t *testing.T) {
	svc := NewAdminService(nil, nil)
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
	svc := NewAdminService(nil, nil)
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
