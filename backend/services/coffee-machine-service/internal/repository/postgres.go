// Package repository 是设备域主数据的数据访问层。
package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

// ErrDeviceNotFound 表示按 ID 查不到设备。
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
	ListDevices(ctx context.Context, filter DeviceFilter) ([]*model.Device, int64, error)
	ListManufacturers(ctx context.Context) ([]*model.Manufacturer, error)
	ListDrinks(ctx context.Context, filter DrinkFilter) ([]*model.Drink, int64, error)
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

func (r *postgresRepository) ListDevices(ctx context.Context, filter DeviceFilter) ([]*model.Device, int64, error) {
	// uuid 列必须转成 text 再比：PostgreSQL 会把参数按列类型解析，空串在 uuid 列上
	// 直接报 invalid input syntax，即便 OR 左边永远为真。
	//
	// 门店那一条多了 coalesce：pgx 把 nil 切片编码成 NULL，而 cardinality(NULL) 是
	// NULL——`NULL = 0` 不为真，整条 AND 求值成 NULL，于是「没选门店」会把每一行都
	// 筛掉，列表空得毫无线索。
	const where = ` WHERE ($1 = '' OR manufacturer_id::text = $1)
		AND (coalesce(cardinality($2::text[]), 0) = 0 OR store_id::text = ANY($2::text[]))
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
