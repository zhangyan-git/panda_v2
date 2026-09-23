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
		// 记录，所以得先删它们；drinks 与 device_payment_methods 是 CASCADE，不用管
		// ——饮品的 device_id 有 ON DELETE CASCADE，删设备时这台机器的饮品跟着走，
		// 这正是「饮品行属于设备」该有的样子。
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

// TestGetDeviceBySerialFindsTheRowTheMachineKnows 是设备回调建单（方案 §四）的第一步：
// 机器只报序列号，我们要把它换成库里那一行，门店与状态都挂在那行上。
func TestGetDeviceBySerialFindsTheRowTheMachineKnows(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)

	device, err := repo.GetDeviceBySerial(ctx, "it-"+deviceID)
	if err != nil {
		t.Fatalf("get device by serial: %v", err)
	}
	if device.ID != deviceID {
		t.Fatalf("id = %q, want %q", device.ID, deviceID)
	}
	if device.SerialUnique != "it-"+deviceID {
		t.Fatalf("serial_unique = %q, want the serial that was inserted", device.SerialUnique)
	}

	// 没登记过的序列号要回 ErrDeviceNotFound，而不是一台零值设备：调用方靠这个把
	// 「这台机器没接入」和「读到了但字段空着」分开。
	if _, err := repo.GetDeviceBySerial(ctx, "it-"+uuid.NewString()); !errors.Is(err, ErrDeviceNotFound) {
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

// TestListDeviceDrinksScopesToTheDevice 是这一屏的核心查询：饮品行自带 device_id，
// 「这台设备上有哪些饮品」就是一句 WHERE。
//
// 边界要一起钉住——别的设备上的同款饮品、以及还没挂设备的行都不能混进来。它们和
// 「这台设备的饮品」在库里只差一个列值，前端没法分辨，漏筛的话界面会显示别的机器的
// 菜单而看不出任何异常。
func TestListDeviceDrinksScopesToTheDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	otherDeviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)

	insertDrink := func(device *string, name string, price int64, sort int) string {
		t.Helper()
		drinkID := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO drinks(id, device_id, manufacturer_id, product_name, price, sort)
			 VALUES($1, $2, $3, $4, $5, $6)`,
			drinkID, device, manufacturerID, name, price, sort); err != nil {
			t.Fatalf("insert drink: %v", err)
		}
		cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drinkID)
		return drinkID
	}
	// sort 故意与插入顺序相反：排序要按 sort 走，不是按 created_at 或者插入次序。
	freeDrinkID := insertDrink(&deviceID, "集成测试免费的", 0, 1)
	paidDrinkID := insertDrink(&deviceID, "集成测试美式", 1800, 2)
	insertDrink(&otherDeviceID, "另一台设备上的", 1800, 3)
	// 还没挂设备的行：它是合法的数据库状态（drinks.device_id 可空），但绝不属于任何一台设备。
	insertDrink(nil, "还没分配设备的", 1800, 4)

	drinks, err := repo.ListDeviceDrinks(ctx, deviceID)
	if err != nil {
		t.Fatalf("list device drinks: %v", err)
	}
	if len(drinks) != 2 {
		t.Fatalf("len = %d, want 2（别的设备上的、以及未分配设备的行都不该出现）", len(drinks))
	}
	if drinks[0].ID != freeDrinkID || drinks[1].ID != paidDrinkID {
		t.Fatalf("排序没按 sort 来：%s, %s", drinks[0].ID, drinks[1].ID)
	}
	// 0 是「这台机器上免费」，一个合法的售价，不是「没填」。
	if drinks[0].Price != 0 {
		t.Fatalf("price = %d, want 0 for a drink sold for free on this device", drinks[0].Price)
	}
	if drinks[1].Price != 1800 {
		t.Fatalf("price = %d, want 1800", drinks[1].Price)
	}
	if drinks[0].ProductName != "集成测试免费的" || drinks[1].ProductName != "集成测试美式" {
		t.Fatalf("product name 没跟着读出来：%q, %q", drinks[0].ProductName, drinks[1].ProductName)
	}
	// 每一行的 DeviceID 都该指回这台设备：详情页那一屏的写操作要把这个值原样回传，
	// 读回来是 nil 的话前端会把它当成「未分配设备」再存回去，饮品就从设备上掉了。
	for _, d := range drinks {
		if d.DeviceID == nil || *d.DeviceID != deviceID {
			t.Fatalf("deviceId = %v, want %s", d.DeviceID, deviceID)
		}
	}
}

// TestListDeviceDrinksReportsUnknownDevice 盯的是「设备不存在」与「这台设备还没配饮品」
// 的分野。两者都会得到一个空列表，而详情页上该说的话完全不同：一个是地址打错了，
// 一个是等厂商同步。
func TestListDeviceDrinksReportsUnknownDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)

	_, err := repo.ListDeviceDrinks(context.Background(), uuid.NewString())
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("err = %v, want ErrDeviceNotFound", err)
	}
}

// TestListDevicesFiltersByStoreIDsAndKeyword 覆盖筛选栏的两项。
//
// 门店的多选语义在这里被钉死：选了多个是并集（不是只认第一个），**nil 不过滤而空切片
// 命中零行**。后两者是这条谓词的全部要害，而它们只差一次手写：两个调用方要的正好相反，
// 后台没选门店要的是全量，商户账号一个点位都没授权要的是没有。把空切片也读成「不过滤」
// 就是让前者顺带放开了后者。
func TestListDevicesFiltersByStoreIDsAndKeyword(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	storeA, storeB := uuid.NewString(), uuid.NewString()

	deviceA := deviceFixture(t, pool, manufacturerID, "active", nil, &storeA)
	deviceFixture(t, pool, manufacturerID, "active", nil, &storeB)
	deviceFixture(t, pool, manufacturerID, "active", nil, nil)

	both, total, err := repo.ListDevices(ctx, DeviceFilter{StoreIDs: []string{storeA, storeB}, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list by two stores: %v", err)
	}
	if total != 2 || len(both) != 2 {
		t.Fatalf("两个门店应该是并集：total = %d, len = %d, want 2 and 2", total, len(both))
	}

	one, total, err := repo.ListDevices(ctx, DeviceFilter{StoreIDs: []string{storeA}, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list by one store: %v", err)
	}
	if total != 1 || len(one) != 1 || one[0].StoreID == nil || *one[0].StoreID != storeA {
		t.Fatalf("只选一个门店却返回了 %d 行", total)
	}

	// nil（后台没选门店）不过滤：pgx 把 nil 切片编码成 NULL，谓词判的就是这个 NULL。
	// 这一条守住的是后台行为一字未变。
	all, total, err := repo.ListDevices(ctx, DeviceFilter{StoreIDs: nil, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list with a nil filter: %v", err)
	}
	if total < 3 || len(all) < 3 {
		t.Fatalf("nil 门店条件把列表筛成了 %d 行，want 至少三台夹具设备", total)
	}

	// 空切片（商户账号一个点位都没授权）命中零行，total 与列表都必须是 0：只要有一个
	// 不是 0，就说明过滤只作用在了其中一条查询上。
	none, total, err := repo.ListDevices(ctx, DeviceFilter{StoreIDs: []string{}, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list with an empty scope: %v", err)
	}
	if total != 0 || len(none) != 0 {
		t.Fatalf("空范围应命中零行，得到 total = %d, len = %d", total, len(none))
	}

	// 取 A 序列号中间的一段做模糊匹配。序列号是 "it-"+uuid，这 8 位十六进制只会出现在
	// A 自己身上，所以命中数必须是 1——写成「大于 0」的话，一条把 keyword 整个忽略掉的
	// 实现也能过。
	keyword, total, err := repo.ListDevices(ctx, DeviceFilter{Keyword: deviceA[6:14], Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list by keyword: %v", err)
	}
	if total != 1 || len(keyword) != 1 || keyword[0].ID != deviceA {
		t.Fatalf("设备标识模糊匹配：total = %d, len = %d, 命中的是 %v，want 只有 %s",
			total, len(keyword), keyword, deviceA)
	}
}

// TestListDeviceBalanceEntriesReadsNewestFirstAndPages 覆盖设备详情页「账变记录」那一屏
// 的读路径。
//
// 流水直接 INSERT 而不是走 AdjustBalance：这里要的是三种 type 各一行、带 reference 的
// 一行、operator_id 为 NULL 的一行，走写接口造不出后面几种（后台调整只会写 adjust）。
// created_at 显式给值，排序才有确定的期望——靠 NOW() 的话同一毫秒内的两行谁在前是随机的。
func TestListDeviceBalanceEntriesReadsNewestFirstAndPages(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	operatorID := uuid.NewString()
	// 充值是第一笔（变动前余额为 0），后面两笔在它之上继续。
	rechargeID := insertLedgerFixture(t, pool, deviceID, "recharge", 20000, 20000, nil, "", nil, operatorID, base)
	deductID := insertLedgerFixture(t, pool, deviceID, "deduct", -5000, 15000, nil, "", nil, "", base.Add(time.Minute))
	// reference 指向提货单那一类来源单据；冲正指向被冲正的那条流水（表上有 CHECK，冲正
	// 必须带 reverses_entry_id）。
	referenceID := uuid.NewString()
	insertLedgerFixture(t, pool, deviceID, "deduct", -1500, 13500, nil, "pickup", &referenceID, "", base.Add(2*time.Minute))
	insertLedgerFixture(t, pool, deviceID, "reverse", 1500, 15000, &deductID, "", nil, "", base.Add(3*time.Minute))

	entries, total, err := repo.ListDeviceBalanceEntries(ctx, deviceID, 1, 2)
	if err != nil {
		t.Fatalf("list balance entries: %v", err)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	// 倒序：最新的一页是冲正和那条带来源单据的扣减。
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2 (pageSize 限制)", len(entries))
	}
	if entries[0].Type != "reverse" || entries[1].Type != "deduct" {
		t.Fatalf("第一页顺序 = %q, %q；want reverse, deduct", entries[0].Type, entries[1].Type)
	}
	if entries[0].ReversesEntryID == nil || *entries[0].ReversesEntryID != deductID {
		t.Fatalf("reverses_entry_id = %v, want %s", entries[0].ReversesEntryID, deductID)
	}
	if entries[1].ReferenceType != "pickup" || entries[1].ReferenceID == nil || *entries[1].ReferenceID != referenceID {
		t.Fatalf("来源单据没读回来：%q / %v", entries[1].ReferenceType, entries[1].ReferenceID)
	}

	// 第二页是剩下的两笔。翻页要真能翻到，而不是永远返回第一页。
	second, _, err := repo.ListDeviceBalanceEntries(ctx, deviceID, 2, 2)
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if len(second) != 2 || second[0].ID != deductID || second[1].ID != rechargeID {
		t.Fatalf("第二页 = %v，want deduct 与 recharge（按时间倒序）", second)
	}

	// balance_after 是变动**之后**的余额，不是增量；operator_id 为空要原样是 nil，
	// 不能被读成一个零值 uuid（那会在界面上显示成另一个操作人）。
	recharge := second[1]
	if recharge.Amount != 20000 || recharge.BalanceAfter != 20000 {
		t.Fatalf("首笔充值 amount/balance_after = %d/%d, want 20000/20000", recharge.Amount, recharge.BalanceAfter)
	}
	if recharge.OperatorID == nil || *recharge.OperatorID != operatorID {
		t.Fatalf("operator_id = %v, want %s", recharge.OperatorID, operatorID)
	}
	if second[0].OperatorID != nil {
		t.Fatalf("没有操作人的那笔读出了 operator_id = %v, want nil", *second[0].OperatorID)
	}
	if second[0].Remark != "" {
		t.Fatalf("remark = %q, want 空串", second[0].Remark)
	}
}

// TestListDeviceBalanceEntriesReportsUnknownDevice 同 ListDeviceDrinks：设备不存在要
// 能和「这台设备还没动过余额」分开，前者在详情页上是地址打错了。
func TestListDeviceBalanceEntriesReportsUnknownDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)

	_, _, err := repo.ListDeviceBalanceEntries(context.Background(), uuid.NewString(), 1, 20)
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("err = %v, want ErrDeviceNotFound", err)
	}
}

// insertLedgerFixture 插一条流水并返回它的 id。清理由 balanceDeviceFixture 统一负责
// （那张表只增不改不删，普通 deviceFixture 的清理会在有流水时直接报错）。
func insertLedgerFixture(
	t *testing.T, pool *pgxpool.Pool, deviceID, entryType string, amount, balanceAfter int64,
	reversesEntryID *string, referenceType string, referenceID *string, operatorID string, createdAt time.Time,
) string {
	t.Helper()
	entryID := uuid.NewString()
	var operator any
	if operatorID != "" {
		operator = operatorID
	}
	_, err := pool.Exec(context.Background(), `INSERT INTO device_balance_ledger
		(id, device_id, type, amount, balance_after, reverses_entry_id, reference_type, reference_id, operator_id, created_at)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		entryID, deviceID, entryType, amount, balanceAfter, reversesEntryID, referenceType, referenceID, operator, createdAt)
	if err != nil {
		t.Fatalf("insert ledger row: %v", err)
	}
	return entryID
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

// codedDrinkFixture 插一行带编号与上下架状态的饮品并注册清理。admin_integration_test.go
// 里那个 drinkFixture 只填名字，编号两列留默认空串——设备回调那条路要按编号找人，
// 所以这里单独一个：deviceID 为 nil 表示这行还没挂设备（drinks.device_id 可空，库里允许
// 这样的行）。
func codedDrinkFixture(t *testing.T, pool *pgxpool.Pool, deviceID *string, manufacturerID, productNum, originID, status string) string {
	t.Helper()
	drinkID := uuid.NewString()
	_, err := pool.Exec(context.Background(), `INSERT INTO drinks(id, device_id, manufacturer_id, product_name, origin_id, product_num, price, status)
		VALUES($1, $2, $3, $4, $5, $6, 1500, $7)`,
		drinkID, deviceID, manufacturerID, "集成测试饮品", originID, productNum, status)
	if err != nil {
		t.Fatalf("insert drink: %v", err)
	}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drinkID)
	return drinkID
}

// TestGetDeviceDrinkFindsTheCodeTheMachineReports 是设备回调建单（方案 §四）的第二步：
// 机器报的编号落在哪一列由当初的同步来源决定，两列都要命中；而已经收过钱的那一杯，
// 不能因为下架就变成「查不到」。
func TestGetDeviceDrinkFindsTheCodeTheMachineReports(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	otherDeviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)

	// 同步来源之一：编号落在 product_num，而且这杯已经下架。
	byProductNum := codedDrinkFixture(t, pool, &deviceID, manufacturerID, "1001", "", "off_shelf")
	// 另一种来源：编号落在 origin_id。
	byOrigin := codedDrinkFixture(t, pool, &deviceID, manufacturerID, "", "2002", "on_shelf")
	// 同一个编号在别的设备上也有一行：设备维度不能串。
	codedDrinkFixture(t, pool, &otherDeviceID, manufacturerID, "1001", "", "on_shelf")

	drink, err := repo.GetDeviceDrink(ctx, deviceID, "1001")
	if err != nil {
		t.Fatalf("get device drink by product_num: %v", err)
	}
	if drink.ID != byProductNum {
		t.Fatalf("id = %q, want the row whose product_num matches (%q)", drink.ID, byProductNum)
	}
	// 不按 status 过滤：下架也认，钱已经在机器上收过了。
	if drink.Status != "off_shelf" {
		t.Fatalf("status = %q, want the off_shelf row to match too", drink.Status)
	}

	drink, err = repo.GetDeviceDrink(ctx, deviceID, "2002")
	if err != nil {
		t.Fatalf("get device drink by origin_id: %v", err)
	}
	if drink.ID != byOrigin {
		t.Fatalf("id = %q, want the row whose origin_id matches (%q)", drink.ID, byOrigin)
	}

	// 别的设备上的同一编号不算数：查询是按设备定位的。
	if _, err := repo.GetDeviceDrink(ctx, otherDeviceID, "2002"); !errors.Is(err, ErrDrinkNotFound) {
		t.Fatalf("err = %v, want ErrDrinkNotFound for a code that lives on another device", err)
	}
	if _, err := repo.GetDeviceDrink(ctx, deviceID, "no-such-code"); !errors.Is(err, ErrDrinkNotFound) {
		t.Fatalf("err = %v, want ErrDrinkNotFound", err)
	}
}

// TestGetDrinkReadsTheRowByPrimaryKey 是小程序下单定价（order-service 那条路）的第一步：
// 调用方手里只有我们的 uuid，按它取一行。
//
// 与上面那条**不是同一个查询的两副面孔**：那一条按设备 + 机器报的编号定位，这一条按主键。
// 它同样**不筛 status**：这一杯现在卖不卖是订单域该判的事（钱收没收到是那边的事实），
// 咖啡机域只如实把 status 回出去。
func TestGetDrinkReadsTheRowByPrimaryKey(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)

	drinkID := codedDrinkFixture(t, pool, &deviceID, manufacturerID, "3003", "", "off_shelf")
	// 另一台设备上的一行：按主键取不该被设备维度影响，取谁是谁。
	otherDeviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	otherDrinkID := codedDrinkFixture(t, pool, &otherDeviceID, manufacturerID, "4004", "", "on_shelf")

	drink, err := repo.GetDrink(ctx, drinkID)
	if err != nil {
		t.Fatalf("get drink: %v", err)
	}
	if drink.ID != drinkID || drink.ProductNum != "3003" {
		t.Fatalf("id/product_num = %q/%q, want %q/3003", drink.ID, drink.ProductNum, drinkID)
	}
	// 下架的那一行照样取得到，status 原样带回来。
	if drink.Status != "off_shelf" {
		t.Fatalf("status = %q, want off_shelf（这一条不按 status 过滤）", drink.Status)
	}
	if drink.DeviceID == nil || *drink.DeviceID != deviceID {
		t.Fatalf("device_id = %v, want %q（下单要靠它判「这杯在不在那台设备上」）", drink.DeviceID, deviceID)
	}

	other, err := repo.GetDrink(ctx, otherDrinkID)
	if err != nil {
		t.Fatalf("get the other device's drink: %v", err)
	}
	if other.DeviceID == nil || *other.DeviceID != otherDeviceID {
		t.Fatalf("device_id = %v, want %q", other.DeviceID, otherDeviceID)
	}

	// 目录里没有这个 id：与「编号在这台机器上查不到」是同一个哨兵——对调用方来说都是
	// 「你给的这个标识在我这儿找不到那一杯」。
	if _, err := repo.GetDrink(ctx, uuid.NewString()); !errors.Is(err, ErrDrinkNotFound) {
		t.Fatalf("err = %v, want ErrDrinkNotFound", err)
	}
}

// TestListDrinksScopesToADeviceAndKeepsUnassignedRowsApart 盯的是 drinks 的两类行：
// 挂在设备上的，和还没挂设备的（device_id 为 NULL）。按设备过滤只该给出前者，不过滤才是
// 两类都在——把两者写成同一个结果，设备回调建单就会拿到别的机器上的饮品。
func TestListDrinksScopesToADeviceAndKeepsUnassignedRowsApart(t *testing.T) {
	pool := integrationPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)

	mineID := uuid.NewString()
	_, err := pool.Exec(ctx, `INSERT INTO drinks(id, device_id, manufacturer_id, product_name, origin_id, price)
		VALUES($1, $2, $3, $4, $5, 1500)`,
		mineID, deviceID, manufacturerID, "集成测试饮品·本机", "it-origin-"+mineID)
	if err != nil {
		t.Fatalf("insert drink on the device: %v", err)
	}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, mineID)

	// 遗留行：drinks.device_id 的列注释说明库里可能还有没挂设备的饮品，过滤与不过滤的差别
	// 就出在它身上。
	orphanID := uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO drinks(id, manufacturer_id, product_name, origin_id, price)
		VALUES($1, $2, $3, $4, 1500)`,
		orphanID, manufacturerID, "集成测试饮品·未挂设备", "it-origin-"+orphanID)
	if err != nil {
		t.Fatalf("insert unassigned drink: %v", err)
	}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, orphanID)

	drinks, total, err := repo.ListDrinks(ctx, DrinkFilter{DeviceID: deviceID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list drinks by device: %v", err)
	}
	if total != 1 || len(drinks) != 1 || drinks[0].ID != mineID {
		t.Fatalf("total = %d, len = %d; want only the drink on this device", total, len(drinks))
	}

	// 不过滤设备时按厂商收窄到本次夹具，两类行都该在。
	all, allTotal, err := repo.ListDrinks(ctx, DrinkFilter{ManufacturerID: manufacturerID, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list drinks by manufacturer: %v", err)
	}
	if allTotal != 2 || len(all) != 2 {
		t.Fatalf("total = %d, len = %d; want both the assigned and the unassigned row", allTotal, len(all))
	}
}
