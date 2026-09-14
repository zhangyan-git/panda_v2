package repository

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

// 写路径的可预期失败。分成三类是为了让 HTTP 层能给出正确的状态码：不存在是 404，
// 撞唯一键是 409，其余才是 500。这几条不翻出来，冲突就会以 500 的形式回给前端，
// 而前端拿 500 是没法提示「这个序列号已被占用」的。
var (
	// ErrManufacturerNotFound 表示被操作的厂商本身不存在（目标，404）。
	ErrManufacturerNotFound = errors.New("manufacturer not found")
	// ErrManufacturerMissing 表示请求里引用的厂商不存在——设备或饮品挂在了一个不
	// 存在的厂商上（字段，400）。与上一条分开：都是「厂商不存在」，但一个要回 404、
	// 一个要回 400，合成一个哨兵错误就没法在 HTTP 层区分了。
	ErrManufacturerMissing   = errors.New("referenced manufacturer does not exist")
	ErrDrinkNotFound         = errors.New("drink not found")
	ErrSerialTaken           = errors.New("device serial_unique already exists")
	ErrManufacturerCodeTaken = errors.New("manufacturer code already exists")
	ErrDrinkOriginTaken      = errors.New("drink origin_id already exists for this manufacturer")
	ErrDuplicateRequest      = errors.New("request_id already recorded")
	// ErrInsufficientBalance 表示这次调整会把余额扣成负数。
	ErrInsufficientBalance = errors.New("balance would go negative")
	// ErrBalanceOutOfRange 表示相加之后超出 BIGINT 能表示的范围。不挡的话会回绕成
	// 一个负数，最后以「余额为负」的 CHECK 违反收场，看起来像是管理员填少了。
	ErrBalanceOutOfRange = errors.New("balance exceeds the supported range")
)

// BalanceAdjustment 是一次后台余额调整。
//
// Amount 有符号：正数为增加，负数为减少，0 不允许（流水表有 amount <> 0 约束）。
// RequestID 是幂等键，必填——重试一次就得分辨出「这是同一次调整」，否则管理员的
// 一次点击在超时重发后会变成两次加钱。
type BalanceAdjustment struct {
	DeviceID  string
	Amount    int64
	RequestID string
	Remark    string
	// Operator 是操作人的用户 ID（来自令牌的 UserID）。
	Operator string
}

// AdminRepository 是设备域后台写路径。
//
// 每一个写方法都是一个事务：改业务行、必要时写流水、往本库 message_outbox 追加
// 一条 admin.operation.logged，最后一起提交。审计事件与它描述的那次写入必须同生共死
// （见 platform/audit 的包注释），所以下面没有「先提交再记日志」的写法。
// 三个 Create 回的是**库里读回来的那一行**，不是调用方传进来的结构体。差别在
// created_at / updated_at：那两列由数据库的默认值和触发器给，服务层拼的那个值只是
// 本地时钟，与真实落库的时间差一截，而且 updated_at 会是零值（0001 年）。创建接口
// 回给前端的对象必须就是刚写进去的那一个，否则前端拿到的 createdAt 和列表里再查到
// 的对不上。
type AdminRepository interface {
	CreateManufacturer(ctx context.Context, m *model.Manufacturer) (*model.Manufacturer, error)
	UpdateManufacturer(ctx context.Context, m *model.Manufacturer) error
	SetManufacturerStatus(ctx context.Context, id, status string) error

	CreateDevice(ctx context.Context, d *model.Device) (*model.Device, error)
	// UpdateDevice 改设备可编辑的列。pickupPassword 是个单独入参而不是取 d 上的
	// 字段，因为它有第三种状态：nil 表示**不动那一列**。放进结构体里就会变成一个
	// 「传了但被忽略」的字段，那种坑不如让签名说清楚。
	UpdateDevice(ctx context.Context, d *model.Device, pickupPassword *string) error
	SetDeviceStatus(ctx context.Context, id, status string) error

	CreateDrink(ctx context.Context, d *model.Drink) (*model.Drink, error)
	UpdateDrink(ctx context.Context, d *model.Drink) error
	SetDrinkStatus(ctx context.Context, id, status string) error

	// AdjustBalance 调整设备余额并追加一条流水，返回调整后的余额。
	AdjustBalance(ctx context.Context, in BalanceAdjustment) (int64, error)
}

type pgAdminRepository struct {
	pool  *pgxpool.Pool
	audit audit.Recorder
}

// NewAdminRepository 创建后台写路径的数据访问实现。
func NewAdminRepository(pool *pgxpool.Pool, recorder audit.Recorder) AdminRepository {
	// 传 nil 等价于关掉审计，而不是「每次写都 panic」——调用方少传一个参数时，
	// 失败的应该是审计开关这么明显的东西，不是运行期的空指针。
	if recorder == nil {
		recorder = audit.Noop{}
	}
	return &pgAdminRepository{pool: pool, audit: recorder}
}

// mapPGError 把唯一/外键约束违反翻成本包的哨兵错误。
//
// 约束名是从实际数据库里查出来的（pg_constraint / pg_indexes），不是照默认命名
// 规则推的：设备序列号那个是 devices_serial_unique_key，饮品那个自然键是部分索引
// drinks_manufacturer_origin_unique，名字对不上就会静默落回 500。
func mapPGError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23505": // unique_violation
		switch pgErr.ConstraintName {
		case "devices_serial_unique_key":
			return ErrSerialTaken
		case "manufacturers_code_key":
			return ErrManufacturerCodeTaken
		case "drinks_manufacturer_origin_unique":
			return ErrDrinkOriginTaken
		case "device_balance_ledger_one_per_request":
			return ErrDuplicateRequest
		}
	case "23503": // foreign_key_violation
		switch pgErr.ConstraintName {
		case "devices_manufacturer_id_fkey", "drinks_manufacturer_id_fkey":
			// 引用不存在的厂商是请求字段的问题，不是「目标不存在」。
			return ErrManufacturerMissing
		}
	}
	return err
}

// ============================================================
// 审计快照
//
// 这些都是专门为审计写的结构，带 json tag、不带密钥。直接把 model 丢进
// audit.Snapshot 会序列化出一堆没有 tag 的大写字段名，而厂商那类对象还会把
// 接入凭据一并带上。
// ============================================================

// manufacturerSnapshot 是厂商在审计里的样子。刻意不含 manufacturer_credentials：
// 密钥、令牌不进审计载荷。
type manufacturerSnapshot struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	ContactName  string `json:"contact_name"`
	ContactPhone string `json:"contact_phone"`
	Status       string `json:"status"`
}

// deviceSnapshot 是设备在审计里的样子。
//
// PickupPassword 带 json:"-"，只用于前后对比，不进审计载荷：那是店员打咖啡用的
// 静态验证码，审计要回答的是「改没改」（PickupPasswordSet 就是给这个看的），而
// 值抄进审计表等于让一份凭据多存一处、多一套保留期，当前值在设备详情页随时能看。
// 厂商同步写入的那几列同理不进快照：它们不由后台改动，混进来只会让「这次人工改了
// 什么」变得难读。
type deviceSnapshot struct {
	SerialUnique               string  `json:"serial_unique"`
	DeviceName                 string  `json:"device_name"`
	ManufacturerID             string  `json:"manufacturer_id"`
	StoreID                    *string `json:"store_id"`
	QrcodeType                 string  `json:"qrcode_type"`
	RegularQrcodePaymentMethod *string `json:"regular_qrcode_payment_method"`
	ShowVip                    bool    `json:"show_vip"`
	EnableCouponVerification   bool    `json:"enable_coupon_verification"`
	WarrantyEndAt              *string `json:"warranty_end_at"`
	Status                     string  `json:"status"`

	PickupPassword    string `json:"-"`
	PickupPasswordSet bool   `json:"pickup_password_set"`
}

// drinkSnapshot 是饮品在审计里的样子。价格是分。
type drinkSnapshot struct {
	ManufacturerID  string  `json:"manufacturer_id"`
	OriginID        string  `json:"origin_id"`
	ProductNum      string  `json:"product_num"`
	ProductName     string  `json:"product_name"`
	EnName          string  `json:"en_name"`
	DrinkType       *string `json:"drink_type"`
	ProductImg      string  `json:"product_img"`
	ProductDesc     string  `json:"product_desc"`
	Price           int64   `json:"price"`
	VipPrice        int64   `json:"vip_price"`
	PickupCodePrice int64   `json:"pickup_code_price"`
	Status          string  `json:"status"`
	Sort            int     `json:"sort"`
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	formatted := t.Format(time.RFC3339)
	return &formatted
}

// ============================================================
// 厂商
// ============================================================

// selectManufacturerSnapshot 读厂商当前值并锁住那一行。写路径统一先取 before，
// 拿了锁再改，所以 before 一定就是这次改动的直接前驱。
func selectManufacturerSnapshot(ctx context.Context, tx pgx.Tx, id string) (manufacturerSnapshot, error) {
	var s manufacturerSnapshot
	err := tx.QueryRow(ctx, `SELECT code, name, contact_name, contact_phone, status
		FROM manufacturers WHERE id = $1 FOR UPDATE`, id).
		Scan(&s.Code, &s.Name, &s.ContactName, &s.ContactPhone, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrManufacturerNotFound
	}
	return s, err
}

func (r *pgAdminRepository) CreateManufacturer(ctx context.Context, m *model.Manufacturer) (*model.Manufacturer, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 不写 status，用列默认的 active：新建即启用，停用走 /status 那条路。
	if _, err := tx.Exec(ctx, `INSERT INTO manufacturers (id, code, name, contact_name, contact_phone)
		VALUES ($1, $2, $3, $4, $5)`, m.ID, m.Code, m.Name, m.ContactName, m.ContactPhone); err != nil {
		return nil, mapPGError(err)
	}
	after, err := selectManufacturerSnapshot(ctx, tx, m.ID)
	if err != nil {
		return nil, err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "manufacturers", Action: "create", Operation: "新增厂商",
		TargetType: "manufacturer", TargetID: m.ID, TargetName: after.Name,
		After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}
	created, err := scanManufacturer(tx.QueryRow(ctx, `SELECT `+manufacturerColumns+` FROM manufacturers WHERE id = $1`, m.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateManufacturer 改展示字段。code 不在 SET 里：它是稳定业务标识，数据库有
// 触发器拦，这里也不给出改的入口——改 code 会让旧系统按 code 关联的设备与出杯
// 分支全部指空。
func (r *pgAdminRepository) UpdateManufacturer(ctx context.Context, m *model.Manufacturer) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectManufacturerSnapshot(ctx, tx, m.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE manufacturers
		SET name = $2, contact_name = $3, contact_phone = $4, updated_at = NOW()
		WHERE id = $1`, m.ID, m.Name, m.ContactName, m.ContactPhone); err != nil {
		return mapPGError(err)
	}
	after, err := selectManufacturerSnapshot(ctx, tx, m.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "manufacturers", Action: "update", Operation: "修改厂商",
		TargetType: "manufacturer", TargetID: m.ID, TargetName: after.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminRepository) SetManufacturerStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectManufacturerSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE manufacturers SET status = $2, updated_at = NOW() WHERE id = $1`, id, status); err != nil {
		return mapPGError(err)
	}
	after, err := selectManufacturerSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "manufacturers", Action: "update_status", Operation: "修改厂商状态",
		TargetType: "manufacturer", TargetID: id, TargetName: after.Name,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ============================================================
// 设备
// ============================================================

func selectDeviceSnapshot(ctx context.Context, tx pgx.Tx, id string) (deviceSnapshot, error) {
	var s deviceSnapshot
	var pickupPassword string
	var warrantyEndAt *time.Time
	err := tx.QueryRow(ctx, `SELECT serial_unique, device_name, manufacturer_id::text,
		store_id::text, qrcode_type, regular_qrcode_payment_method, show_vip,
		enable_coupon_verification, warranty_end_at, pickup_password, status
		FROM devices WHERE id = $1 FOR UPDATE`, id).
		Scan(&s.SerialUnique, &s.DeviceName, &s.ManufacturerID, &s.StoreID, &s.QrcodeType,
			&s.RegularQrcodePaymentMethod, &s.ShowVip, &s.EnableCouponVerification,
			&warrantyEndAt, &pickupPassword, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrDeviceNotFound
	}
	if err != nil {
		return s, err
	}
	s.WarrantyEndAt = formatTimePtr(warrantyEndAt)
	s.PickupPassword = pickupPassword
	s.PickupPasswordSet = pickupPassword != ""
	return s, nil
}

func (r *pgAdminRepository) CreateDevice(ctx context.Context, d *model.Device) (*model.Device, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// status 用列默认值（active），上下架走 /status；coffee_balance 用默认 0，
	// 改钱只能走 AdjustBalance，那里才有流水。
	if _, err := tx.Exec(ctx, `INSERT INTO devices (id, serial_unique, device_name,
		manufacturer_id, store_id, qrcode_type, regular_qrcode_payment_method,
		pickup_password, show_vip, enable_coupon_verification, warranty_end_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		d.ID, d.SerialUnique, d.DeviceName, d.ManufacturerID, d.StoreID, d.QrcodeType,
		d.RegularQrcodePaymentMethod, d.PickupPassword, d.ShowVip,
		d.EnableCouponVerification, d.WarrantyEndAt); err != nil {
		return nil, mapPGError(err)
	}
	after, err := selectDeviceSnapshot(ctx, tx, d.ID)
	if err != nil {
		return nil, err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "devices", Action: "create", Operation: "新增设备",
		TargetType: "device", TargetID: d.ID, TargetName: after.DeviceName,
		After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}
	created, err := scanDevice(tx.QueryRow(ctx, `SELECT `+deviceColumns+` FROM devices WHERE id = $1`, d.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateDevice 改后台可编辑的那几列。状态、余额、厂商同步写入的列都不在 SET 里，
// 理由见 model.Device 的分类注释。
//
// pickupPassword 为 nil 时那一列原样不动（COALESCE 保住旧值）。调用方传 nil 的
// 场景是编辑表单留空——静态验证码不回填，不能因为「这栏是空的」就把它抹掉。
func (r *pgAdminRepository) UpdateDevice(ctx context.Context, d *model.Device, pickupPassword *string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectDeviceSnapshot(ctx, tx, d.ID)
	if err != nil {
		return err
	}
	// RegularQrcodePaymentMethod 必须跟着 QrcodeType 一起写：两者由一条 CHECK
	// （qrcode_type <> 'regular' OR regular_qrcode_payment_method IS NOT NULL）绑定，
	// 分开更新会在中间态撞上它。切回 miniprogram 时把它清空，留着旧值会让下一次
	// 切到 regular 时带着一个用户早就忘了的支付方式。
	var paymentMethod *string
	if d.QrcodeType == "regular" {
		paymentMethod = d.RegularQrcodePaymentMethod
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET serial_unique = $2, device_name = $3,
		manufacturer_id = $4, store_id = $5, qrcode_type = $6,
		regular_qrcode_payment_method = $7,
		pickup_password = COALESCE($8, pickup_password), show_vip = $9,
		enable_coupon_verification = $10, warranty_end_at = $11, updated_at = NOW()
		WHERE id = $1`,
		d.ID, d.SerialUnique, d.DeviceName, d.ManufacturerID, d.StoreID, d.QrcodeType,
		paymentMethod, pickupPassword, d.ShowVip, d.EnableCouponVerification,
		d.WarrantyEndAt); err != nil {
		return mapPGError(err)
	}
	after, err := selectDeviceSnapshot(ctx, tx, d.ID)
	if err != nil {
		return err
	}
	// PickupPassword 是 json:"-"，不会被序列化出去，但快照读得到，所以「改没改」
	// 只能在这里比。判据是**改完之后的库里状态**，而不是「这次传了没有」：编辑时
	// 留空表示不改（COALESCE 保住旧值），只看入参的话，一次改名会被记成动过验证码，
	// 而真换了码、恰好又落了别的字段变更时反而看不出来。比库里的值，两边都不会错。
	after.PickupPasswordSet = after.PickupPassword != ""
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "devices", Action: "update", Operation: "修改设备",
		TargetType: "device", TargetID: d.ID, TargetName: after.DeviceName,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminRepository) SetDeviceStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectDeviceSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET status = $2, updated_at = NOW() WHERE id = $1`, id, status); err != nil {
		return mapPGError(err)
	}
	after, err := selectDeviceSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "devices", Action: "update_status", Operation: "修改设备状态",
		TargetType: "device", TargetID: id, TargetName: after.DeviceName,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ============================================================
// 饮品
// ============================================================

func selectDrinkSnapshot(ctx context.Context, tx pgx.Tx, id string) (drinkSnapshot, error) {
	var s drinkSnapshot
	err := tx.QueryRow(ctx, `SELECT manufacturer_id::text, origin_id, product_num,
		product_name, en_name, drink_type, product_img, product_desc, price, vip_price,
		pickup_code_price, status, sort FROM drinks WHERE id = $1 FOR UPDATE`, id).
		Scan(&s.ManufacturerID, &s.OriginID, &s.ProductNum, &s.ProductName, &s.EnName,
			&s.DrinkType, &s.ProductImg, &s.ProductDesc, &s.Price, &s.VipPrice,
			&s.PickupCodePrice, &s.Status, &s.Sort)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrDrinkNotFound
	}
	return s, err
}

func (r *pgAdminRepository) CreateDrink(ctx context.Context, d *model.Drink) (*model.Drink, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 与设备同理：status 走列默认值 on_shelf，上下架有专门的入口。
	if _, err := tx.Exec(ctx, `INSERT INTO drinks (id, manufacturer_id, origin_id,
		product_num, product_name, en_name, drink_type, product_img, product_desc,
		price, vip_price, pickup_code_price, sort)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		d.ID, d.ManufacturerID, d.OriginID, d.ProductNum, d.ProductName, d.EnName,
		d.DrinkType, d.ProductImg, d.ProductDesc, d.Price, d.VipPrice,
		d.PickupCodePrice, d.Sort); err != nil {
		return nil, mapPGError(err)
	}
	after, err := selectDrinkSnapshot(ctx, tx, d.ID)
	if err != nil {
		return nil, err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "drinks", Action: "create", Operation: "新增饮品",
		TargetType: "drink", TargetID: d.ID, TargetName: after.ProductName,
		After: audit.Snapshot(after),
	}); err != nil {
		return nil, err
	}
	created, err := scanDrink(tx.QueryRow(ctx, `SELECT `+drinkColumns+` FROM drinks WHERE id = $1`, d.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateDrink 改饮品目录字段。
//
// manufacturer_id 与 origin_id 不在 SET 里：这两个是厂商同步的自然键
// （drinks_manufacturer_origin_unique），后台改掉它们，这一行就再也匹配不上厂商
// 侧的同一款饮品，下一轮同步会新插一行，旧的留在库里变成谁都说不清来历的孤儿。
func (r *pgAdminRepository) UpdateDrink(ctx context.Context, d *model.Drink) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectDrinkSnapshot(ctx, tx, d.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE drinks SET product_num = $2, product_name = $3,
		en_name = $4, drink_type = $5, product_img = $6, product_desc = $7,
		price = $8, vip_price = $9, pickup_code_price = $10, sort = $11,
		updated_at = NOW() WHERE id = $1`,
		d.ID, d.ProductNum, d.ProductName, d.EnName, d.DrinkType, d.ProductImg,
		d.ProductDesc, d.Price, d.VipPrice, d.PickupCodePrice, d.Sort); err != nil {
		return mapPGError(err)
	}
	after, err := selectDrinkSnapshot(ctx, tx, d.ID)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "drinks", Action: "update", Operation: "修改饮品",
		TargetType: "drink", TargetID: d.ID, TargetName: after.ProductName,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *pgAdminRepository) SetDrinkStatus(ctx context.Context, id, status string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	before, err := selectDrinkSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE drinks SET status = $2, updated_at = NOW() WHERE id = $1`, id, status); err != nil {
		return mapPGError(err)
	}
	after, err := selectDrinkSnapshot(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := r.audit.Record(ctx, tx, audit.Entry{
		Module: "drinks", Action: "update_status", Operation: "修改饮品上下架",
		TargetType: "drink", TargetID: id, TargetName: after.ProductName,
		Before: audit.Snapshot(before), After: audit.Snapshot(after),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ============================================================
// 余额调整
// ============================================================

// balanceSnapshot 是余额在审计里的样子。
//
// 这里就是方案 11.6 L893 那张必审清单要的 before-image：改之前是多少、这次动了多少、
// 改完是多少，三样一起落进 admin_operation_logs。没有它，一次改错的余额在库里只留下
// 「现在是 X」，无从回溯是谁把它从哪改过来的。
type balanceSnapshot struct {
	CoffeeBalance int64  `json:"coffee_balance"`
	Amount        int64  `json:"amount"`
	RequestID     string `json:"request_id"`
	Remark        string `json:"remark"`
}

// AdjustBalance 是唯一能改 devices.coffee_balance 的入口。
//
// 四件事必须在同一个事务里：锁住设备行、改余额、追加流水、往 outbox 记审计。少任何
// 一步都会留下一种对不上的状态——余额变了没有流水，或者流水记了审计没记。冲正不在这
// 条路径上：它要求指向被冲正的那条流水（reverses_entry_id），是另一个入口的事；
// 管理员改错了就再调一次相反金额，流水上留两条而不是一条。
func (r *pgAdminRepository) AdjustBalance(ctx context.Context, in BalanceAdjustment) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 行锁不能省：余额是读-改-写，两个并发调整各自读到同一个旧值、各自算出新值，
	// 后提交的那条会覆盖前一条，devices.coffee_balance 与流水表上的账就此对不上。
	var before int64
	var deviceName string
	err = tx.QueryRow(ctx, `SELECT coffee_balance, device_name FROM devices WHERE id = $1 FOR UPDATE`,
		in.DeviceID).Scan(&before, &deviceName)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrDeviceNotFound
	}
	if err != nil {
		return 0, err
	}

	// 先挡溢出再算：回绕出来的负数会以 CHECK 违反的形式炸掉，报错指向「余额为负」，
	// 而真正的原因是这次填的金额太大。
	if in.Amount > 0 && before > math.MaxInt64-in.Amount {
		return 0, ErrBalanceOutOfRange
	}
	after := before + in.Amount
	// 数据库有 CHECK (coffee_balance >= 0) 兜底，但在这里先判一次，才能把一个
	// 「余额不够扣」的业务结论回成 400，而不是让调用方收到约束违反翻译出来的 500。
	if after < 0 {
		return 0, ErrInsufficientBalance
	}

	if _, err := tx.Exec(ctx, `UPDATE devices SET coffee_balance = $2, updated_at = NOW() WHERE id = $1`,
		in.DeviceID, after); err != nil {
		return 0, mapPGError(err)
	}

	// operator_id 是权威值；operator_name 留空。令牌里没有用户名（见 platform/audit
	// 的 Entry 说明），展示名由读侧按 id 解析——在写入侧编一个名字出来，只会得到一个
	// 会过期的快照。
	var operatorID any
	if in.Operator != "" {
		operatorID = in.Operator
	}
	// type 固定 adjust：recharge 是小程序充值那条路、deduct 是 fulfillment 的提货
	// 扣减，两者都不经过后台；reverse 需要 reverses_entry_id，见上面的说明。
	if _, err := tx.Exec(ctx, `INSERT INTO device_balance_ledger (id, device_id, type,
		amount, balance_after, request_id, remark, operator_id, operator_name)
		VALUES ($1, $2, 'adjust', $3, $4, $5, $6, $7, '')`,
		uuid.NewString(), in.DeviceID, in.Amount, after, in.RequestID, in.Remark, operatorID); err != nil {
		return 0, mapPGError(err)
	}

	if err := r.audit.Record(ctx, tx, audit.Entry{
		// ActorID 用传进来的操作人，不留给 Recorder 去 ctx 里找：这条路径已经把
		// 操作人写进流水表的 operator_id 了，两处若是各取各的来源，就会出现流水说
		// 是甲改的、审计说找不到人。传空串时 Recorder 才回落到 ctx（见 platform/audit），
		// 所以非 HTTP 的调用方也不会把操作人整个丢掉。
		ActorID:    in.Operator,
		Module:     "device_balance",
		Action:     "adjust",
		Operation:  "调整咖啡余额",
		TargetType: "device", TargetID: in.DeviceID, TargetName: deviceName,
		Before: audit.Snapshot(balanceSnapshot{CoffeeBalance: before}),
		After:  audit.Snapshot(balanceSnapshot{CoffeeBalance: after, Amount: in.Amount, RequestID: in.RequestID, Remark: in.Remark}),
	}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return after, nil
}
