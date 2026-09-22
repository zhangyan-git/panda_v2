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

	ErrDrinkNameRequired = errors.New("饮品名称不能为空")
	// ErrDrinkDeviceRequired 是新建饮品时的必填：饮品行就是「某台设备上的一杯」，
	// 没有设备的行卖不出去，只会在列表里显示成「未分配设备」。编辑时允许传空——
	// 库里那几行遗留的无设备饮品得有个地方能挂上去，但**不能**从界面上造出新的。
	ErrDrinkDeviceRequired = errors.New("饮品必须挂到一台设备上")
	// ErrDrinkDeviceInvalid 挡的是格式错的 deviceId。device_id 是 uuid 列，一个
	// "abc" 会让 PostgreSQL 在参数解析阶段报 22P02 并以 500 收场，而这明摆着是填错了。
	ErrDrinkDeviceInvalid = errors.New("deviceId 必须是合法的设备 ID")
	ErrDrinkTypeInvalid   = errors.New("drinkType 只能为 milk_coffee、black_coffee 或 other")
	ErrPriceNegative      = errors.New("价格不能为负数")
	// ErrDrinkPriceInvalid 对应数据库那条 CHECK (price > 0 OR (vip_price = 0 AND
	// pickup_code_price = 0))：原价为 0 时只允许整款饮品三级价全为 0（免费的），
	// 不允许「原价 0、会员价 5 元」这种只填了一半的行。
	ErrDrinkPriceInvalid = errors.New("原价为 0 时，会员价与提货码价也必须为 0")

	// ErrDrinkLookupDeviceRequired / ErrDrinkLookupCodeRequired 是设备回调取饮品
	// （gRPC GetDeviceDrink）的两个查询参数：机器只报设备 uuid 与饮品编号，缺一个都定位
	// 不到那一杯。和上面那批写路径的校验一样，空串在进 SQL 之前挡掉——不然它会变成一次
	// 「查不到」，而调用方会把「没给编号」读成「这杯不在库里」，那时钱已经收过了。
	ErrDrinkLookupDeviceRequired = errors.New("device_id 不能为空")
	ErrDrinkLookupCodeRequired   = errors.New("drink_code 不能为空")
	// ErrDrinkIDRequired 是按主键取饮品（gRPC GetDrink）的参数校验。与上面两个同一条
	// 理由，但后果更贵：这一条的调用方是 order-service 下单，把「没给 id」读成「这杯不在
	// 目录里」，用户看到的是一句「该饮品已下架」——一次填漏的参数被报成一次业务拒绝。
	ErrDrinkIDRequired = errors.New("drink_id 不能为空")

	ErrManufacturerStatusInvalid = errors.New("厂商状态只能为 active 或 disabled")
	ErrDeviceStatusInvalid       = errors.New("设备状态只能为 active 或 disabled")
	ErrDrinkStatusInvalid        = errors.New("饮品状态只能为 on_shelf 或 off_shelf")

	ErrBalanceAmountZero        = errors.New("调整金额不能为 0")
	ErrBalanceRequestIDRequired = errors.New("requestId 不能为空")

	// ErrStoreNotFound 表示设备要挂的点位在商户服务里不存在。以前这个 id 是裸值引用，
	// 填错了照样能存：设备会一直挂在一个不存在的地方，直到有人拿它去授权或派单才发现。
	ErrStoreNotFound = errors.New("点位不存在")
	// ErrStoreDisabled 表示点位存在但已停用。停用的点位不该再往上挂设备，理由和停用
	// 的设备不该接单一样。
	ErrStoreDisabled = errors.New("点位已停用，不能挂载设备")
	// ErrStoreUnavailable 表示这次没能确认点位状态（商户服务不可用、超时或应答看不懂）。
	// 它不是输入问题：确认不了就不写，也绝不退回「不校验」——校验只在问得到的时候才
	// 生效，等于没有校验。控制器把它映射成 503 而不是 400。
	ErrStoreUnavailable = errors.New("门店服务暂时不可用，无法确认点位")
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
	ErrDrinkNameRequired,
	ErrDrinkDeviceRequired, ErrDrinkDeviceInvalid, ErrDrinkTypeInvalid,
	ErrPriceNegative, ErrDrinkPriceInvalid,
	ErrDrinkLookupDeviceRequired, ErrDrinkLookupCodeRequired,
	ErrManufacturerStatusInvalid, ErrDeviceStatusInvalid, ErrDrinkStatusInvalid,
	ErrBalanceAmountZero, ErrBalanceRequestIDRequired,
	ErrStoreNotFound, ErrStoreDisabled,
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

// StoreResolver 回答「这个点位现在能不能挂设备」。
//
// 端口定义在消费方：跨服务调用是 gRPC 还是别的，是 main 装配时才定的事，业务规则
// 不该知道。返回 found 而不是靠一个哨兵错误表示「不存在」，是因为这个包的实现方
// （internal/client）不能反过来 import 本包来共享哨兵。
type StoreResolver interface {
	Resolve(ctx context.Context, storeID string) (status string, found bool, err error)
}

// AdminService 提供设备域主数据的后台写操作。
//
// 校验只做到「能不能落库、落库之后语义对不对」，不做「该不该这么做」的审批判断：
// 方案里设备域没有品牌/门店那种审核流程，后台改完即生效，改动留痕走审计。
type AdminService struct {
	admin  repository.AdminRepository
	stores StoreResolver
}

func NewAdminService(admin repository.AdminRepository, stores StoreResolver) *AdminService {
	return &AdminService{admin: admin, stores: stores}
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
	if err := s.checkStore(ctx, device.StoreID); err != nil {
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
	// 编辑与新建的差别就在这一句：新建一定要校验，编辑只在换了点位时校验。
	if err := s.checkStoreIfChanged(ctx, id, device.StoreID); err != nil {
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

// checkStore 确认要挂的点位真实存在且没被停用。
//
// store_id 是跨库的值引用，本库没有外键能拦它，所以这个判断只能在这里做——而且只在
// **写的时候**做一次：真到用的时候（派单、授权）再问一次是另一件事，那时设备已经
// 在某个点位上，问出来的结果是「它现在能不能用」，不是「当初挂得对不对」。
//
// 摘掉点位（nil）不需要校验，它没有指向任何东西。新建走这里，没有条件；编辑要先问
// 「这次动没动点位」，见 checkStoreIfChanged——那才是「挂在停用点位上的设备还能不能
// 改别的字段」这条规则的所在地。
func (s *AdminService) checkStore(ctx context.Context, storeID *string) error {
	if storeID == nil {
		return nil
	}
	// 形状先于存在性。商户库那一列是 uuid，把一个不是 uuid 的字符串递过去，Postgres
	// 抛的是 22P02（语法错），不是「查无此行」；商户服务只把 pgx.ErrNoRows 映射成
	// NotFound，于是它变成 Internal，到这儿就成了 503「门店服务暂时不可用」。填错
	// 一个 id 却被告知对方服务挂了，重试永远不会好——而这本来就是个 400。
	//
	// 放在问商户服务之前，也是让它在 s.stores 缺席时照样能给对答案：能不能认出这是个
	// id，不需要问任何人。
	if _, err := uuid.Parse(*storeID); err != nil {
		return ErrStoreNotFound
	}
	if s.stores == nil {
		return errors.New("admin service: store resolver is not configured")
	}
	status, found, err := s.stores.Resolve(ctx, *storeID)
	if err != nil {
		return errors.Join(ErrStoreUnavailable, err)
	}
	if !found {
		return ErrStoreNotFound
	}
	if status != "active" {
		return ErrStoreDisabled
	}
	return nil
}

// checkStoreIfChanged 只在这次真的换了点位时才校验。
//
// 设备本来就挂在 X、这次保存也没改这一栏，就不该再问一遍 X 还能不能用：点位停用是
// 门店的事，设备该不该跟着下线由人去决定，不该让一次「只想改提货码」的保存被另一个
// 模块的状态拦住——而且报错会写着「点位已停用，不能挂载设备」，把操作人指到一个他
// 根本没碰过的字段上。真正要拦的是「把设备挂到一个不存在或已停用的点位上」这个动作。
//
// 判据必须是库里那一行，不能是入参：入参里只有「这次要写成什么」，没有「原来是什么」，
// 只看入参的话「没动」和「换成一个同样停用的点位」长得一模一样。
//
// 多出来的这次读不会让「没校验过的值」落库：跳过校验的那条路上，写回去的就是刚读上
// 来的那个值，而它当初挂上去时是校验过的（新建和换点位都校验），归纳成立。
func (s *AdminService) checkStoreIfChanged(ctx context.Context, deviceID string, storeID *string) error {
	if storeID == nil {
		// 摘掉点位没有指向任何东西，不必校验，这次读也省了。
		return nil
	}
	current, err := s.admin.DeviceStoreID(ctx, deviceID)
	if err != nil {
		// 读不到当前值就无从比较。设备不存在会在这里变成 repository.ErrDeviceNotFound
		// 交给上层回 404——比先报「点位不对」再发现设备根本没有更顺。
		return err
	}
	if current != nil && *current == *storeID {
		return nil
	}
	return s.checkStore(ctx, storeID)
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
	deviceID, err := normalizeDrinkDeviceID(in.DeviceID, true)
	if err != nil {
		return nil, err
	}
	drinkType, err := normalizeDrinkType(in.DrinkType)
	if err != nil {
		return nil, err
	}
	if err := checkPrices(in.Price, in.VipPrice, in.PickupCodePrice); err != nil {
		return nil, err
	}
	// 厂商取自设备，不由调用方给：厂商挂在设备上，饮品跟着设备走。两边各填一次的话，
	// 迟早会有一条饮品的厂商和它那台设备对不上，而那种行既卖不出去也查不出来。
	//
	// 放在上面几条之后：这是本次唯一的库读，入参本身就错的请求不该先付出一次查库，
	// 也不该让「设备不存在」盖掉「饮品类型非法」——要改的是哪个字段，报错就该说哪个。
	manufacturerID, err := s.admin.DeviceManufacturerID(ctx, *deviceID)
	if err != nil {
		return nil, err
	}
	d := &model.Drink{
		ID:              uuid.NewString(),
		DeviceID:        deviceID,
		ManufacturerID:  manufacturerID,
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

// UpdateDrink 改饮品这一行的可编辑列。入参里没有 manufacturerId / originId，理由见
// repository.UpdateDrink：那是厂商同步的自然键，改了会让这一行再也匹配不上。
//
// 设备详情那一屏改价、改排序走的也是这个接口——饮品行自带 device_id，没有另一条
// 「设备上的饮品」写路径了。deviceId 是整行覆盖语义：不传就是「不挂设备」。
func (s *AdminService) UpdateDrink(ctx context.Context, id string, in dto.DrinkUpdateInput) (*model.Drink, error) {
	name := strings.TrimSpace(in.ProductName)
	if name == "" {
		return nil, ErrDrinkNameRequired
	}
	deviceID, err := normalizeDrinkDeviceID(in.DeviceID, false)
	if err != nil {
		return nil, err
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
		DeviceID:        deviceID,
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

// normalizeDrinkDeviceID 归一化饮品行上的设备 ID：去空白、空串当没填、非空必须是
// 合法 UUID，并回规范写法（大小写、无花括号那几种写法都收敛成同一个值）。
//
// required 为真时「没填」是错误（新建），为假时表示「摘下来」（编辑整行覆盖）。
// 归一之后交给事务里的外键去判设备在不在：那一条能顺手把并发删除挡在同一个事务里，
// 而在这里先查一次会多一趟往返，还挡不住查完与写入之间的那一瞬。
func normalizeDrinkDeviceID(raw *string, required bool) (*string, error) {
	if raw == nil {
		if required {
			return nil, ErrDrinkDeviceRequired
		}
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		if required {
			return nil, ErrDrinkDeviceRequired
		}
		return nil, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return nil, ErrDrinkDeviceInvalid
	}
	normalized := parsed.String()
	return &normalized, nil
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
