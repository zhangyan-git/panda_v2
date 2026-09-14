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

// DeviceFilter 是设备列表的过滤条件。空串表示不过滤。
type DeviceFilter struct {
	ManufacturerID string
	StoreID        string
	Status         string
	Page           int
	PageSize       int
}

// DrinkFilter 是饮品列表的过滤条件。空串表示不过滤。
type DrinkFilter struct {
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
	ListDeviceDrinks(ctx context.Context, deviceID string) ([]*model.DeviceDrink, error)
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
	const where = ` WHERE ($1 = '' OR manufacturer_id::text = $1)
		AND ($2 = '' OR store_id::text = $2)
		AND ($3 = '' OR status = $3)`
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM devices`+where, filter.ManufacturerID, filter.StoreID, filter.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+deviceColumns+` FROM devices`+where+
		` ORDER BY created_at DESC, id LIMIT $4 OFFSET $5`,
		filter.ManufacturerID, filter.StoreID, filter.Status, filter.PageSize, (filter.Page-1)*filter.PageSize)
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

const drinkColumns = `id::text, manufacturer_id::text, origin_id, product_num, product_name,
	en_name, drink_type, product_img, product_desc, price, vip_price, pickup_code_price,
	status, sort, created_at, updated_at`

func scanDrink(row interface{ Scan(...any) error }) (*model.Drink, error) {
	d := &model.Drink{}
	err := row.Scan(&d.ID, &d.ManufacturerID, &d.OriginID, &d.ProductNum, &d.ProductName,
		&d.EnName, &d.DrinkType, &d.ProductImg, &d.ProductDesc, &d.Price, &d.VipPrice,
		&d.PickupCodePrice, &d.Status, &d.Sort, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

func (r *postgresRepository) ListDrinks(ctx context.Context, filter DrinkFilter) ([]*model.Drink, int64, error) {
	const where = ` WHERE ($1 = '' OR manufacturer_id::text = $1) AND ($2 = '' OR status = $2)`
	var total int64
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM drinks`+where, filter.ManufacturerID, filter.Status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+drinkColumns+` FROM drinks`+where+
		` ORDER BY sort, created_at DESC, id LIMIT $3 OFFSET $4`,
		filter.ManufacturerID, filter.Status, filter.PageSize, (filter.Page-1)*filter.PageSize)
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

// ListDeviceDrinks 返回一台设备供应的全部饮品，含每机覆盖价。
// 不做分页：一台设备的饮品数量由设备本身决定，量级是几十。
func (r *postgresRepository) ListDeviceDrinks(ctx context.Context, deviceID string) ([]*model.DeviceDrink, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text, device_id::text, drink_id::text, enabled,
		sort_order, price, vip_price, pickup_code_price, created_at, updated_at
		FROM device_drinks WHERE device_id = $1 ORDER BY sort_order, id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relations := make([]*model.DeviceDrink, 0, 16)
	for rows.Next() {
		rel := &model.DeviceDrink{}
		if err := rows.Scan(&rel.ID, &rel.DeviceID, &rel.DrinkID, &rel.Enabled, &rel.SortOrder,
			&rel.Price, &rel.VipPrice, &rel.PickupCodePrice, &rel.CreatedAt, &rel.UpdatedAt); err != nil {
			return nil, err
		}
		relations = append(relations, rel)
	}
	return relations, rows.Err()
}
