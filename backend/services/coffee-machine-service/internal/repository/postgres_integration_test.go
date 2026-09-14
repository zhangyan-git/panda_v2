package repository

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 这些测试只用调用方给的数据库 URL，自己不建库、不删库、不跑迁移，夹具行统一用
// 随机 ID，并按依赖顺序在用例结束后清理。
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("COFFEE_MACHINE_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set COFFEE_MACHINE_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("coffee machine test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("coffee machine test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// deleteFixture 执行一条夹具清理语句，失败时报出来而不是丢掉。
//
// 丢掉错误正是这几个夹具原先的毛病：余额用例写的流水删不掉（原因见
// balanceDeviceFixture），设备的 DELETE 被那条流水挡下，厂商跟着也删不掉，于是每跑
// 一次测试就在开发库里多留几行，而 go test 照样全绿。清理失败漏的是数据，必须看得见。
func deleteFixture(t *testing.T, pool *pgxpool.Pool, stmt string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), stmt, args...); err != nil {
		t.Errorf("清理夹具行失败：%s：%v", stmt, err)
	}
}

// cleanupFixture 把一行夹具的清理注册到用例结束时执行。
func cleanupFixture(t *testing.T, pool *pgxpool.Pool, stmt string, args ...any) {
	t.Helper()
	t.Cleanup(func() { deleteFixture(t, pool, stmt, args...) })
}

// deviceFixture 插一台设备并注册清理。vendorOnline 与 storeID 是 in/out：调用方给
// 什么就插什么，用来验证 NULL 能不能原样读回来。
func deviceFixture(t *testing.T, pool *pgxpool.Pool, manufacturerID, status string, vendorOnline *bool, storeID *string) string {
	t.Helper()
	ctx := context.Background()
	deviceID := uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO devices(id, serial_unique, device_name, manufacturer_id, store_id, status, vendor_online)
		VALUES($1, $2, $3, $4, $5, $6, $7)`,
		deviceID, "it-"+deviceID, "集成测试设备", manufacturerID, storeID, status, vendorOnline)
	if err != nil {
		t.Fatalf("insert device: %v", err)
	}
	t.Cleanup(func() {
		// 按依赖顺序删，一个闭包里依次执行：流水与设备事件的外键是 RESTRICT 而不是
		// CASCADE（见 pg_constraint），线上没人想删台设备就顺手带走余额流水和事件
		// 记录，所以得先删它们；device_drinks 与 device_payment_methods 是 CASCADE，
		// 不用管。
		//
		// 这里删流水还有一层意思：device_balance_ledger 上有 BEFORE DELETE/UPDATE
		// 触发器，删除是删不掉的。所以这两句在「本夹具本来就不该有流水」时是空操作，
		// 一旦用例真写了流水就会报错——那就说明它该改用 balanceDeviceFixture。
		deleteFixture(t, pool, `DELETE FROM device_balance_ledger WHERE device_id = $1`, deviceID)
		deleteFixture(t, pool, `DELETE FROM device_events WHERE device_id = $1`, deviceID)
		deleteFixture(t, pool, `DELETE FROM devices WHERE id = $1`, deviceID)
	})
	return deviceID
}

// balanceDeviceFixture 是 deviceFixture 加上「会写余额流水」这一件事。
//
// 余额用例造完设备就会写 device_balance_ledger，而那张表上有 BEFORE DELETE OR
// UPDATE 触发器，流水只增不改不删——这正是它该有的样子，也意味着有流水的设备在线上
// 根本删不掉。测试造的那几行还是得收走，否则每跑一次就在开发库里留一台设备、一个
// 厂商和一串流水。
//
// 收的方式是 SET LOCAL session_replication_role = replica：只在这一条连接、这一条
// 事务里让触发器歇一次，删完即恢复，不动表结构也不影响别的会话。注册的清理晚于
// deviceFixture 的，按 LIFO 会先跑——流水删掉了，后面设备那条 DELETE 才过得去。
func balanceDeviceFixture(t *testing.T, pool *pgxpool.Pool, manufacturerID string) string {
	t.Helper()
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	t.Cleanup(func() { deleteLedgerRows(t, pool, deviceID) })
	return deviceID
}

// deleteLedgerRows 删掉一台设备留下的余额流水与设备事件。
func deleteLedgerRows(t *testing.T, pool *pgxpool.Pool, deviceID string) {
	t.Helper()
	ctx := context.Background()
	// SET LOCAL 是事务级的，必须和 DELETE 落在同一条连接上：pool.Exec 每次可能换一条
	// 连接，那样设置会留在别的连接上，DELETE 照样被触发器挡下。
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Errorf("取连接清流水失败：%v", err)
		return
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Errorf("开启清理事务失败：%v", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Errorf("关闭触发器失败（需要超级用户）：%v", err)
		return
	}
	for _, stmt := range []string{
		`DELETE FROM device_balance_ledger WHERE device_id = $1`,
		`DELETE FROM device_events WHERE device_id = $1`,
	} {
		if _, err := tx.Exec(ctx, stmt, deviceID); err != nil {
			t.Errorf("清理流水失败：%s：%v", stmt, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("提交清理事务失败：%v", err)
	}
}

func manufacturerFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	manufacturerID := uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO manufacturers(id, code, name) VALUES($1, $2, $3)`,
		manufacturerID, "it-"+manufacturerID, "集成测试厂商")
	if err != nil {
		t.Fatalf("insert manufacturer: %v", err)
	}
	cleanupFixture(t, pool, `DELETE FROM manufacturers WHERE id = $1`, manufacturerID)
	return manufacturerID
}

func TestGetDeviceDistinguishesUnknownFromOffline(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)

	// 从未同步过：vendor_online 是 NULL，不是 false。这是整个设备域最容易读错的
	// 一处——「不知道」和「离线」在下单校验里是两个结论。
	neverSynced := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	device, err := repo.GetDevice(ctx, neverSynced)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if device.VendorOnline != nil {
		t.Fatalf("vendor_online = %v, want nil for a device that was never synced", *device.VendorOnline)
	}
	if device.StoreID != nil {
		t.Fatalf("store_id = %v, want nil for a device with no store", *device.StoreID)
	}

	offline := false
	syncedOffline := deviceFixture(t, pool, manufacturerID, "active", &offline, nil)
	device, err = repo.GetDevice(ctx, syncedOffline)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if device.VendorOnline == nil || *device.VendorOnline {
		t.Fatalf("vendor_online = %v, want a known false", device.VendorOnline)
	}
}

func TestGetDeviceReportsMissingAsNotFound(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)

	_, err := repo.GetDevice(context.Background(), uuid.NewString())
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("err = %v, want ErrDeviceNotFound", err)
	}
}

func TestListDevicesFiltersAndPages(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	otherManufacturerID := manufacturerFixture(t, pool)
	storeID := uuid.NewString()

	deviceFixture(t, pool, manufacturerID, "active", nil, &storeID)
	deviceFixture(t, pool, manufacturerID, "disabled", nil, nil)
	deviceFixture(t, pool, otherManufacturerID, "active", nil, nil)

	active, total, err := repo.ListDevices(ctx, DeviceFilter{ManufacturerID: manufacturerID, Status: "active", Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if total != 1 || len(active) != 1 {
		t.Fatalf("total = %d, len = %d; want 1 and 1", total, len(active))
	}
	if active[0].StoreID == nil || *active[0].StoreID != storeID {
		t.Fatalf("store_id did not round-trip: %v", active[0].StoreID)
	}

	// 空过滤条件不能把 uuid 列上的空串当参数解析——那会直接报 invalid input syntax。
	_, total, err = repo.ListDevices(ctx, DeviceFilter{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list devices without filters: %v", err)
	}
	if total < 2 {
		t.Fatalf("total = %d, want at least the three fixture devices", total)
	}

	// 页码越界返回空页而不是报错。
	empty, _, err := repo.ListDevices(ctx, DeviceFilter{ManufacturerID: manufacturerID, Page: 99, PageSize: 10})
	if err != nil {
		t.Fatalf("list out-of-range page: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("len = %d, want 0 for an out-of-range page", len(empty))
	}
}

func TestListDeviceDrinksKeepsNullOverrideDistinctFromZero(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)

	inheritDrinkID := uuid.NewString()
	freeDrinkID := uuid.NewString()
	for _, drinkID := range []string{inheritDrinkID, freeDrinkID} {
		_, err := pool.Exec(ctx, `INSERT INTO drinks(id, manufacturer_id, product_name, price) VALUES($1, $2, $3, 1800)`,
			drinkID, manufacturerID, "集成测试饮品")
		if err != nil {
			t.Fatalf("insert drink: %v", err)
		}
		cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drinkID)
	}

	_, err := pool.Exec(ctx, `INSERT INTO device_drinks(id, device_id, drink_id, enabled, sort_order) VALUES($1, $2, $3, TRUE, 1)`,
		uuid.NewString(), deviceID, inheritDrinkID)
	if err != nil {
		t.Fatalf("insert inheriting relation: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO device_drinks(id, device_id, drink_id, enabled, sort_order, price) VALUES($1, $2, $3, TRUE, 2, 0)`,
		uuid.NewString(), deviceID, freeDrinkID)
	if err != nil {
		t.Fatalf("insert zero-priced relation: %v", err)
	}

	relations, err := repo.ListDeviceDrinks(ctx, deviceID)
	if err != nil {
		t.Fatalf("list device drinks: %v", err)
	}
	if len(relations) != 2 {
		t.Fatalf("len = %d, want 2", len(relations))
	}
	if relations[0].Price != nil {
		t.Fatalf("price = %v, want nil for a relation that inherits the catalog price", *relations[0].Price)
	}
	if relations[1].Price == nil || *relations[1].Price != 0 {
		t.Fatalf("price = %v, want a known 0 for a device that sells it for free", relations[1].Price)
	}
}

func TestListDrinksFiltersByManufacturer(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)

	drinkID := uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO drinks(id, manufacturer_id, product_name, origin_id, price) VALUES($1, $2, $3, $4, 1500)`,
		drinkID, manufacturerID, "集成测试饮品二", "it-origin-"+drinkID)
	if err != nil {
		t.Fatalf("insert drink: %v", err)
	}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drinkID)

	drinks, total, err := repo.ListDrinks(ctx, DrinkFilter{ManufacturerID: manufacturerID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list drinks: %v", err)
	}
	if total != 1 || len(drinks) != 1 {
		t.Fatalf("total = %d, len = %d; want 1 and 1", total, len(drinks))
	}
	if drinks[0].OriginID != "it-origin-"+drinkID {
		t.Fatalf("origin_id = %q, want the value that was inserted", drinks[0].OriginID)
	}
}
