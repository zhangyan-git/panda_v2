package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// 校验失败的错误。这些条都在数据库里有对应的 CHECK 或触发器，服务层再判一遍不是
// 重复：让约束去拦的意思是调用方收到一个 500 加一句 PostgreSQL 的英文报错，而这些都是
// 可预期的输入问题，该回 400 并把话说清楚。
var (
	ErrManufacturerCodeRequired = errors.New("厂商编码不能为空")
	ErrManufacturerNameRequired = errors.New("厂商名称不能为空")

	ErrDeviceSerialRequired       = errors.New("设备序列号不能为空")
	ErrDeviceManufacturerRequired = errors.New("设备必须归属一个厂商")
	ErrQrcodeTypeInvalid          = errors.New("qrcodeType 只能为 miniprogram 或 regular")
	ErrRegularQrcodePaymentNeeded = errors.New("二维码类型为 regular 时必须指定支付方式")
	ErrRegularQrcodePaymentBad    = errors.New("支付方式只能为 fengxuan_wanlian 或 youlian")

	ErrDrinkNameRequired         = errors.New("饮品名称不能为空")
	ErrDrinkManufacturerRequired = errors.New("饮品必须归属一个厂商")
	ErrDrinkTypeInvalid          = errors.New("drinkType 只能为 milk_coffee、black_coffee 或 other")
	ErrPriceNegative             = errors.New("价格不能为负数")
	// ErrDrinkPriceInvalid 对应数据库那条 CHECK (price > 0 OR (vip_price = 0 AND
	// pickup_code_price = 0))：原价为 0 时只允许整款饮品三级价全为 0（免费的），
	// 不允许「原价 0、会员价 5 元」这种只填了一半的行。
	ErrDrinkPriceInvalid = errors.New("原价为 0 时，会员价与提货码价也必须为 0")

	ErrManufacturerStatusInvalid = errors.New("厂商状态只能为 active 或 disabled")
	ErrDeviceStatusInvalid       = errors.New("设备状态只能为 active 或 disabled")
	ErrDrinkStatusInvalid        = errors.New("饮品状态只能为 on_shelf 或 off_shelf")

	ErrBalanceAmountZero        = errors.New("调整金额不能为 0")
	ErrBalanceRequestIDRequired = errors.New("requestId 不能为空")
)

// ValidationErrors 收齐所有「输入不合法」的哨兵错误。
//
// 控制器靠它一把判断该回 400 还是 500：默认回 500 才是安全的方向——把基础设施故障
// 误报成 400 会让调用方以为改一下参数就行，而反过来只是错误码难看。所以新增一条校验
// 时，把错误加进这个切片，不要去控制器里加 case。
var ValidationErrors = []error{
	ErrManufacturerCodeRequired, ErrManufacturerNameRequired,
	ErrDeviceSerialRequired, ErrDeviceManufacturerRequired,
	ErrQrcodeTypeInvalid, ErrRegularQrcodePaymentNeeded, ErrRegularQrcodePaymentBad,
	ErrDrinkNameRequired, ErrDrinkManufacturerRequired, ErrDrinkTypeInvalid,
	ErrPriceNegative, ErrDrinkPriceInvalid,
	ErrManufacturerStatusInvalid, ErrDeviceStatusInvalid, ErrDrinkStatusInvalid,
	ErrBalanceAmountZero, ErrBalanceRequestIDRequired,
}

// IsValidationError 表示这个错误是「输入不合法」，该回 400。
//
// 控制器用它，而不是自己写一串 case：新增校验时只需要把哨兵错误加进
// ValidationErrors，HTTP 层不用动。注意它只回答「是不是 400」，不认识的错误一律
// 返回 false——调用方得把 false 当 500 处理，那才是安全的方向。
func IsValidationError(err error) bool {
	for _, validationErr := range ValidationErrors {
		if errors.Is(err, validationErr) {
			return true
		}
	}
	return false
}

var (
	validQrcodeTypes = map[string]bool{"miniprogram": true, "regular": true}
	// regular_qrcode_payment_method 的取值来自数据库 CHECK，两边必须一致。
	validRegularQrcodePayments = map[string]bool{"fengxuan_wanlian": true, "youlian": true}
	validDrinkTypes            = map[string]bool{"milk_coffee": true, "black_coffee": true, "other": true}
)

// AdminService 提供设备域主数据的后台写操作。
//
// 校验只做到「能不能落库、落库之后语义对不对」，不做「该不该这么做」的审批判断：
// 方案里设备域没有品牌/门店那种审核流程，后台改完即生效，改动留痕走审计。
type AdminService struct {
	admin repository.AdminRepository
}

func NewAdminService(admin repository.AdminRepository) *AdminService {
	return &AdminService{admin: admin}
}

// ============================================================
// 厂商
// ============================================================

func (s *AdminService) CreateManufacturer(ctx context.Context, in dto.ManufacturerInput) (*model.Manufacturer, error) {
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	if in.Code == "" {
		return nil, ErrManufacturerCodeRequired
	}
	if in.Name == "" {
		return nil, ErrManufacturerNameRequired
	}
	// 只填要写进去的列。CreatedAt / UpdatedAt 由数据库给，这里的结构体活不过
	// 这一行——落库之后是从库里读回来的那一行回给调用方。
	m := &model.Manufacturer{
		ID:           uuid.NewString(),
		Code:         in.Code,
		Name:         in.Name,
		ContactName:  strings.TrimSpace(in.ContactName),
		ContactPhone: strings.TrimSpace(in.ContactPhone),
	}
	return s.admin.CreateManufacturer(ctx, m)
}

// UpdateManufacturer 改展示字段。入参里没有 code，所以「编码不可改」这件事在类型上
// 就成立了，不需要再比对一次。
func (s *AdminService) UpdateManufacturer(ctx context.Context, id string, in dto.ManufacturerInput) (*model.Manufacturer, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, ErrManufacturerNameRequired
	}
	m := &model.Manufacturer{
		ID:           id,
		Name:         in.Name,
		ContactName:  strings.TrimSpace(in.ContactName),
		ContactPhone: strings.TrimSpace(in.ContactPhone),
	}
	if err := s.admin.UpdateManufacturer(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *AdminService) SetManufacturerStatus(ctx context.Context, id, status string) error {
	if status != "active" && status != "disabled" {
		return ErrManufacturerStatusInvalid
	}
	return s.admin.SetManufacturerStatus(ctx, id, status)
}

// ============================================================
// 设备
// ============================================================

func (s *AdminService) CreateDevice(ctx context.Context, in dto.DeviceInput) (*model.Device, error) {
	device, err := s.buildDevice(uuid.NewString(), in)
	if err != nil {
		return nil, err
	}
	// 创建时留空就是没有静态验证码（列是 NOT NULL DEFAULT ''，不是 NULL）；没填就
	// 保持结构体的零值。
	if password := trimPickupPassword(in.PickupPassword); password != nil {
		device.PickupPassword = *password
	}
	return s.admin.CreateDevice(ctx, device)
}

func (s *AdminService) UpdateDevice(ctx context.Context, id string, in dto.DeviceInput) (*model.Device, error) {
	device, err := s.buildDevice(id, in)
	if err != nil {
		return nil, err
	}
	device.UpdatedAt = time.Now()
	// 第三个入参为 nil 表示「这一栏留空＝不改」。不放进 device 里是因为它是第三态，
	// 见 repository.AdminRepository.UpdateDevice。
	if err := s.admin.UpdateDevice(ctx, device, trimPickupPassword(in.PickupPassword)); err != nil {
		return nil, err
	}
	return device, nil
}

// buildDevice 把入参过一遍校验并折成设备。创建与编辑共用：两边可编辑的字段集合是
// 同一个，分成两份会立刻漂移。
func (s *AdminService) buildDevice(id string, in dto.DeviceInput) (*model.Device, error) {
	in.SerialUnique = strings.TrimSpace(in.SerialUnique)
	if in.SerialUnique == "" {
		return nil, ErrDeviceSerialRequired
	}
	if strings.TrimSpace(in.ManufacturerID) == "" {
		return nil, ErrDeviceManufacturerRequired
	}
	qrcodeType := strings.TrimSpace(in.QrcodeType)
	if !validQrcodeTypes[qrcodeType] {
		return nil, ErrQrcodeTypeInvalid
	}
	// 切回 miniprogram 时把支付方式清掉：留着旧值不会立刻出错，但下一次有人把类型
	// 改成 regular 时，会带着一个早就没人记得的支付方式直接通过校验。
	var paymentMethod *string
	if qrcodeType == "regular" {
		value := ""
		if in.RegularQrcodePaymentMethod != nil {
			value = strings.TrimSpace(*in.RegularQrcodePaymentMethod)
		}
		if value == "" {
			return nil, ErrRegularQrcodePaymentNeeded
		}
		if !validRegularQrcodePayments[value] {
			return nil, ErrRegularQrcodePaymentBad
		}
		paymentMethod = &value
	}
	return &model.Device{
		ID:                         id,
		SerialUnique:               in.SerialUnique,
		DeviceName:                 strings.TrimSpace(in.DeviceName),
		ManufacturerID:             strings.TrimSpace(in.ManufacturerID),
		StoreID:                    normalizeOptional(in.StoreID),
		QrcodeType:                 qrcodeType,
		RegularQrcodePaymentMethod: paymentMethod,
		ShowVip:                    in.ShowVip,
		EnableCouponVerification:   in.EnableCouponVerification,
		WarrantyEndAt:              in.WarrantyEndAt,
	}, nil
}

func (s *AdminService) SetDeviceStatus(ctx context.Context, id, status string) error {
	if status != "active" && status != "disabled" {
		return ErrDeviceStatusInvalid
	}
	return s.admin.SetDeviceStatus(ctx, id, status)
}

// ============================================================
// 饮品
// ============================================================

func (s *AdminService) CreateDrink(ctx context.Context, in dto.DrinkInput) (*model.Drink, error) {
	name := strings.TrimSpace(in.ProductName)
	if name == "" {
		return nil, ErrDrinkNameRequired
	}
	if strings.TrimSpace(in.ManufacturerID) == "" {
		return nil, ErrDrinkManufacturerRequired
	}
	drinkType, err := normalizeDrinkType(in.DrinkType)
	if err != nil {
		return nil, err
	}
	if err := checkPrices(in.Price, in.VipPrice, in.PickupCodePrice); err != nil {
		return nil, err
	}
	d := &model.Drink{
		ID:              uuid.NewString(),
		ManufacturerID:  strings.TrimSpace(in.ManufacturerID),
		OriginID:        strings.TrimSpace(in.OriginID),
		ProductNum:      strings.TrimSpace(in.ProductNum),
		ProductName:     name,
		EnName:          strings.TrimSpace(in.EnName),
		DrinkType:       drinkType,
		ProductImg:      strings.TrimSpace(in.ProductImg),
		ProductDesc:     in.ProductDesc,
		Price:           in.Price,
		VipPrice:        in.VipPrice,
		PickupCodePrice: in.PickupCodePrice,
		Sort:            in.Sort,
	}
	return s.admin.CreateDrink(ctx, d)
}

// UpdateDrink 改目录字段。入参里没有 manufacturerId / originId，理由见
// repository.UpdateDrink：那是厂商同步的自然键，改了会让这一行再也匹配不上。
func (s *AdminService) UpdateDrink(ctx context.Context, id string, in dto.DrinkUpdateInput) (*model.Drink, error) {
	name := strings.TrimSpace(in.ProductName)
	if name == "" {
		return nil, ErrDrinkNameRequired
	}
	drinkType, err := normalizeDrinkType(in.DrinkType)
	if err != nil {
		return nil, err
	}
	if err := checkPrices(in.Price, in.VipPrice, in.PickupCodePrice); err != nil {
		return nil, err
	}
	d := &model.Drink{
		ID:              id,
		ProductNum:      strings.TrimSpace(in.ProductNum),
		ProductName:     name,
		EnName:          strings.TrimSpace(in.EnName),
		DrinkType:       drinkType,
		ProductImg:      strings.TrimSpace(in.ProductImg),
		ProductDesc:     in.ProductDesc,
		Price:           in.Price,
		VipPrice:        in.VipPrice,
		PickupCodePrice: in.PickupCodePrice,
		Sort:            in.Sort,
		UpdatedAt:       time.Now(),
	}
	if err := s.admin.UpdateDrink(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *AdminService) SetDrinkStatus(ctx context.Context, id, status string) error {
	if status != "on_shelf" && status != "off_shelf" {
		return ErrDrinkStatusInvalid
	}
	return s.admin.SetDrinkStatus(ctx, id, status)
}

// normalizeDrinkType 把「没填」和「填空串」都归一成 nil：drink_type 可空，而
// 空串不在 CHECK 的取值里，直接落库会被约束拦下。
func normalizeDrinkType(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return nil, nil
	}
	if !validDrinkTypes[value] {
		return nil, ErrDrinkTypeInvalid
	}
	return &value, nil
}

func checkPrices(price, vipPrice, pickupCodePrice int64) error {
	if price < 0 || vipPrice < 0 || pickupCodePrice < 0 {
		return ErrPriceNegative
	}
	if price == 0 && (vipPrice != 0 || pickupCodePrice != 0) {
		return ErrDrinkPriceInvalid
	}
	return nil
}

// ============================================================
// 余额
// ============================================================

// AdjustBalance 调整设备余额，返回调整后的余额。
//
// 这条是方案 11.6 L893 必审清单上的操作，审计由 repository 在同一个事务里写进
// 本库 message_outbox（见 platform/audit），这里只负责把输入挡成合法形状。
func (s *AdminService) AdjustBalance(ctx context.Context, deviceID string, in dto.BalanceAdjustInput, operator string) (int64, error) {
	if in.Amount == 0 {
		return 0, ErrBalanceAmountZero
	}
	in.RequestID = strings.TrimSpace(in.RequestID)
	if in.RequestID == "" {
		return 0, ErrBalanceRequestIDRequired
	}
	return s.admin.AdjustBalance(ctx, repository.BalanceAdjustment{
		DeviceID:  deviceID,
		Amount:    in.Amount,
		RequestID: in.RequestID,
		Remark:    strings.TrimSpace(in.Remark),
		Operator:  operator,
	})
}

// normalizeOptional 把指向可空列的可选字符串归一成 nil，「没填」和「填空串」在
// 这里没有区别。
func normalizeOptional(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// trimPickupPassword 归一化提货码：nil 与「全是空白」都算没填。
//
// 它是店员照着敲的一串码，前后带空格只会在现场变成一张工单，不可能是本意。
//
// 「没填」在两条路径上落成的东西不同，判断只做一次是为了不让两处各判一遍：创建时
// 它落成空串（列是 NOT NULL DEFAULT ”），编辑时它落成 nil——那表示那一列原样不动。
func trimPickupPassword(raw *string) *string {
	if raw == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
