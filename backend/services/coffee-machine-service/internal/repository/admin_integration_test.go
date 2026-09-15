package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/model"
)

// 这些用例盯的是「写路径真的把该落的都落了」，尤其是审计：余额调整是方案 11.6
// L893 必审清单上的操作，而它唯一的证据就是同一个事务里那条 outbox 记录。所以每个
// 写入用例都顺手数一遍 message_outbox——只断言业务行变了，等于没验证审计。

// adminRepo 建一个带真实 Recorder 的写仓库。用真 Recorder 而不是 Noop：审计是不是
// 真的落进 outbox 正是这些用例要验的东西之一。
func adminRepo(t *testing.T, pool *pgxpool.Pool) AdminRepository {
	t.Helper()
	return NewAdminRepository(pool, audit.NewRecorder())
}

// countAudit 数出这个 target 上的审计事件条数。按 target_id 过滤，避免把别的用例
// （或并发跑的别的测试）的行算进来。
func countAudit(t *testing.T, pool *pgxpool.Pool, targetID string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM message_outbox WHERE event_type = $1 AND convert_from(payload, 'UTF8') LIKE $2`,
		audit.EventType, "%"+targetID+"%").Scan(&count)
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return count
}

// latestAudit 读出该 target 上最近一条审计事件，解开成 Entry 供断言。
func latestAudit(t *testing.T, pool *pgxpool.Pool, targetID string) audit.Entry {
	t.Helper()
	var payload []byte
	err := pool.QueryRow(context.Background(),
		`SELECT payload FROM message_outbox WHERE event_type = $1 AND convert_from(payload, 'UTF8') LIKE $2
		 ORDER BY created_at DESC LIMIT 1`,
		audit.EventType, "%"+targetID+"%").Scan(&payload)
	if err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	var entry audit.Entry
	if err := json.Unmarshal(payload, &entry); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	return entry
}

// cleanupAudit 删掉这个 target 留下的 outbox 行。运行时 relay 会把它们标成已发布，
// 但行还在——留着的话，identity 库的审计表会多出测试造的操作，而那是给人查的账。
func cleanupAudit(t *testing.T, pool *pgxpool.Pool, targetID string) {
	t.Helper()
	cleanupFixture(t, pool,
		`DELETE FROM message_outbox WHERE event_type = $1 AND convert_from(payload, 'UTF8') LIKE $2`,
		audit.EventType, "%"+targetID+"%")
}

func TestCreateManufacturerRecordsAudit(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()

	m := &model.Manufacturer{
		ID: uuid.NewString(), Code: "it-" + uuid.NewString(),
		Name: "集成测试厂商", ContactName: "张三", ContactPhone: "13800000000",
	}
	cleanupFixture(t, pool, `DELETE FROM manufacturers WHERE id = $1`, m.ID)
	cleanupAudit(t, pool, m.ID)

	created, err := repo.CreateManufacturer(ctx, m)
	if err != nil {
		t.Fatalf("create manufacturer: %v", err)
	}

	// 没传 status，落库的应该是列默认的 active——新建即启用是写在 INSERT 里的决定，
	// 不是调用方的责任。
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM manufacturers WHERE id = $1`, m.ID).Scan(&status); err != nil {
		t.Fatalf("read back manufacturer: %v", err)
	}
	if status != "active" {
		t.Fatalf("status = %q, want active", status)
	}

	// 回来的必须是库里那一行：时间戳由数据库给，服务层拼的那个只是本地时钟。
	// updated_at 若是零值（0001 年），前端在创建响应里渲染「最后更新」就会显示错的东西。
	if created.Status != "active" {
		t.Fatalf("created.Status = %q, want active read back from the row", created.Status)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created timestamps = %v/%v, want them read back from the database",
			created.CreatedAt, created.UpdatedAt)
	}

	if count := countAudit(t, pool, m.ID); count != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", count)
	}
	entry := latestAudit(t, pool, m.ID)
	if entry.Module != "manufacturers" || entry.Action != "create" {
		t.Fatalf("audit module/action = %q/%q, want manufacturers/create", entry.Module, entry.Action)
	}
	if entry.TargetID != m.ID || entry.TargetName != m.Name {
		t.Fatalf("audit target = %q/%q, want %q/%q", entry.TargetID, entry.TargetName, m.ID, m.Name)
	}
	if entry.Result != "success" {
		t.Fatalf("audit result = %q, want success", entry.Result)
	}
	// 新增没有前值，before_data 必须是空的——写个 {} 进去会让读的人以为改动前
	// 是一组空字段。
	if len(entry.Before) != 0 {
		t.Fatalf("before_data = %s, want empty on create", entry.Before)
	}
	var after manufacturerSnapshot
	if err := json.Unmarshal(entry.After, &after); err != nil {
		t.Fatalf("decode after_data: %v", err)
	}
	if after.Code != m.Code || after.ContactPhone != "13800000000" {
		t.Fatalf("after_data = %+v, want the created values", after)
	}
}

func TestManufacturerCodeIsImmutable(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()

	m := &model.Manufacturer{ID: uuid.NewString(), Code: "it-" + uuid.NewString(), Name: "改名前"}
	cleanupFixture(t, pool, `DELETE FROM manufacturers WHERE id = $1`, m.ID)
	cleanupAudit(t, pool, m.ID)
	if _, err := repo.CreateManufacturer(ctx, m); err != nil {
		t.Fatalf("create manufacturer: %v", err)
	}

	// 改名的接口只改展示字段。就算调用方把 Code 填成别的，也不该在库里生效——
	// 它根本不在 UPDATE 的 SET 里。
	updated := &model.Manufacturer{ID: m.ID, Code: "it-should-not-apply", Name: "改名后"}
	if err := repo.UpdateManufacturer(ctx, updated); err != nil {
		t.Fatalf("update manufacturer: %v", err)
	}
	var code, name string
	if err := pool.QueryRow(ctx, `SELECT code, name FROM manufacturers WHERE id = $1`, m.ID).Scan(&code, &name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if code != m.Code {
		t.Fatalf("code = %q, want %q unchanged", code, m.Code)
	}
	if name != "改名后" {
		t.Fatalf("name = %q, want 改名后", name)
	}

	// 数据库那个触发器也在，绕过服务层直接改同样要被拦下。
	if _, err := pool.Exec(ctx, `UPDATE manufacturers SET code = 'it-direct' WHERE id = $1`, m.ID); err == nil {
		t.Fatalf("direct UPDATE of code succeeded, want the prevent_manufacturer_code_change trigger to reject it")
	}

	if count := countAudit(t, pool, m.ID); count != 2 {
		t.Fatalf("audit rows = %d, want 2 (create + update)", count)
	}
}

func TestCreateManufacturerRejectsDuplicateCode(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()

	code := "it-" + uuid.NewString()
	first := &model.Manufacturer{ID: uuid.NewString(), Code: code, Name: "第一个"}
	second := &model.Manufacturer{ID: uuid.NewString(), Code: code, Name: "第二个"}
	cleanupFixture(t, pool, `DELETE FROM manufacturers WHERE id = ANY($1)`, []string{first.ID, second.ID})
	cleanupAudit(t, pool, first.ID)
	cleanupAudit(t, pool, second.ID)

	if _, err := repo.CreateManufacturer(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	_, err := repo.CreateManufacturer(ctx, second)
	if !errors.Is(err, ErrManufacturerCodeTaken) {
		t.Fatalf("err = %v, want ErrManufacturerCodeTaken", err)
	}
	// 失败的那次不能留下审计：事件说「新增了厂商」，而厂商并不存在。
	if count := countAudit(t, pool, second.ID); count != 0 {
		t.Fatalf("audit rows for the rejected create = %d, want 0", count)
	}
}

func TestUpdateManufacturerReportsMissingAsNotFound(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)

	err := repo.UpdateManufacturer(context.Background(),
		&model.Manufacturer{ID: uuid.NewString(), Name: "不存在"})
	if !errors.Is(err, ErrManufacturerNotFound) {
		t.Fatalf("err = %v, want ErrManufacturerNotFound", err)
	}
}

func TestCreateDeviceRecordsAuditAndIgnoresVendorColumns(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)

	device := &model.Device{
		ID: uuid.NewString(), SerialUnique: "it-" + uuid.NewString(),
		DeviceName: "集成测试设备", ManufacturerID: manufacturerID,
		QrcodeType: "miniprogram", PickupPassword: "8321", ShowVip: true,
	}
	cleanupFixture(t, pool, `DELETE FROM devices WHERE id = $1`, device.ID)
	cleanupAudit(t, pool, device.ID)

	if _, err := repo.CreateDevice(ctx, device); err != nil {
		t.Fatalf("create device: %v", err)
	}

	var status string
	var balance int64
	var vendorOnline *bool
	if err := pool.QueryRow(ctx, `SELECT status, coffee_balance, vendor_online FROM devices WHERE id = $1`,
		device.ID).Scan(&status, &balance, &vendorOnline); err != nil {
		t.Fatalf("read back device: %v", err)
	}
	if status != "active" {
		t.Fatalf("status = %q, want active", status)
	}
	if balance != 0 {
		t.Fatalf("coffee_balance = %d, want 0 on create", balance)
	}
	// 没同步过的设备 vendor_online 是 NULL 而不是 false——新建走的就是这条。
	if vendorOnline != nil {
		t.Fatalf("vendor_online = %v, want NULL on a freshly created device", *vendorOnline)
	}

	entry := latestAudit(t, pool, device.ID)
	if entry.Module != "devices" || entry.Action != "create" {
		t.Fatalf("audit module/action = %q/%q, want devices/create", entry.Module, entry.Action)
	}
	// 提货码是凭据，不进审计载荷，但要能看出「设了」。
	var after map[string]any
	if err := json.Unmarshal(entry.After, &after); err != nil {
		t.Fatalf("decode after_data: %v", err)
	}
	if _, present := after["pickup_password"]; present {
		t.Fatalf("after_data carries pickup_password: %s", entry.After)
	}
	if after["pickup_password_set"] != true {
		t.Fatalf("pickup_password_set = %v, want true", after["pickup_password_set"])
	}
	// 厂商同步写入的列不该混进快照：它们不由后台改动。
	if _, present := after["vendor_online"]; present {
		t.Fatalf("after_data carries vendor_online: %s", entry.After)
	}
}

func TestCreateDeviceRejectsUnknownManufacturer(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()

	device := &model.Device{
		ID: uuid.NewString(), SerialUnique: "it-" + uuid.NewString(),
		DeviceName: "挂在不存在厂商上的设备", ManufacturerID: uuid.NewString(),
		QrcodeType: "miniprogram",
	}
	cleanupFixture(t, pool, `DELETE FROM devices WHERE id = $1`, device.ID)
	cleanupAudit(t, pool, device.ID)

	_, err := repo.CreateDevice(ctx, device)
	// 引用一个不存在的厂商是请求字段的问题（400），不是「目标不存在」（404），
	// 所以是 ErrManufacturerMissing 而不是 ErrManufacturerNotFound。这条映射靠的是
	// 外键约束名，名字写错就会静默落回 500。
	if !errors.Is(err, ErrManufacturerMissing) {
		t.Fatalf("err = %v, want ErrManufacturerMissing", err)
	}
	if count := countAudit(t, pool, device.ID); count != 0 {
		t.Fatalf("audit rows for the rejected create = %d, want 0", count)
	}
}

func TestCreateDeviceRejectsDuplicateSerial(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)

	serial := "it-" + uuid.NewString()
	first := &model.Device{ID: uuid.NewString(), SerialUnique: serial, DeviceName: "第一台",
		ManufacturerID: manufacturerID, QrcodeType: "miniprogram"}
	second := &model.Device{ID: uuid.NewString(), SerialUnique: serial, DeviceName: "第二台",
		ManufacturerID: manufacturerID, QrcodeType: "miniprogram"}
	cleanupFixture(t, pool, `DELETE FROM devices WHERE id = ANY($1)`, []string{first.ID, second.ID})
	cleanupAudit(t, pool, first.ID)
	cleanupAudit(t, pool, second.ID)

	if _, err := repo.CreateDevice(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := repo.CreateDevice(ctx, second); !errors.Is(err, ErrSerialTaken) {
		t.Fatalf("err = %v, want ErrSerialTaken", err)
	}
}

// TestUpdateDeviceKeepsVendorAndMoneyColumns 验的是字段分类那条边界：后台的编辑
// 接口碰不到余额，也碰不到厂商同步写入的列。调用方在结构体里把它们填上也没用——
// 它们不在 UPDATE 的 SET 里。
func TestUpdateDeviceKeepsVendorAndMoneyColumns(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	cleanupAudit(t, pool, deviceID)

	// 先把余额和同步列摆成一组可辨认的值，再走编辑接口。
	if _, err := pool.Exec(ctx, `UPDATE devices SET coffee_balance = 5000,
		vendor_online = true, version_number = '1.2.3', android_version = 'android-11'
		WHERE id = $1`, deviceID); err != nil {
		t.Fatalf("seed vendor columns: %v", err)
	}

	device := &model.Device{
		ID: deviceID, SerialUnique: "it-" + deviceID, DeviceName: "改过名字的设备",
		ManufacturerID: manufacturerID, QrcodeType: "miniprogram",
		// 这三个是「不该被这次编辑写进去」的值。
		CoffeeBalance: 999999, VersionNumber: "9.9.9", AndroidVersion: "android-99",
	}
	if err := repo.UpdateDevice(ctx, device, nil); err != nil {
		t.Fatalf("update device: %v", err)
	}

	var balance int64
	var version, android string
	var vendorOnline *bool
	if err := pool.QueryRow(ctx, `SELECT coffee_balance, version_number, android_version, vendor_online
		FROM devices WHERE id = $1`, deviceID).Scan(&balance, &version, &android, &vendorOnline); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if balance != 5000 {
		t.Fatalf("coffee_balance = %d, want 5000 untouched by the edit endpoint", balance)
	}
	if version != "1.2.3" || android != "android-11" {
		t.Fatalf("vendor columns = %q/%q, want 1.2.3/android-11 untouched", version, android)
	}
	if vendorOnline == nil || !*vendorOnline {
		t.Fatalf("vendor_online = %v, want a known true untouched", vendorOnline)
	}

	var name string
	if err := pool.QueryRow(ctx, `SELECT device_name FROM devices WHERE id = $1`, deviceID).Scan(&name); err != nil {
		t.Fatalf("read back name: %v", err)
	}
	if name != "改过名字的设备" {
		t.Fatalf("device_name = %q, want the edit to have applied", name)
	}
}

// TestDeviceStoreIDSeparatesMissingFromUnset 盯的是「这次有没有换点位」的判据来源。
//
// 判据必须直接来自库里那一行（入参里只有「要写成什么」），而三种情况要分得开：挂了
// 某个点位 / 没挂点位 / 设备不存在。后两者塌成一个空串的话，一台还没挂点位的设备去挂
// 一个停用的点位，就会被读成「和原来一样」而跳过校验——正是这条改动要拦的那件事。
func TestDeviceStoreIDSeparatesMissingFromUnset(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	storeID := uuid.NewString()

	attached := deviceFixture(t, pool, manufacturerID, "active", nil, &storeID)
	got, err := repo.DeviceStoreID(ctx, attached)
	if err != nil {
		t.Fatalf("DeviceStoreID: %v", err)
	}
	if got == nil || *got != storeID {
		t.Fatalf("DeviceStoreID = %v, want %q", got, storeID)
	}

	detached := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	got, err = repo.DeviceStoreID(ctx, detached)
	if err != nil {
		t.Fatalf("DeviceStoreID: %v", err)
	}
	if got != nil {
		t.Fatalf("没挂点位的设备读出 %q, want nil", *got)
	}

	// 设备不存在要报错，不能也回 nil——那会和「没挂点位」混成一条路，编辑一台不存在
	// 的设备看起来就像编辑一台没挂点位的设备。
	if _, err := repo.DeviceStoreID(ctx, uuid.NewString()); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("DeviceStoreID(不存在的设备) = %v, want ErrDeviceNotFound", err)
	}
}

// TestUpdateDeviceClearsPaymentMethodWhenNotRegular 盯的是那条引用了两列的 CHECK：
// qrcode_type <> 'regular' OR regular_qrcode_payment_method IS NOT NULL。切回小程序
// 时把旧的支付方式留着，下一次有人切到 regular 就会带着一个没人记得的值通过校验。
func TestUpdateDeviceClearsPaymentMethodWhenNotRegular(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	cleanupAudit(t, pool, deviceID)

	paymentMethod := "youlian"
	regular := &model.Device{
		ID: deviceID, SerialUnique: "it-" + deviceID, DeviceName: "扫码设备",
		ManufacturerID: manufacturerID, QrcodeType: "regular",
		RegularQrcodePaymentMethod: &paymentMethod,
	}
	if err := repo.UpdateDevice(ctx, regular, nil); err != nil {
		t.Fatalf("switch to regular: %v", err)
	}

	// 切回小程序，调用方还带着那个旧的支付方式。
	back := &model.Device{
		ID: deviceID, SerialUnique: "it-" + deviceID, DeviceName: "扫码设备",
		ManufacturerID: manufacturerID, QrcodeType: "miniprogram",
		RegularQrcodePaymentMethod: &paymentMethod,
	}
	if err := repo.UpdateDevice(ctx, back, nil); err != nil {
		t.Fatalf("switch back to miniprogram: %v", err)
	}

	var qrcodeType string
	var stored *string
	if err := pool.QueryRow(ctx, `SELECT qrcode_type, regular_qrcode_payment_method
		FROM devices WHERE id = $1`, deviceID).Scan(&qrcodeType, &stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if qrcodeType != "miniprogram" {
		t.Fatalf("qrcode_type = %q, want miniprogram", qrcodeType)
	}
	if stored != nil {
		t.Fatalf("regular_qrcode_payment_method = %q, want NULL after switching away from regular", *stored)
	}
}

// TestUpdateDeviceKeepsPickupPasswordWhenBlank 盯的是「编辑时这栏留空」这条路。
//
// 静态验证码不回填到编辑表单，所以一次普通的改名会带着空的 PickupPassword 过来。
// 若按值直接落库，店员正在用的码就被抹掉了，而且要等到有人真去机器上提货才会发现。
// 传 nil 时那一列必须原样不动；传了值才覆盖。
func TestUpdateDeviceKeepsPickupPasswordWhenBlank(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	cleanupAudit(t, pool, deviceID)

	if _, err := pool.Exec(ctx, `UPDATE devices SET pickup_password = '8321' WHERE id = $1`, deviceID); err != nil {
		t.Fatalf("seed pickup password: %v", err)
	}

	edit := func(name string, password *string) {
		t.Helper()
		if err := repo.UpdateDevice(ctx, &model.Device{
			ID: deviceID, SerialUnique: "it-" + deviceID, DeviceName: name,
			ManufacturerID: manufacturerID, QrcodeType: "miniprogram",
		}, password); err != nil {
			t.Fatalf("update device to %q: %v", name, err)
		}
	}

	edit("只改了名字", nil)

	var stored string
	if err := pool.QueryRow(ctx, `SELECT pickup_password FROM devices WHERE id = $1`, deviceID).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "8321" {
		t.Fatalf("pickup_password = %q, want 8321 kept when the edit left it blank", stored)
	}

	// 传了值才覆盖。
	replaced := "9999"
	edit("连码一起换", &replaced)
	if err := pool.QueryRow(ctx, `SELECT pickup_password FROM devices WHERE id = $1`, deviceID).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "9999" {
		t.Fatalf("pickup_password = %q, want 9999 after an explicit change", stored)
	}

	// 审计记的是「有没有设」，不是值本身：载荷里不能出现这串码。
	entry := latestAudit(t, pool, deviceID)
	if strings.Contains(string(entry.After), "8321") || strings.Contains(string(entry.After), "9999") {
		t.Fatalf("after_data leaked the pickup password: %s", entry.After)
	}
	var after deviceSnapshot
	if err := json.Unmarshal(entry.After, &after); err != nil {
		t.Fatalf("decode after_data: %v", err)
	}
	if !after.PickupPasswordSet {
		t.Fatalf("after_data.pickup_password_set = false, want true")
	}
}

func TestCreateDrinkRecordsAudit(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)

	drink := &model.Drink{
		ID: uuid.NewString(), ManufacturerID: manufacturerID,
		OriginID: "it-origin-" + uuid.NewString(), ProductNum: "P001",
		ProductName: "集成测试拿铁", Price: 1800, VipPrice: 1500, PickupCodePrice: 1200,
	}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drink.ID)
	cleanupAudit(t, pool, drink.ID)

	if _, err := repo.CreateDrink(ctx, drink); err != nil {
		t.Fatalf("create drink: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM drinks WHERE id = $1`, drink.ID).Scan(&status); err != nil {
		t.Fatalf("read back drink: %v", err)
	}
	if status != "on_shelf" {
		t.Fatalf("status = %q, want on_shelf from the column default", status)
	}

	entry := latestAudit(t, pool, drink.ID)
	if entry.Module != "drinks" || entry.Action != "create" {
		t.Fatalf("audit module/action = %q/%q, want drinks/create", entry.Module, entry.Action)
	}
	var after drinkSnapshot
	if err := json.Unmarshal(entry.After, &after); err != nil {
		t.Fatalf("decode after_data: %v", err)
	}
	if after.Price != 1800 || after.VipPrice != 1500 || after.PickupCodePrice != 1200 {
		t.Fatalf("after_data prices = %d/%d/%d, want 1800/1500/1200", after.Price, after.VipPrice, after.PickupCodePrice)
	}
}

// TestUpdateDrinkKeepsNaturalKey 验的是：编辑接口改不动厂商同步的自然键。入参里没有
// 这两个字段，调用方硬填也不进 SET——改了它就再也匹配不上厂商侧同一款饮品。
func TestUpdateDrinkKeepsNaturalKey(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)

	originID := "it-origin-" + uuid.NewString()
	drink := &model.Drink{ID: uuid.NewString(), ManufacturerID: manufacturerID,
		OriginID: originID, ProductName: "原名", Price: 1000}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drink.ID)
	cleanupAudit(t, pool, drink.ID)
	if _, err := repo.CreateDrink(ctx, drink); err != nil {
		t.Fatalf("create drink: %v", err)
	}

	updated := &model.Drink{ID: drink.ID, ManufacturerID: uuid.NewString(),
		OriginID: "it-hijacked", ProductName: "新名", Price: 2000}
	if err := repo.UpdateDrink(ctx, updated); err != nil {
		t.Fatalf("update drink: %v", err)
	}

	var storedManufacturer, storedOrigin, name string
	if err := pool.QueryRow(ctx, `SELECT manufacturer_id::text, origin_id, product_name
		FROM drinks WHERE id = $1`, drink.ID).Scan(&storedManufacturer, &storedOrigin, &name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if storedManufacturer != manufacturerID || storedOrigin != originID {
		t.Fatalf("natural key = %q/%q, want %q/%q unchanged", storedManufacturer, storedOrigin, manufacturerID, originID)
	}
	if name != "新名" {
		t.Fatalf("product_name = %q, want the edit to have applied", name)
	}
}

// TestOriginIndexOnlyBindsWithinOneDevice 守住判重索引的两个「不生效」面。它们不是漏
// 网，是索引定义本身的两条边界，改动索引写法时最容易顺手丢掉：
//
//   - device_id 为 NULL 的行互不相撞：唯一索引把 NULL 当作彼此不同，所以没有设备的
//     历史行（迁移 003 之前建的那批）可以同 origin 并存。这是可接受的——设备维度的读
//     接口根本看不到它们，而新写入路径已经被服务层挡在 ErrDrinkDeviceRequired 上。
//   - origin_id 为空的行也不相撞：索引只收 origin_id 非空的行（部分索引），后台手工
//     新建的饮品全是空 origin，同一台设备上可以有任意多行。
//
// 「同一台设备上的同款被拒」在 TestDrinkOriginIsUniquePerDeviceNotPerManufacturer 里。
func TestOriginIndexOnlyBindsWithinOneDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)

	originID := "it-origin-" + uuid.NewString()
	first := &model.Drink{ID: uuid.NewString(), ManufacturerID: manufacturerID,
		OriginID: originID, ProductName: "第一款", Price: 1000}
	second := &model.Drink{ID: uuid.NewString(), ManufacturerID: manufacturerID,
		OriginID: originID, ProductName: "第二款", Price: 1000}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = ANY($1)`, []string{first.ID, second.ID})
	cleanupAudit(t, pool, first.ID)
	cleanupAudit(t, pool, second.ID)

	if _, err := repo.CreateDrink(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := repo.CreateDrink(ctx, second); err != nil {
		t.Fatalf("create a second device-less drink with the same origin: %v（device_id 为 NULL 的行不该被唯一索引拦住）", err)
	}

	// 同一台设备上的两行手工饮品：origin 都是空串，必须都放行。
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	manual := &model.Drink{ID: uuid.NewString(), DeviceID: &deviceID, ManufacturerID: manufacturerID,
		OriginID: "", ProductName: "手工饮品一", Price: 1000}
	another := &model.Drink{ID: uuid.NewString(), DeviceID: &deviceID, ManufacturerID: manufacturerID,
		OriginID: "", ProductName: "手工饮品二", Price: 1200}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = ANY($1)`, []string{manual.ID, another.ID})
	cleanupAudit(t, pool, manual.ID)
	cleanupAudit(t, pool, another.ID)
	for _, d := range []*model.Drink{manual, another} {
		if _, err := repo.CreateDrink(ctx, d); err != nil {
			t.Fatalf("create %s with an empty origin_id: %v", d.ProductName, err)
		}
	}
}

// ============================================================
// 余额调整（方案 11.6 L893 必审清单）
// ============================================================

// TestAdjustBalanceRecordsLedgerAndAudit 是这个模块里最重要的一个用例：余额、流水、
// 审计三样必须在同一个事务里一起落。少任何一样都是一种对不上的状态——钱变了没有
// 流水，或者流水记了审计没记。
func TestAdjustBalanceRecordsLedgerAndAudit(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)
	cleanupAudit(t, pool, deviceID)

	requestID := "it-req-" + uuid.NewString()
	balance, err := repo.AdjustBalance(ctx, BalanceAdjustment{
		DeviceID: deviceID, Amount: 20000, RequestID: requestID,
		Remark: "开业充值", Operator: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("adjust balance: %v", err)
	}
	if balance != 20000 {
		t.Fatalf("returned balance = %d, want 20000", balance)
	}

	// 再做一次负数调整，把「先加后扣」这条真实路径也走一遍。
	operator := uuid.NewString()
	balance, err = repo.AdjustBalance(ctx, BalanceAdjustment{
		DeviceID: deviceID, Amount: -5000, RequestID: "it-req-" + uuid.NewString(),
		Remark: "误充退回", Operator: operator,
	})
	if err != nil {
		t.Fatalf("second adjust: %v", err)
	}
	if balance != 15000 {
		t.Fatalf("balance after the second adjust = %d, want 15000", balance)
	}

	var stored int64
	if err := pool.QueryRow(ctx, `SELECT coffee_balance FROM devices WHERE id = $1`, deviceID).Scan(&stored); err != nil {
		t.Fatalf("read back balance: %v", err)
	}
	if stored != 15000 {
		t.Fatalf("devices.coffee_balance = %d, want 15000", stored)
	}

	// 流水两行，type 都是 adjust，balance_after 是当时的余额（不是增量）。
	var ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_balance_ledger
		WHERE device_id = $1 AND type = 'adjust'`, deviceID).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledgerCount != 2 {
		t.Fatalf("ledger rows = %d, want 2", ledgerCount)
	}
	var ledgerBalance int64
	var ledgerAmount int64
	var ledgerOperator *string
	var ledgerRemark string
	if err := pool.QueryRow(ctx, `SELECT balance_after, amount, operator_id::text, remark
		FROM device_balance_ledger WHERE device_id = $1 AND request_id = $2`,
		deviceID, requestID).Scan(&ledgerBalance, &ledgerAmount, &ledgerOperator, &ledgerRemark); err != nil {
		t.Fatalf("read back ledger row: %v", err)
	}
	if ledgerBalance != 20000 || ledgerAmount != 20000 {
		t.Fatalf("ledger amount/balance_after = %d/%d, want 20000/20000", ledgerAmount, ledgerBalance)
	}
	if ledgerOperator == nil {
		t.Fatalf("operator_id is NULL, want the operator id")
	}
	if ledgerRemark != "开业充值" {
		t.Fatalf("remark = %q, want 开业充值", ledgerRemark)
	}

	// 审计：两行，且 before/after 就是方案要的 before-image。缺了 before，一次改错的
	// 余额在库里只留下「现在是 X」，无从回溯它是从哪改过来的。
	if count := countAudit(t, pool, deviceID); count != 2 {
		t.Fatalf("audit rows = %d, want 2", count)
	}
	entry := latestAudit(t, pool, deviceID)
	if entry.Module != "device_balance" || entry.Action != "adjust" {
		t.Fatalf("audit module/action = %q/%q, want device_balance/adjust", entry.Module, entry.Action)
	}
	if entry.ActorID != operator {
		t.Fatalf("audit actor = %q, want the operator id %q", entry.ActorID, operator)
	}
	var before, after balanceSnapshot
	if err := json.Unmarshal(entry.Before, &before); err != nil {
		t.Fatalf("decode before_data: %v", err)
	}
	if err := json.Unmarshal(entry.After, &after); err != nil {
		t.Fatalf("decode after_data: %v", err)
	}
	if before.CoffeeBalance != 20000 {
		t.Fatalf("before_data.coffee_balance = %d, want 20000", before.CoffeeBalance)
	}
	if after.CoffeeBalance != 15000 || after.Amount != -5000 {
		t.Fatalf("after_data = %+v, want balance 15000 / amount -5000", after)
	}
}

func TestAdjustBalanceRejectsDuplicateRequestID(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)
	cleanupAudit(t, pool, deviceID)

	requestID := "it-req-" + uuid.NewString()
	in := BalanceAdjustment{DeviceID: deviceID, Amount: 1000, RequestID: requestID, Operator: uuid.NewString()}
	if _, err := repo.AdjustBalance(ctx, in); err != nil {
		t.Fatalf("first adjust: %v", err)
	}
	// 管理员的点击超时重发：同一个 requestId 又来一次。幂等键必须挡住它，否则一次
	// 点击会变成两次加钱。
	if _, err := repo.AdjustBalance(ctx, in); !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("err = %v, want ErrDuplicateRequest", err)
	}

	var balance int64
	if err := pool.QueryRow(ctx, `SELECT coffee_balance FROM devices WHERE id = $1`, deviceID).Scan(&balance); err != nil {
		t.Fatalf("read back balance: %v", err)
	}
	if balance != 1000 {
		t.Fatalf("coffee_balance = %d, want 1000 (the duplicate must not have applied)", balance)
	}
	// 被拒的那次整体回滚了，所以余额没变、流水一行、审计一行。
	if count := countAudit(t, pool, deviceID); count != 1 {
		t.Fatalf("audit rows = %d, want 1", count)
	}
}

func TestAdjustBalanceRejectsNegativeResult(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)
	cleanupAudit(t, pool, deviceID)

	// 余额是 0，扣 1 分就不够。这条要在服务层拦成 400，而不是让数据库的
	// CHECK (coffee_balance >= 0) 炸出一个 500。
	if _, err := repo.AdjustBalance(ctx, BalanceAdjustment{
		DeviceID: deviceID, Amount: -1, RequestID: "it-req-" + uuid.NewString(),
	}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}

	var balance int64
	var ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT coffee_balance FROM devices WHERE id = $1`, deviceID).Scan(&balance); err != nil {
		t.Fatalf("read back balance: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_balance_ledger WHERE device_id = $1`, deviceID).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if balance != 0 || ledgerCount != 0 {
		t.Fatalf("balance/ledger = %d/%d, want 0/0 after a rejected adjustment", balance, ledgerCount)
	}
	if count := countAudit(t, pool, deviceID); count != 0 {
		t.Fatalf("audit rows = %d, want 0 for a rejected adjustment", count)
	}
}

// TestAdjustBalanceRejectsOverflow 验的是那道先于加法的溢出检查。不挡的话相加会回绕
// 成负数，最后以「余额为负」的 CHECK 违反收场，报错指向的原因和真实原因不同。
func TestAdjustBalanceRejectsOverflow(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)
	cleanupAudit(t, pool, deviceID)

	if _, err := pool.Exec(ctx, `UPDATE devices SET coffee_balance = $2 WHERE id = $1`,
		deviceID, int64(1)<<62); err != nil {
		t.Fatalf("seed balance: %v", err)
	}
	if _, err := repo.AdjustBalance(ctx, BalanceAdjustment{
		DeviceID: deviceID, Amount: 1 << 62, RequestID: "it-req-" + uuid.NewString(),
	}); !errors.Is(err, ErrBalanceOutOfRange) {
		t.Fatalf("err = %v, want ErrBalanceOutOfRange", err)
	}

	var balance int64
	if err := pool.QueryRow(ctx, `SELECT coffee_balance FROM devices WHERE id = $1`, deviceID).Scan(&balance); err != nil {
		t.Fatalf("read back balance: %v", err)
	}
	if balance != int64(1)<<62 {
		t.Fatalf("coffee_balance = %d, want the seeded value untouched", balance)
	}
}

func TestAdjustBalanceReportsMissingDeviceAsNotFound(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)

	_, err := repo.AdjustBalance(context.Background(), BalanceAdjustment{
		DeviceID: uuid.NewString(), Amount: 1000, RequestID: "it-req-" + uuid.NewString(),
	})
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("err = %v, want ErrDeviceNotFound", err)
	}
}

// TestAuditActorComesFromTheRequestIdentity 验的是「谁做的」这件事在两条路径上都
// 落得下来。后台的写接口都在鉴权中间件后面，中间件把身份放进了请求 context，所以
// 不带 Operator 的那几个写方法靠 Recorder 回落就能拿到操作人；余额调整则显式传，
// 两种都得有值。
func TestAuditActorComesFromTheRequestIdentity(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)
	cleanupAudit(t, pool, deviceID)

	actorID := uuid.NewString()
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: actorID})

	// 不传 Operator：审计的操作人只能来自 context。
	if _, err := repo.AdjustBalance(ctx, BalanceAdjustment{
		DeviceID: deviceID, Amount: 1000, RequestID: "it-req-" + uuid.NewString(),
	}); err != nil {
		t.Fatalf("adjust balance: %v", err)
	}
	if entry := latestAudit(t, pool, deviceID); entry.ActorID != actorID {
		t.Fatalf("audit actor = %q, want %q from the request identity", entry.ActorID, actorID)
	}

	// 厂商/设备/饮品那几个写方法没有 Operator 入参，同理。
	m := &model.Manufacturer{ID: uuid.NewString(), Code: "it-" + uuid.NewString(), Name: "带操作人"}
	cleanupFixture(t, pool, `DELETE FROM manufacturers WHERE id = $1`, m.ID)
	cleanupAudit(t, pool, m.ID)
	if _, err := repo.CreateManufacturer(ctx, m); err != nil {
		t.Fatalf("create manufacturer: %v", err)
	}
	if entry := latestAudit(t, pool, m.ID); entry.ActorID != actorID {
		t.Fatalf("manufacturer audit actor = %q, want %q from the request identity", entry.ActorID, actorID)
	}
}

// TestNoopRecorderStillWritesBusinessRows 确认审计开关关掉时写路径照常工作：
// Recorder 是可替换的，业务写入不该依赖审计是否开着。
func TestNoopRecorderStillWritesBusinessRows(t *testing.T) {
	pool := integrationPool(t)
	repo := NewAdminRepository(pool, nil)
	ctx := context.Background()

	m := &model.Manufacturer{ID: uuid.NewString(), Code: "it-" + uuid.NewString(), Name: "无审计"}
	cleanupFixture(t, pool, `DELETE FROM manufacturers WHERE id = $1`, m.ID)
	cleanupAudit(t, pool, m.ID)

	if _, err := repo.CreateManufacturer(ctx, m); err != nil {
		t.Fatalf("create manufacturer with auditing off: %v", err)
	}
	if count := countAudit(t, pool, m.ID); count != 0 {
		t.Fatalf("audit rows = %d, want 0 with a nil recorder", count)
	}

	// 时间戳得真的落下来，不能是零值。零值时间是 0001 年，读出来会是很明显的错。
	var createdAt time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM manufacturers WHERE id = $1`, m.ID).Scan(&createdAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if createdAt.IsZero() {
		t.Fatalf("created_at is the zero time")
	}
}

// ============================================================
// 饮品行上的设备
// ============================================================

// drinkFixture 造一款饮品并注册清理，返回它的 id。deviceID 为空串时写 NULL——库里
// 确实存在还没挂设备的行（003 是加列迁移），「未分配设备」那条路径要有现场。
func drinkFixture(t *testing.T, pool *pgxpool.Pool, manufacturerID, deviceID, name string) string {
	t.Helper()
	drinkID := uuid.NewString()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO drinks(id, manufacturer_id, device_id, product_name, price)
		 VALUES($1, $2, NULLIF($3, '')::uuid, $4, 1800)`,
		drinkID, manufacturerID, deviceID, name); err != nil {
		t.Fatalf("insert drink: %v", err)
	}
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drinkID)
	return drinkID
}

// readDrinkDevice 读回这一行挂在哪台设备上。空串表示 NULL。
func readDrinkDevice(t *testing.T, pool *pgxpool.Pool, drinkID string) string {
	t.Helper()
	var deviceID string
	err := pool.QueryRow(context.Background(),
		`SELECT coalesce(device_id::text, '') FROM drinks WHERE id = $1`, drinkID).Scan(&deviceID)
	if err != nil {
		t.Fatalf("read drink: %v", err)
	}
	return deviceID
}

// TestCreateDrinkStoresDeviceAndRecordsAudit 盯的是这次改动的核心：设备 ID 就存在
// 饮品行上，不是另建一行关系。审计里也要看得见它——「这杯是哪台机器上的」是这一行
// 最有信息量的字段。
func TestCreateDrinkStoresDeviceAndRecordsAudit(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	drinkID := uuid.NewString()
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE id = $1`, drinkID)
	cleanupAudit(t, pool, drinkID)

	d := &model.Drink{
		ID: drinkID, DeviceID: &deviceID, ManufacturerID: manufacturerID,
		ProductName: "集成测试美式", Price: 1800, Sort: 3,
	}
	created, err := repo.CreateDrink(ctx, d)
	if err != nil {
		t.Fatalf("create drink: %v", err)
	}
	// 回的是库里读回来的那一行：扫描列清单里少一个 device_id 就会让这里拿到 nil，
	// 而接口那头看起来只是「新建完设备没了」。
	if created.DeviceID == nil || *created.DeviceID != deviceID {
		t.Fatalf("created.DeviceID = %v, want %s", created.DeviceID, deviceID)
	}
	if got := readDrinkDevice(t, pool, drinkID); got != deviceID {
		t.Fatalf("devices in db = %q, want %q", got, deviceID)
	}

	if count := countAudit(t, pool, drinkID); count != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", count)
	}
	entry := latestAudit(t, pool, drinkID)
	if entry.Module != "drinks" || entry.Action != "create" || entry.TargetType != "drink" {
		t.Fatalf("audit = %q/%q/%q, want drinks/create/drink", entry.Module, entry.Action, entry.TargetType)
	}
	if len(entry.Before) != 0 {
		t.Fatalf("before_data = %s, want empty on create", entry.Before)
	}
	var after drinkSnapshot
	if err := json.Unmarshal(entry.After, &after); err != nil {
		t.Fatalf("decode after_data: %v", err)
	}
	if after.DeviceID == nil || *after.DeviceID != deviceID {
		t.Fatalf("after_data device = %v, want %s", after.DeviceID, deviceID)
	}
}

// TestUpdateDrinkMovesAndDetachesDevice 盯的是编辑这一行的两个方向：换到另一台设备、
// 以及摘下来。before/after 都得带着改之前是哪台——只看 after 一个值的话，「从哪台
// 挪过来的」读不出来，而这正是这种事后来要查的东西。
func TestUpdateDrinkMovesAndDetachesDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	first := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	second := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	drinkID := drinkFixture(t, pool, manufacturerID, first, "集成测试卡布")
	cleanupAudit(t, pool, drinkID)

	// 换设备。
	if err := repo.UpdateDrink(ctx, &model.Drink{
		ID: drinkID, DeviceID: &second, ProductName: "集成测试卡布", Price: 1800, Sort: 1,
	}); err != nil {
		t.Fatalf("move drink: %v", err)
	}
	if got := readDrinkDevice(t, pool, drinkID); got != second {
		t.Fatalf("device after move = %q, want %q", got, second)
	}

	// 摘下来：device_id 变成 NULL，这一行留在库里（不是删行）。
	if err := repo.UpdateDrink(ctx, &model.Drink{
		ID: drinkID, ProductName: "集成测试卡布", Price: 1800, Sort: 1,
	}); err != nil {
		t.Fatalf("detach drink: %v", err)
	}
	if got := readDrinkDevice(t, pool, drinkID); got != "" {
		t.Fatalf("device after detach = %q, want NULL", got)
	}

	if count := countAudit(t, pool, drinkID); count != 2 {
		t.Fatalf("audit rows = %d, want 2", count)
	}
	entry := latestAudit(t, pool, drinkID)
	var before, after drinkSnapshot
	if err := json.Unmarshal(entry.Before, &before); err != nil {
		t.Fatalf("decode before_data: %v", err)
	}
	if err := json.Unmarshal(entry.After, &after); err != nil {
		t.Fatalf("decode after_data: %v", err)
	}
	if before.DeviceID == nil || *before.DeviceID != second {
		t.Fatalf("before_data device = %v, want %s", before.DeviceID, second)
	}
	if after.DeviceID != nil {
		t.Fatalf("after_data device = %v, want nil（摘下来要看得见）", *after.DeviceID)
	}
}

// TestDrinkOriginIsUniquePerDeviceNotPerManufacturer 是这次模型改动最要紧的一条：
// 判重键从 (manufacturer_id, origin_id) 变成 (device_id, manufacturer_id, origin_id)。
//
// 同一款饮品在 N 台设备上就是 N 行——这正是「每台设备一行饮品」的应有之义。旧的唯一
// 索引会把第二台设备上架同款饮品直接拒掉，静默地少一行。
func TestDrinkOriginIsUniquePerDeviceNotPerManufacturer(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	first := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	second := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	originID := "origin-" + uuid.NewString()
	cleanupFixture(t, pool, `DELETE FROM drinks WHERE manufacturer_id = $1 AND origin_id = $2`, manufacturerID, originID)

	// 同一台设备上的同款：第二行必须被拒。
	newDrink := func(deviceID, name string) *model.Drink {
		return &model.Drink{
			ID: uuid.NewString(), DeviceID: &deviceID, ManufacturerID: manufacturerID,
			OriginID: originID, ProductName: name, Price: 1800,
		}
	}
	firstDrink := newDrink(first, "集成测试美式 A")
	if _, err := repo.CreateDrink(ctx, firstDrink); err != nil {
		t.Fatalf("create on first device: %v", err)
	}
	cleanupAudit(t, pool, firstDrink.ID)
	if _, err := repo.CreateDrink(ctx, newDrink(first, "集成测试美式 A 重复")); !errors.Is(err, ErrDrinkOriginTaken) {
		t.Fatalf("err = %v, want ErrDrinkOriginTaken（同一台设备上的同款饮品）", err)
	}
	// 换一台设备：同样的 origin 必须放行。
	secondDrink := newDrink(second, "集成测试美式 B")
	if _, err := repo.CreateDrink(ctx, secondDrink); err != nil {
		t.Fatalf("create on second device: %v", err)
	}
	cleanupAudit(t, pool, secondDrink.ID)
}

// TestUpdateDrinkRejectsUnknownDevice 走的是外键那条路：device_id 有 FK，填一个不存在
// 的设备会是一条 23503。mapPGError 必须认识它，否则「设备填错了」会以 500 收场。
func TestUpdateDrinkRejectsUnknownDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := adminRepo(t, pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	drinkID := drinkFixture(t, pool, manufacturerID, "", "集成测试摩卡")
	cleanupAudit(t, pool, drinkID)

	missing := uuid.NewString()
	err := repo.UpdateDrink(ctx, &model.Drink{
		ID: drinkID, DeviceID: &missing, ProductName: "集成测试摩卡", Price: 1800,
	})
	if !errors.Is(err, ErrDeviceMissing) {
		t.Fatalf("err = %v, want ErrDeviceMissing", err)
	}
	// 被拒的写入不留痕迹：设备没挂上（写下去就是个孤儿引用），也没有审计
	// （一条说自己发生过的日志描述的是没发生的事）。
	if got := readDrinkDevice(t, pool, drinkID); got != "" {
		t.Fatalf("device = %q, want NULL after a rejected write", got)
	}
	if got := countAudit(t, pool, drinkID); got != 0 {
		t.Fatalf("audit rows = %d, want 0 for a rejected write", got)
	}
}

// TestUpdateDrinkIsSilentWithoutRecorder 顺带把 Noop 那条路走一遍：关掉审计时业务行
// 照样要写。审计是可选的旁路，不该成为写入是否能成功的前提。
func TestUpdateDrinkIsSilentWithoutRecorder(t *testing.T) {
	pool := integrationPool(t)
	repo := NewAdminRepository(pool, nil)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := deviceFixture(t, pool, manufacturerID, "active", nil, nil)
	drinkID := drinkFixture(t, pool, manufacturerID, "", "集成测试澳白")

	if err := repo.UpdateDrink(ctx, &model.Drink{
		ID: drinkID, DeviceID: &deviceID, ProductName: "集成测试澳白", Price: 1800,
	}); err != nil {
		t.Fatalf("update drink with auditing off: %v", err)
	}
	if got := readDrinkDevice(t, pool, drinkID); got != deviceID {
		t.Fatalf("device = %q, want %q", got, deviceID)
	}
	if count := countAudit(t, pool, drinkID); count != 0 {
		t.Fatalf("audit rows = %d, want 0 with a nil recorder", count)
	}
}
