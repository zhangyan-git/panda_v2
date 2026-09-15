package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 这一组用例守的是 005 迁移加的那道闸：**豆已经扣了的支付单不许被当成没付过一样关掉**。
//
// 只有真库能验它。判据全在那两条 SQL 的 WHERE 子句里（关单排除 account_entry_id 非空的行、
// 补偿任务只挑非空的行），应用层看不见也补不回来——把子句写错，任何单元测试都不会红，而
// 后果是一次真实的扣款变成一张「已过期」的单：钱在账户域已经出账，本地却说这笔支付从来没成。
//
// 与其它服务的集成用例同一个约定：只用一个调用方给的库 URL，不建库、不删库、不跑迁移，
// 夹具一律用随机 uuid。**payment_state_transitions 跑完不清理**——那张表有 append-only
// 触发器，DELETE 会直接抛；残留行每次都是新的聚合 id，不会让断言互相串台。
func paymentIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PAYMENT_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set PAYMENT_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("payment test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("payment test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fundedPaymentFixture 是一张「到点未结」的支付单，账户出资与渠道支付两种形状都能造。
//
// accountEntryID 为空串时造的是渠道那一类：它**不该**被这两条新规则碰到，所以它必须与账户
// 出资那张出现在同一个用例里——只测「账户出资不会被关掉」的话，把关单扫描写成一个什么都不
// 关的空操作也能过。
type fundedPaymentFixture struct {
	paymentID      string
	paymentNo      string
	orderNo        string
	amount         int64
	accountEntryID string
	fundedAt       time.Time
}

func newFundedPaymentFixture(t *testing.T, pool *pgxpool.Pool, accountEntryID string) *fundedPaymentFixture {
	t.Helper()
	fixture := &fundedPaymentFixture{
		paymentID:      uuid.NewString(),
		paymentNo:      "PAY-INT-" + uuid.NewString(),
		orderNo:        "ORD-INT-" + uuid.NewString(),
		amount:         1980,
		accountEntryID: accountEntryID,
		fundedAt:       time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond),
	}
	// expires_at 在过去：这张单在关单扫描的判定里已经是「到点未支付」。
	_, err := pool.Exec(context.Background(), `INSERT INTO payments
		(id, payment_no, order_no, user_id, amount, funding_type, status,
		 request_id, expires_at, account_entry_id, account_funded_at)
		VALUES ($1,$2,$3,$4,$5,$6,'created',$7,NOW() - INTERVAL '1 hour',
			NULLIF($8,'')::uuid, $9)`,
		fixture.paymentID, fixture.paymentNo, fixture.orderNo, uuid.NewString(),
		fixture.amount, model.FundingCoffeeBean, "req-"+fixture.paymentID,
		fixture.accountEntryID, fixture.fundedAt)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 出资行对 payments 是 ON DELETE RESTRICT，必须先删它。状态流水不删（append-only）。
		_, _ = pool.Exec(ctx, `DELETE FROM payment_fundings WHERE payment_id=$1`, fixture.paymentID)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_transactions WHERE payment_no=$1`, fixture.paymentNo)
		_, _ = pool.Exec(ctx, `DELETE FROM payments WHERE id=$1`, fixture.paymentID)
	})
	return fixture
}

// paymentStatus 读回这张单当前的状态与记着的账变 ID。
func paymentStatus(t *testing.T, pool *pgxpool.Pool, paymentID string) (string, *string) {
	t.Helper()
	var status string
	var entryID *string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, account_entry_id::text FROM payments WHERE id=$1`, paymentID).
		Scan(&status, &entryID); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	return status, entryID
}

// TestPostgresExpireSkipsPaymentsWithADeductedAccountEntry 关单扫描必须放过「豆已经扣了」的单。
//
// 一条用例里同时摆两张单，因为只断言「账户出资那张没被关」是不够的——把关单扫描整个写空也
// 能让它过。渠道那张必须被关掉，两条断言合起来才说明这道闸是「按列筛的」而不是「没关」。
func TestPostgresExpireSkipsPaymentsWithADeductedAccountEntry(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	funded := newFundedPaymentFixture(t, pool, uuid.NewString())
	channel := newFundedPaymentFixture(t, pool, "")

	if _, err := repo.ExpireOverduePayments(ctx, 100); err != nil {
		t.Fatalf("ExpireOverduePayments: %v", err)
	}

	if status, _ := paymentStatus(t, pool, funded.paymentID); status != model.PaymentCreated {
		t.Fatalf("豆已经扣了的单被关成了 %q，want %q（钱已经出账，不能当成没付过）",
			status, model.PaymentCreated)
	}
	if status, _ := paymentStatus(t, pool, channel.paymentID); status != model.PaymentExpired {
		t.Fatalf("渠道支付那张该被关掉，got %q", status)
	}
}

// TestPostgresRecordAccountDeductionLeavesATrace 扣豆留痕要真的落在那两列上。
//
// 这是整个修复的起点：没有它，扣豆成功而结算失败的那张单在本地一无所有。时间也要落对——
// 它是结算时的成交时间，不是「补偿任务跑到的那一刻」。
func TestPostgresRecordAccountDeductionLeavesATrace(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	// 造一张没有账变 ID 的单，再用 RecordAccountDeduction 给它写上。
	fixture := newFundedPaymentFixture(t, pool, "")
	entryID := uuid.NewString()
	fundedAt := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Millisecond)

	if err := repo.RecordAccountDeduction(ctx, fixture.paymentID, entryID, fundedAt); err != nil {
		t.Fatalf("RecordAccountDeduction: %v", err)
	}

	var gotEntry *string
	var gotFunded *time.Time
	if err := pool.QueryRow(ctx, `SELECT account_entry_id::text, account_funded_at
		FROM payments WHERE id=$1`, fixture.paymentID).Scan(&gotEntry, &gotFunded); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if gotEntry == nil || *gotEntry != entryID {
		t.Fatalf("account_entry_id = %v, want %s", gotEntry, entryID)
	}
	if gotFunded == nil || !gotFunded.Equal(fundedAt) {
		t.Fatalf("account_funded_at = %v, want %v", gotFunded, fundedAt)
	}
}

// TestPostgresSettleOverdueAccountPaymentCarriesTheEntryID 补偿路径选得出这些单，并且结算把
// 账变 ID 一路带到出资行上。
//
// 出资行上的 account_entry_id 是**退款与对账唯一能反查那笔账变**的东西（见
// payment_fundings 的列注释），漏了它，这条补偿路径就只是把状态改对了，钱还是找不回来。
func TestPostgresSettleOverdueAccountPaymentCarriesTheEntryID(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	entryID := uuid.NewString()
	funded := newFundedPaymentFixture(t, pool, entryID)
	// 摆一张不该被选中的对照：同样是到点未结，但它是渠道支付。
	newFundedPaymentFixture(t, pool, "")

	overdue, err := repo.FindOverdueAccountFundedPayments(ctx, 100)
	if err != nil {
		t.Fatalf("FindOverdueAccountFundedPayments: %v", err)
	}
	var found *model.Payment
	for i := range overdue {
		// 选出来的每一行都必须带着账变 ID：**没有它就没有可结算的东西**，这张单之所以被
		// 选中，全部依据就是「豆已经扣了」。少这一条断言，把筛选条件整个去掉也照样能过
		// （上面那张对照单会跟着混进来，而它根本不该在这批里）。
		if overdue[i].AccountEntryID == nil {
			t.Fatalf("补偿扫描选中了没有账变 ID 的 %s，它没有豆可结", overdue[i].PaymentNo)
		}
		if overdue[i].ID == funded.paymentID {
			found = &overdue[i]
		}
	}
	if found == nil {
		t.Fatalf("补偿扫描没有选中 %s（豆已经扣了、到点还没结）", funded.paymentNo)
	}
	if found.AccountEntryID == nil || *found.AccountEntryID != entryID {
		t.Fatalf("选中行的 account_entry_id = %v, want %s", found.AccountEntryID, entryID)
	}

	if _, err := repo.SettleAccountPayment(ctx, SettleAccountPaymentParams{
		PaymentID:      funded.paymentID,
		AccountEntryID: entryID,
		// 成交时间用扣豆那一刻：记成本次扫描的时间会让对账看到一个凭空晚了一小时的收款。
		PaidAt:    funded.fundedAt,
		RequestID: "req-" + funded.paymentID,
	}); err != nil {
		t.Fatalf("SettleAccountPayment: %v", err)
	}

	if status, _ := paymentStatus(t, pool, funded.paymentID); status != model.PaymentSucceeded {
		t.Fatalf("结算后状态 = %q, want %q", status, model.PaymentSucceeded)
	}
	var lineEntry *string
	var succeededAt time.Time
	if err := pool.QueryRow(ctx, `SELECT account_entry_id::text, succeeded_at
		FROM payment_fundings WHERE payment_id=$1 AND line_no=1`, funded.paymentID).
		Scan(&lineEntry, &succeededAt); err != nil {
		t.Fatalf("read funding line: %v", err)
	}
	if lineEntry == nil || *lineEntry != entryID {
		t.Fatalf("出资行的 account_entry_id = %v, want %s", lineEntry, entryID)
	}
	if !succeededAt.Equal(funded.fundedAt) {
		t.Fatalf("出资行成交时间 = %v, want %v", succeededAt, funded.fundedAt)
	}
}
