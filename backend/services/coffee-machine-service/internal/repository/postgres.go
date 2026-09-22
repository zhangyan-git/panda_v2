// Package repository 是设备域主数据的数据访问层。
package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

// ErrDeviceNotFound 表示按 ID 或序列号都查不到设备。
var ErrDeviceNotFound = errors.New("device not found")

// DeviceFilter 是设备列表的过滤条件。空串/空切片表示不过滤。
//
// StoreIDs 是切片而不是单个值：后台的筛选栏里门店是多选，而「选了三个门店」与
// 「一个都没选」必须区分开——后者不过滤，前者是三条 OR。传进来的每个 id 都必须是
// 合法 UUID，见下面 ListDevices 的说明。
type DeviceFilter struct {
	ManufacturerID string
	StoreIDs       []string
	Status         string
	// Keyword 按设备序列号模糊匹配（后台那一栏叫「设备标识」）。设备表只有几百行，
	// 上一个 pg_trgm 索引与它的维护成本换不来什么，所以就是一句 ILIKE。
	Keyword  string
	Page     int
	PageSize int
}

// DrinkFilter 是饮品列表的过滤条件。空串表示不过滤。
//
// DeviceID 是给饮品管理页用的：饮品行本身就挂在设备上，所以那一页天然想看「某台设备
// 上的饮品」。同样要转成 text 再比，理由见 ListDevices。
type DrinkFilter struct {
	DeviceID       string
	ManufacturerID string
	Status         string
	Page           int
	PageSize       int
}

// MasterDataRepository 是设备域主数据的读路径。
//
// 本服务对出杯链路的全部义务就是被读——下单时校验设备状态（方案 5.8），所以读
// 接口先落地。写入路径（后台的厂商/设备/饮品增改、余额调整）需要校验、审计与
// 幂等，随写接口一起加，不在这里留半成品方法。
type MasterDataRepository interface {
	GetDevice(ctx context.Context, id string) (*model.Device, error)
	// GetDeviceBySerial 按机器序列号取设备。线下刷卡机回调（方案 §四）只报序列号，
	// 那台机器不会知道我们的 uuid，所以这不是 GetDevice 的便利版，是那条路上唯一的入口。
	GetDeviceBySerial(ctx context.Context, serialUnique string) (*model.Device, error)
	ListDevices(ctx context.Context, filter DeviceFilter) ([]*model.Device, int64, error)
	ListManufacturers(ctx context.Context) ([]*model.Manufacturer, error)
	ListDrinks(ctx context.Context, filter DrinkFilter) ([]*model.Drink, int64, error)
	// GetDeviceDrink 按机器报的饮品编号取这台设备上的那一杯。编号同时比 product_num 与
	// origin_id：它落在哪一列取决于这家厂商当初的同步来源，调用方不该知道这件事。
	GetDeviceDrink(ctx context.Context, deviceID, drinkCode string) (*model.Drink, error)
	// GetDrink 按我们的 uuid 取一杯饮品（gRPC GetDrink 的落点）。它是 GetDeviceDrink 的
	// 另一把钥匙：那一条收机器报上来的编号，这一条收主键——下单的调用方手里只有后者。
	// 库里没有这一行时返回 ErrDrinkNotFound。
	GetDrink(ctx context.Context, id string) (*model.Drink, error)
	// ListDeviceDrinks 返回一台设备上的全部饮品。饮品行自带 device_id，所以这就是一次
	// 带设备条件的列表查询，不再是两张表的连接。
	ListDeviceDrinks(ctx context.Context, deviceID string) ([]*model.Drink, error)
	// ListDeviceBalanceEntries 返回一台设备的余额流水一页，最近的在前。
	ListDeviceBalanceEntries(ctx context.Context, deviceID string, page, pageSize int) ([]*model.DeviceBalanceEntry, int64, error)
}

type postgresRepository struct{ pool *pgxpool.Pool }

// NewPostgresRepository 创建 PostgreSQL 数据访问实现。
func NewPostgresRepository(pool *pgxpool.Pool) MasterDataRepository {
	return &postgresRepository{pool: pool}
}

// deviceColumns 是 devices 的全列投影。读路径整体取全列而不是各接口各挑一截：
// 列表与详情只差几个展示字段，两套列清单一旦分叉就会各自漂移。
const deviceColumns = `id::text, serial_unique, device_name, manufacturer_id::text,
	store_id::text, status, vendor_online, last_synced_at, last_fault_code,
	last_fault_message, last_fault_at, last_active_at, version_number, android_version,
	main_board_version, pickup_password, coffee_balance, show_vip,
	enable_coupon_verification, warranty_end_at, qrcode_type,
	regular_qrcode_payment_method, created_at, updated_at`

func scanDevice(row interface{ Scan(...any) error }) (*model.Device, error) {
	d := &model.Device{}
	err := row.Scan(&d.ID, &d.SerialUnique, &d.DeviceName, &d.ManufacturerID,
		&d.StoreID, &d.Status, &d.VendorOnline, &d.LastSyncedAt, &d.LastFaultCode,
		&d.LastFaultMessage, &d.LastFaultAt, &d.LastActiveAt, &d.VersionNumber,
		&d.AndroidVersion, &d.MainBoardVersion, &d.PickupPassword, &d.CoffeeBalance,
		&d.ShowVip, &d.EnableCouponVerification, &d.WarrantyEndAt, &d.QrcodeType,
		&d.RegularQrcodePaymentMethod, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

func (r *postgresRepository) GetDevice(ctx context.Context, id string) (*model.Device, error) {
	device, err := scanDevice(r.pool.QueryRow(ctx, `SELECT `+deviceColumns+` FROM devices WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	return device, err
}

// GetDeviceBySerial 按机器序列号取设备。
//
// serial_unique 上建表时就带了 UNIQUE 约束（001_coffee_machine_core.sql），所以最多命中
// 一行：不需要 LIMIT，也不需要决胜排序。空串的挡在 service 层，不落在这里——落到这里
// 就成了一次「查不到」，而「没给序列号」与「没这台机器」是两回事。
func (r *postgresRepository) GetDeviceBySerial(ctx context.Context, serialUnique string) (*model.Device, error) {
	device, err := scanDevice(r.pool.QueryRow(ctx,
		`SELECT `+deviceColumns+` FROM devices WHERE serial_unique = $1`, serialUnique))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	return device, err
}

func (r *postgresRepository) ListDevices(ctx context.Context, filter DeviceFilter) ([]*model.Device, int64, error) {
	// uuid 列必须转成 text 再比：PostgreSQL 会把参数按列类型解析，空串在 uuid 列上
	// 直接报 invalid input syntax，即便 OR 左边永远为真。
	//
	// 门店那一条判的是 **NULL 与空切片**，不是长度。这两种取值来自两个调用方，而它们
	// 要的是相反的意思：
	//
	//   - 后台筛选栏：没选门店时参数缺席，pgx 把 nil 切片编码成 NULL → 不过滤，
	//     后台的行为与改动前逐字一致。
	//   - 商户域：范围展开出来的空集编码成 '{}' → ANY('{}') 恒假 → 命中零行，
	//     这正是「这个账号一个点位都没授权」该有的结果。
	//
	// 写成 coalesce(cardinality($2::text[]), 0) = 0 会把后者读成前者——一个没授权任何
	// 点位的商户账号就能看到全平台的设备。这句话以前只服务后台，所以那时它是对的。
	const where = ` WHERE ($1 = '' OR manufacturer_id::text = $1)
		AND ($2::text[] IS NULL OR store_id::text = ANY($2::text[]))
		AND ($3 = '' OR status = $3)
		AND ($4 = '' OR serial_unique ILIKE '%' || $4 || '%')`
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM devices`+where,
		filter.ManufacturerID, filter.StoreIDs, filter.Status, filter.Keyword).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+deviceColumns+` FROM devices`+where+
		` ORDER BY created_at DESC, id LIMIT $5 OFFSET $6`,
		filter.ManufacturerID, filter.StoreIDs, filter.Status, filter.Keyword,
		filter.PageSize, (filter.Page-1)*filter.PageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	devices := make([]*model.Device, 0, filter.PageSize)
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, 0, err
		}
		devices = append(devices, device)
	}
	return devices, total, rows.Err()
}

// manufacturerColumns 单独抽出来是因为写路径也要按同样的顺序读回整行——创建接口回给
// 前端的必须是库里那一行，不是服务层自己拼的。两处各写一份列清单就会漂移。
const manufacturerColumns = `id::text, code, name, contact_name, contact_phone,
	status, created_at, updated_at`

func scanManufacturer(row interface{ Scan(...any) error }) (*model.Manufacturer, error) {
	m := &model.Manufacturer{}
	err := row.Scan(&m.ID, &m.Code, &m.Name, &m.ContactName, &m.ContactPhone,
		&m.Status, &m.CreatedAt, &m.UpdatedAt)
	return m, err
}

func (r *postgresRepository) ListManufacturers(ctx context.Context) ([]*model.Manufacturer, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+manufacturerColumns+` FROM manufacturers ORDER BY code, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	manufacturers := make([]*model.Manufacturer, 0, 16)
	for rows.Next() {
		m, err := scanManufacturer(rows)
		if err != nil {
			return nil, err
		}
		manufacturers = append(manufacturers, m)
	}
	return manufacturers, rows.Err()
}

const drinkColumns = `id::text, device_id::text, manufacturer_id::text, origin_id, product_num,
	product_name, en_name, drink_type, product_img, product_desc, price, vip_price,
	pickup_code_price, status, sort, created_at, updated_at`

func scanDrink(row interface{ Scan(...any) error }) (*model.Drink, error) {
	d := &model.Drink{}
	err := row.Scan(&d.ID, &d.DeviceID, &d.ManufacturerID, &d.OriginID, &d.ProductNum,
		&d.ProductName, &d.EnName, &d.DrinkType, &d.ProductImg, &d.ProductDesc, &d.Price,
		&d.VipPrice, &d.PickupCodePrice, &d.Status, &d.Sort, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

func (r *postgresRepository) ListDrinks(ctx context.Context, filter DrinkFilter) ([]*model.Drink, int64, error) {
	const where = ` WHERE ($1 = '' OR device_id::text = $1)
		AND ($2 = '' OR manufacturer_id::text = $2) AND ($3 = '' OR status = $3)`
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM drinks`+where,
		filter.DeviceID, filter.ManufacturerID, filter.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+drinkColumns+` FROM drinks`+where+
		` ORDER BY sort, created_at DESC, id LIMIT $4 OFFSET $5`,
		filter.DeviceID, filter.ManufacturerID, filter.Status,
		filter.PageSize, (filter.Page-1)*filter.PageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	drinks := make([]*model.Drink, 0, filter.PageSize)
	for rows.Next() {
		drink, err := scanDrink(rows)
		if err != nil {
			return nil, 0, err
		}
		drinks = append(drinks, drink)
	}
	return drinks, total, rows.Err()
}

// GetDeviceDrink 按机器报上来的编号取这台设备上的那一杯饮品。
//
// 编号两列都试（product_num 或 origin_id）：厂商侧编号落在哪一列取决于当初的同步来源，
// 老系统就是这么兜的（panda_serve 的 sync_order_handler `$or`），两种来源今天都还在库里。
// 两个参数的空串挡在 service 层，不落在这里——落到这里就是一次「查不到」。
//
// **不按 status 过滤。** 走到这里说明钱已经在机器上收过了：这时候回「查不到」，这笔钱
// 在库里就没有任何对应的饮品记录，比「卖了一杯已下架的饮品」严重得多。下架是给后台看
// 的状态，不该在这里变成一次丢单。
//
// 它可能匹配到多行：product_num 上没有唯一约束（唯一的是 (device_id, manufacturer_id,
// origin_id) 那个部分索引），同一台设备上两行共用一个 product_num 是允许的。此时
// QueryRow 取第一行，取到哪一行由执行计划决定——老系统的 FindOne 也是这个行为。
func (r *postgresRepository) GetDeviceDrink(ctx context.Context, deviceID, drinkCode string) (*model.Drink, error) {
	drink, err := scanDrink(r.pool.QueryRow(ctx, `SELECT `+drinkColumns+`
		FROM drinks WHERE device_id = $1 AND (product_num = $2 OR origin_id = $2)`,
		deviceID, drinkCode))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDrinkNotFound
	}
	return drink, err
}

// GetDrink 按主键取一杯饮品。与 GetDeviceDrink 是两把不同的钥匙，不要合并：那一条的答案
// 在特定一台设备上（`device_id + product_num|origin_id`），这一条的答案是库里那一行，
// 调用方手里只有我们的 uuid。
//
// **同样不按 status 过滤。** 这一条的调用方是 order-service 下单，它要判「这杯还卖不卖」，
// 而判这件事需要知道 status——在这里过滤掉，就等于把「已下架」与「没有这一杯」说成同一件
// 事，调用方只能把两者都拒了，而这两句话在收银台上差得很远。下架是一条事实，不是一次
// 查询失败。
func (r *postgresRepository) GetDrink(ctx context.Context, id string) (*model.Drink, error) {
	drink, err := scanDrink(r.pool.QueryRow(ctx, `SELECT `+drinkColumns+`
		FROM drinks WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDrinkNotFound
	}
	return drink, err
}

// ListDeviceDrinks 返回一台设备上的全部饮品。
// 不做分页：一台设备的饮品数量由设备本身决定，量级是几十。
//
// 排序用 sort 而不是 created_at：这是后台那一屏的展示次序，值越小越靠前，调完顺序
// 就该立刻在界面上按新顺序站好。id 是决胜位，同 sort 的行要有稳定次序。
func (r *postgresRepository) ListDeviceDrinks(ctx context.Context, deviceID string) ([]*model.Drink, error) {
	// 先确认设备在不在。少了这一句，一个打错的 id 会得到和「这台设备还没配饮品」
	// 完全一样的空数组，而这两件事在详情页上该说的话不同。这条查询走的是一次索引
	// 命中，与后面的列表比不上一趟往返的成本。
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id = $1)`, deviceID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrDeviceNotFound
	}
	rows, err := r.pool.Query(ctx, `SELECT `+drinkColumns+`
		FROM drinks WHERE device_id = $1 ORDER BY sort, id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	drinks := make([]*model.Drink, 0, 16)
	for rows.Next() {
		drink, err := scanDrink(rows)
		if err != nil {
			return nil, err
		}
		drinks = append(drinks, drink)
	}
	return drinks, rows.Err()
}

const balanceEntryColumns = `id::text, device_id::text, type, amount, balance_after,
	reverses_entry_id::text, reference_type, reference_id::text, request_id, remark,
	operator_id::text, operator_name, created_at`

func scanBalanceEntry(row interface{ Scan(...any) error }) (*model.DeviceBalanceEntry, error) {
	e := &model.DeviceBalanceEntry{}
	err := row.Scan(&e.ID, &e.DeviceID, &e.Type, &e.Amount, &e.BalanceAfter,
		&e.ReversesEntryID, &e.ReferenceType, &e.ReferenceID, &e.RequestID, &e.Remark,
		&e.OperatorID, &e.OperatorName, &e.CreatedAt)
	return e, err
}

// ListDeviceBalanceEntries 返回一台设备的余额流水一页，最近的在前。
//
// 分页而不是一次给全：这张表只增不改不删，一台高频使用的设备攒下的流水没有上界，而
// 这一屏要回答的是「最近这些钱是怎么来的」。排序走 device_balance_ledger_device_idx
// (device_id, created_at)：倒序扫这条索引即可，不必新增索引。
//
// id 是排序决胜位：同一次批量操作里的若干条流水时间戳可能完全相同（NOW() 在一个事务
// 里是同一个值），单靠时间排序没有稳定次序，翻页会重复或漏行。
func (r *postgresRepository) ListDeviceBalanceEntries(ctx context.Context, deviceID string, page, pageSize int) ([]*model.DeviceBalanceEntry, int64, error) {
	// 与 ListDeviceDrinks 同样的理由：一个打错的 id 要和「这台设备还没动过余额」分得开，
	// 前者重试无用，后者是正常状态。
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE id = $1)`, deviceID).Scan(&exists); err != nil {
		return nil, 0, err
	}
	if !exists {
		return nil, 0, ErrDeviceNotFound
	}
	var total int64
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM device_balance_ledger WHERE device_id = $1`, deviceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+balanceEntryColumns+`
		FROM device_balance_ledger WHERE device_id = $1
		ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`,
		deviceID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	entries := make([]*model.DeviceBalanceEntry, 0, pageSize)
	for rows.Next() {
		entry, err := scanBalanceEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, entry)
	}
	return entries, total, rows.Err()
}
