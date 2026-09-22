package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 这些用例盯的是取货码那条路的收款动作（方案 §四）：从设备余额里扣一笔，并留下能查的
// 流水。老系统那条路上只有一次 `$inc`，一分钱记录都没有——这台机器的钱怎么没的查不到。
// 下面每一条都在确认这里不照抄那个做法，尤其是「扣了钱却没有流水」这种只有出事那天
// 才会被发现的状态。

// seedCoffeeBalance 直接写 devices.coffee_balance。
//
// 不走 AdjustBalance 播种：那会顺手写一行 type='adjust' 的流水和一条审计，让「流水恰好
// 一行」这类断言先要减掉播种那几行，而用例要盯的是扣减自己写了几行。
func seedCoffeeBalance(t *testing.T, pool *pgxpool.Pool, deviceID string, amount int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE devices SET coffee_balance = $2 WHERE id = $1`, deviceID, amount); err != nil {
		t.Fatalf("seed coffee balance: %v", err)
	}
}

// itPickupPassword 是取货码用例给设备配的那个静态验证码。
const itPickupPassword = "1357"

// seedPickupPassword 直接写 devices.pickup_password——设备详情页上运营填的那个字段。
func seedPickupPassword(t *testing.T, pool *pgxpool.Pool, deviceID, password string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE devices SET pickup_password = $2 WHERE id = $1`, deviceID, password); err != nil {
		t.Fatalf("seed pickup password: %v", err)
	}
}

// pickupDeviceFixture 造一台「能走取货码」的设备：配好验证码、余额是给定的数。
//
// 每一步都是这条路的前提，逐条手写会让用例里塞满与它要盯的东西无关的行；而漏掉验证码
// 那一步的表现是每个用例都回 ErrPickupPasswordNotSet，看着像功能坏了。
func pickupDeviceFixture(t *testing.T, pool *pgxpool.Pool, manufacturerID string, balance int64) string {
	t.Helper()
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)
	seedCoffeeBalance(t, pool, deviceID, balance)
	seedPickupPassword(t, pool, deviceID, itPickupPassword)
	return deviceID
}

// deviceBalance 读回设备的当前余额。
func deviceBalance(t *testing.T, pool *pgxpool.Pool, deviceID string) int64 {
	t.Helper()
	var balance int64
	if err := pool.QueryRow(context.Background(),
		`SELECT coffee_balance FROM devices WHERE id = $1`, deviceID).Scan(&balance); err != nil {
		t.Fatalf("read back balance: %v", err)
	}
	return balance
}

// ledgerRow 是一条余额流水在用例里的样子，只取下面几条断言要看的列。
type ledgerRow struct {
	Type          string
	Amount        int64
	BalanceAfter  int64
	ReferenceType string
	RequestID     string
	OperatorID    *string
}

// ledgerRows 按 request_id 读回流水行。按 request_id 过滤而不是按设备：同一台设备上
// 「扣了几次」正是重投那条用例要数的东西。
func ledgerRows(t *testing.T, pool *pgxpool.Pool, deviceID, requestID string) []ledgerRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT type, amount, balance_after,
		reference_type, request_id, operator_id::text FROM device_balance_ledger
		WHERE device_id = $1 AND request_id = $2 ORDER BY created_at`, deviceID, requestID)
	if err != nil {
		t.Fatalf("read back ledger rows: %v", err)
	}
	defer rows.Close()

	var entries []ledgerRow
	for rows.Next() {
		var entry ledgerRow
		if err := rows.Scan(&entry.Type, &entry.Amount, &entry.BalanceAfter,
			&entry.ReferenceType, &entry.RequestID, &entry.OperatorID); err != nil {
			t.Fatalf("scan ledger row: %v", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ledger rows: %v", err)
	}
	return entries
}

// TestDeductDeviceBalanceRecordsLedgerAndBalance 是这条路的正常态，三件事必须一起发生：
// 余额减了、流水加了一行、流水上那个 balance_after 与设备当前余额对得上。
//
// 最后一条不是废话：流水上的 balance_after 是「当时的余额」，不是增量。写错成增量
// （比如写成 after-before），账面上看不出问题，直到有人拿它对账。
func TestDeductDeviceBalanceRecordsLedgerAndBalance(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	requestID := "it-pickup-" + uuid.NewString()
	result, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: itPickupPassword, Remark: "取货码 1 杯",
	})
	if err != nil {
		t.Fatalf("deduct device balance: %v", err)
	}
	if !result.Applied {
		t.Fatal("applied = false, want true on the first call")
	}
	if result.BalanceAfter != 3500 {
		t.Fatalf("balance_after = %d, want 3500", result.BalanceAfter)
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 3500 {
		t.Fatalf("devices.coffee_balance = %d, want the same 3500 the ledger records", balance)
	}

	entries := ledgerRows(t, pool, deviceID, requestID)
	if len(entries) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(entries))
	}
	entry := entries[0]
	if entry.Type != "deduct" {
		t.Fatalf("type = %q, want deduct (adjust 是后台手工改数，两者要能分开)", entry.Type)
	}
	// 金额在库里是**有符号**的：扣减记负数。记成正数的话，流水表上「这台机器收了多少」
	// 与「充了多少」就没法用一句 sum 算出来。
	if entry.Amount != -1500 {
		t.Fatalf("amount = %d, want -1500", entry.Amount)
	}
	if entry.BalanceAfter != 3500 {
		t.Fatalf("balance_after = %d, want the balance at that moment (not the delta)", entry.BalanceAfter)
	}
	if entry.ReferenceType != "pickup_code" {
		t.Fatalf("reference_type = %q, want pickup_code", entry.ReferenceType)
	}
	// 这条路上没有操作人：操作人是机器前面那个人，他在我们库里没有账号。编一个出来
	// 只会让审计里出现一批查无此人的记录。
	if entry.OperatorID != nil {
		t.Fatalf("operator_id = %q, want NULL for a device-side deduction", *entry.OperatorID)
	}
}

// TestDeductDeviceBalanceRefusesWhenTheMachineIsShort 是余额不够那一条：这一单真的做不成，
// 而**一个字段都不能写**。
//
// 「扣成负数再回滚」不是这条路的样子——数据库上那条 CHECK 会先炸，而调用方收到的是一个
// 500，看起来像故障而不是「余额不够」。判断必须在 UPDATE 之前做完。
func TestDeductDeviceBalanceRefusesWhenTheMachineIsShort(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 1000)

	requestID := "it-pickup-" + uuid.NewString()
	_, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: itPickupPassword,
	})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 1000 {
		t.Fatalf("devices.coffee_balance = %d, want 1000 untouched", balance)
	}
	if entries := ledgerRows(t, pool, deviceID, requestID); len(entries) != 0 {
		t.Fatalf("ledger rows = %d, want none for a refused deduction", len(entries))
	}
}

// TestDeductDeviceBalanceIsIdempotentOnTheRequestID 是重投那一条（厂商报文被重放、对方
// 自己重试都会走到这里）。
//
// 第二次不是错误，而是 applied=false + 当初那个余额：调用方要拿它继续把订单建出来。
// 回错误的话，一次**已经收到钱**的取货会永远建不出单；而再扣一次的话，用户为同一杯
// 付了两回。
func TestDeductDeviceBalanceIsIdempotentOnTheRequestID(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	requestID := "it-pickup-" + uuid.NewString()
	first, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: itPickupPassword,
	})
	if err != nil {
		t.Fatalf("first deduct: %v", err)
	}

	// 第二次带同样的 request_id，但金额故意不同：幂等键命中时**不看金额**，否则
	// 「重投」会被当成一次新扣减，只是因为它这次报的数不一样。
	second, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 900, RequestID: requestID, PickupPassword: itPickupPassword,
	})
	if err != nil {
		t.Fatalf("replayed deduct: %v", err)
	}
	if second.Applied {
		t.Fatal("applied = true, want false on a replay")
	}
	if second.BalanceAfter != first.BalanceAfter {
		t.Fatalf("balance_after = %d, want the %d recorded by the first call",
			second.BalanceAfter, first.BalanceAfter)
	}
	// 金额那一格回的是**当初扣掉的** 1500，不是这次请求里报的 900。重投一分钱都不会再扣，
	// 而订单要照这一格记账：回本次请求的金额（或者回 0）会让订单与流水对不上，那笔差额
	// 在库里没有任何一处能解释。
	if first.Amount != 1500 {
		t.Fatalf("first amount = %d, want 1500", first.Amount)
	}
	if second.Amount != 1500 {
		t.Fatalf("replayed amount = %d, want the 1500 that was actually charged", second.Amount)
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 3500 {
		t.Fatalf("devices.coffee_balance = %d, want 3500 (the replay must not have applied)", balance)
	}
	if entries := ledgerRows(t, pool, deviceID, requestID); len(entries) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1 (只是那一行，不是两行)", len(entries))
	}
}

// TestDeductDeviceBalanceRejectsEmptyRequestID 是幂等键为空那一条，它必须在 SQL 之前停下。
//
// device_balance_ledger_one_per_request 只索引 request_id 非空的行：空串会**绕过**那个
// 唯一索引，于是「重投扣两次」这条路在数据库那一层是敞开的，只有这里能挡。挡不住的
// 表现是两次都成功、都不报错。
func TestDeductDeviceBalanceRejectsEmptyRequestID(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	if _, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, PickupPassword: itPickupPassword,
	}); err == nil {
		t.Fatal("err = nil, want a refusal for an empty request id")
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 5000 {
		t.Fatalf("devices.coffee_balance = %d, want 5000 untouched", balance)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_balance_ledger
		WHERE device_id = $1`, deviceID).Scan(&count); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if count != 0 {
		t.Fatalf("ledger rows = %d, want none", count)
	}
}

// TestDeductDeviceBalanceRejectsNonPositiveAmount 盯金额这一侧：0 会撞流水表的
// `amount <> 0` 约束，负数在这条路上等于给机器加钱。两件都要在写之前挡下——尤其是
// 负数，它会让一次取货变成一次充值。
func TestDeductDeviceBalanceRejectsNonPositiveAmount(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	for _, amount := range []int64{0, -1500} {
		if _, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
			DeviceID: deviceID, Amount: amount, RequestID: "it-pickup-" + uuid.NewString(),
		}); err == nil {
			t.Fatalf("amount = %d: err = nil, want a refusal", amount)
		}
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 5000 {
		t.Fatalf("devices.coffee_balance = %d, want 5000 untouched", balance)
	}
}

// TestDeductDeviceBalanceRejectsAWrongPickupPassword 是「码不对」那一条。
//
// 它盯的是**校验确实在这条路上**：这个码是设备余额的唯一凭据，没有它，任何能通过厂商
// 验签的调用方（也就是厂商自己）都能把任意一台机器上的钱扣走，而运营看不出异常。
//
// 断言里除了错误本身，还要确认**什么都没写**：被拒的一次尝试不该在只增不删的流水表上
// 留下痕迹。
func TestDeductDeviceBalanceRejectsAWrongPickupPassword(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	requestID := "it-pickup-" + uuid.NewString()
	_, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: "0000",
	})
	if !errors.Is(err, ErrPickupPasswordMismatch) {
		t.Fatalf("err = %v, want ErrPickupPasswordMismatch", err)
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 5000 {
		t.Fatalf("devices.coffee_balance = %d, want 5000 untouched", balance)
	}
	if entries := ledgerRows(t, pool, deviceID, requestID); len(entries) != 0 {
		t.Fatalf("ledger rows = %d, want none for a rejected password", len(entries))
	}
}

// TestDeductDeviceBalanceRefusesADeviceWithoutAPickupPassword 盯的是「没配码」那一条。
//
// devices.pickup_password 是 NOT NULL 且默认空串，所以「没配过」在库里长得像「配了个
// 空码」。如果这里按 `stored == submitted` 比，那么提交空串就等于通行证——这台机器上的
// 钱谁都能扣。所以没配过必须是**拒绝**，而不是空对空放行。
func TestDeductDeviceBalanceRefusesADeviceWithoutAPickupPassword(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := balanceDeviceFixture(t, pool, manufacturerID)
	seedCoffeeBalance(t, pool, deviceID, 5000)

	for _, submitted := range []string{"", "1357"} {
		requestID := "it-pickup-" + uuid.NewString()
		_, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
			DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: submitted,
		})
		if !errors.Is(err, ErrPickupPasswordNotSet) {
			t.Fatalf("submitted %q: err = %v, want ErrPickupPasswordNotSet", submitted, err)
		}
		if entries := ledgerRows(t, pool, deviceID, requestID); len(entries) != 0 {
			t.Fatalf("submitted %q: ledger rows = %d, want none", submitted, len(entries))
		}
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 5000 {
		t.Fatalf("devices.coffee_balance = %d, want 5000 untouched", balance)
	}
}

// TestDeductDeviceBalanceReportsMissingDevice 是设备不存在那一条：调用方要把它和「余额
// 不够」分开——一个要换设备号，一个这一单就是做不成。
func TestDeductDeviceBalanceReportsMissingDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)

	_, err := repo.DeductDeviceBalance(context.Background(), DeductDeviceBalanceParams{
		DeviceID: uuid.NewString(), Amount: 1500, RequestID: "it-pickup-" + uuid.NewString(),
	})
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("err = %v, want ErrDeviceNotFound", err)
	}
}

// TestDeductDeviceBalanceReplayDoesNotNeedThePickupPassword 是补单那条路的**前提**：重投在
// 验证码之前就被认出来。
//
// 出事的样子：钱已经扣了、订单没建出来（中间断了），运维拿着对方单号去补——而这时候这台设备的
// 取货码已经被后台改过了（或者被清空了）。补单接口带的是空码（它本来就没有码可带），如果重投
// 还要过验证码这一关，这一笔**已经扣掉的钱**就永远变不成一张单，流水上那笔扣减从此无人认领。
//
// 所以这里刻意把码改掉再重投：第一次用 1357 扣成功，随后把设备上的码换成 9999（模拟改码），
// 再用空码重投——必须仍然是 applied=false + 当初的余额，一个字段都不写。
func TestDeductDeviceBalanceReplayDoesNotNeedThePickupPassword(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	requestID := "it-pickup-" + uuid.NewString()
	first, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: itPickupPassword,
	})
	if err != nil {
		t.Fatalf("first deduct: %v", err)
	}

	// 码被改过（后台能改、也能清空），补单那边带的是空码。
	seedPickupPassword(t, pool, deviceID, "9999")
	second, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: "",
	})
	if err != nil {
		t.Fatalf("a replay must not be stopped by the pickup password: %v", err)
	}
	if second.Applied {
		t.Fatal("applied = true, want false on a replay")
	}
	if second.Amount != 1500 || second.BalanceAfter != first.BalanceAfter {
		t.Fatalf("replay = (amount %d, balance %d), want the first call's (%d, %d)",
			second.Amount, second.BalanceAfter, first.Amount, first.BalanceAfter)
	}
	if balance := deviceBalance(t, pool, deviceID); balance != 3500 {
		t.Fatalf("devices.coffee_balance = %d, want 3500 (重投一个字段都不写)", balance)
	}
	if entries := ledgerRows(t, pool, deviceID, requestID); len(entries) != 1 {
		t.Fatalf("ledger rows = %d, want exactly 1", len(entries))
	}
}

// TestDeductDeviceBalanceRejectsARequestIDUsedByAnAdjustment 是三态读的第三态：这个 request_id
// 在流水上有行，但那一行是**后台调整**写的（同一张表、同一个唯一索引，键却是另一条路放进去的）。
//
// 这一态必须与「没有」和「重投」分开，因为把它当成重投的后果是**这一杯白送**：调用方拿到
// applied=false，当作「上一次已经扣成功了」继续把订单建出来，而设备的钱一分没动。回错误只是
// 这一单建不出来——看得见，也查得到。
func TestDeductDeviceBalanceRejectsARequestIDUsedByAnAdjustment(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	admin := NewAdminRepository(pool, nil)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	deviceID := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	requestID := "it-pickup-" + uuid.NewString()
	// 管理员的后台调整先用了这个 request_id（同一个命名空间，见 device_balance_ledger 的
	// 唯一索引：它只认 request_id，不认这个值是哪条路放进去的）。
	if _, err := admin.AdjustBalance(ctx, BalanceAdjustment{
		DeviceID: deviceID, Amount: 2000, RequestID: requestID, Remark: "集成测试：先占住这个键",
	}); err != nil {
		t.Fatalf("adjust balance: %v", err)
	}

	_, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: deviceID, Amount: 1500, RequestID: requestID, PickupPassword: itPickupPassword,
	})
	if !errors.Is(err, ErrRequestIDUsedByAnotherEntry) {
		t.Fatalf("err = %v, want ErrRequestIDUsedByAnotherEntry", err)
	}
	// 一个字都没写：余额是调整之后那 7000，流水还是调整那一行。
	if balance := deviceBalance(t, pool, deviceID); balance != 7000 {
		t.Fatalf("devices.coffee_balance = %d, want 7000 (调整之后，未被这次扣减动过)", balance)
	}
	entries := ledgerRows(t, pool, deviceID, requestID)
	if len(entries) != 1 || entries[0].Type != "adjust" {
		t.Fatalf("ledger = %+v, want exactly the one adjust row", entries)
	}
}

// TestDeductDeviceBalanceRejectsARequestIDFromAnotherDevice 是第三态的另一种长相：那一行**是**
// 一条 deduct，但它在另一台设备上。当成重投的话，这台机器上的这一杯会在没扣钱的情况下被建单
// ——扣减记的是甲机器的账，卖出去的却是乙机器的饮料。
func TestDeductDeviceBalanceRejectsARequestIDFromAnotherDevice(t *testing.T) {
	pool := integrationPool(t)
	repo := NewDeviceBalanceRepository(pool)
	ctx := context.Background()
	manufacturerID := manufacturerFixture(t, pool)
	chargedDevice := pickupDeviceFixture(t, pool, manufacturerID, 5000)
	otherDevice := pickupDeviceFixture(t, pool, manufacturerID, 5000)

	requestID := "it-pickup-" + uuid.NewString()
	if _, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: chargedDevice, Amount: 1500, RequestID: requestID, PickupPassword: itPickupPassword,
	}); err != nil {
		t.Fatalf("deduct on the first device: %v", err)
	}

	_, err := repo.DeductDeviceBalance(ctx, DeductDeviceBalanceParams{
		DeviceID: otherDevice, Amount: 1500, RequestID: requestID, PickupPassword: itPickupPassword,
	})
	if !errors.Is(err, ErrRequestIDUsedByAnotherEntry) {
		t.Fatalf("err = %v, want ErrRequestIDUsedByAnotherEntry", err)
	}
	if balance := deviceBalance(t, pool, otherDevice); balance != 5000 {
		t.Fatalf("另一台设备的余额 = %d, want 5000 untouched", balance)
	}
	if entries := ledgerRows(t, pool, otherDevice, requestID); len(entries) != 0 {
		t.Fatalf("另一台设备的流水 = %+v, want none", entries)
	}
}
