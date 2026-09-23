package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 这一组用例守的是「支付成功 → 分账任务跟着成功」这一条（succeedSettlementInTx）。
//
// 只有真库能验它，因为这条路上有三件东西任何单元测试都碰不到：两条 UPDATE 的 WHERE 子句
// （接收方那一句要**赶在任务还是 pending 的时候**读它，顺序反过来就一条也匹配不到——那是个
// 静默的空操作，成功回调照样返回成功）、`finished_at`/`attempts` 这几个只在 SQL 里写的列，
// 以及「整单归平台也有任务」这个反直觉的形状。
//
// 与其它集成用例同一个约定：只用一个调用方给的库 URL，不建库、不删库、不跑迁移，夹具一律用
// 随机 uuid，跑完自己删干净。**payment_state_transitions 不清理**（append-only 触发器），
// 残留行每次都是新的聚合 id。
//
// 夹具链比别处长：接收方要求一个结算账户（RESTRICT 外键），所以造一张分账任务要先造一个账户。
// 这本身就说明了一件事——**「有接收方」是要配出来的**，库里默认的那条路是「没命中规则、整单
// 归平台」，那种任务照样要在这个回调里置成功，所以下面第二种夹具（没有接收方）不是简化，
// 是另一半现实。
//
// 账户的渠道是一个**名字**（`provider`），不是指向某张渠道表的外键：渠道与支付方式不在库里，
// 是代码里的目录（internal/catalog），没有可 JOIN 的那张表。

// settlementCallbackFixture 是一张「等着回调」的支付单，以及它那条分账任务。
type settlementCallbackFixture struct {
	paymentID      string
	paymentNo      string
	taskID         string
	receiverIDs    []string
	accountID      string
	amount         int64
	platformAmount int64
}

// newSettlementCallbackFixture 造夹具。amount 与 platformAmount 由调用方给：这一组用例不验
// 金额恒等式（那是写任务那一步的事，见 checkSettlementIdentity），验的是状态怎么翻。
func newSettlementCallbackFixture(t *testing.T, pool *pgxpool.Pool, withReceiver bool) *settlementCallbackFixture {
	t.Helper()
	ctx := context.Background()

	fixture := &settlementCallbackFixture{
		paymentID:      uuid.NewString(),
		paymentNo:      "PAY-INT-SET-" + uuid.NewString(),
		taskID:         uuid.NewString(),
		amount:         1980,
		platformAmount: 1980,
	}
	if withReceiver {
		fixture.platformAmount = 200
	}

	fixture.accountID = uuid.NewString()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 顺序由外键决定：接收方挂着账户（RESTRICT），任务挂着支付单（RESTRICT）。
		_, _ = pool.Exec(ctx, `DELETE FROM settlement_receivers WHERE task_id=$1`, fixture.taskID)
		_, _ = pool.Exec(ctx, `DELETE FROM settlement_tasks WHERE id=$1`, fixture.taskID)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_transactions WHERE payment_no=$1`, fixture.paymentNo)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_fundings WHERE payment_id=$1`, fixture.paymentID)
		_, _ = pool.Exec(ctx, `DELETE FROM payments WHERE id=$1`, fixture.paymentID)
		_, _ = pool.Exec(ctx, `DELETE FROM settlement_accounts WHERE id=$1`, fixture.accountID)
	})

	if _, err := pool.Exec(ctx, `INSERT INTO settlement_accounts
		(id, party_name, party_type, provider, receiver_id, status)
		VALUES ($1,$2,'member_store','ums',$3,'enabled')`,
		fixture.accountID, "集成测试门店",
		"MID-"+fixture.accountID); err != nil {
		t.Fatalf("insert settlement account: %v", err)
	}
	// status 是 pending：能结算的就这两个状态之一（见 canSettle），created 是发起后、用户还没付。
	if _, err := pool.Exec(ctx, `INSERT INTO payments
		(id, payment_no, order_no, user_id, amount, payment_method, provider, status, request_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,'ums_h5_alipay','ums','pending',$6, NOW() + INTERVAL '1 hour')`,
		fixture.paymentID, fixture.paymentNo, "ORD-INT-"+uuid.NewString(), uuid.NewString(),
		fixture.amount, "req-"+fixture.paymentID); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settlement_tasks
		(id, task_no, payment_id, payment_no, base_amount, platform_amount, scope_type, scope_ref)
		VALUES ($1,$2,$3,$4,$5,$6,'store',$7)`,
		fixture.taskID, "SET-INT-"+uuid.NewString()[:12], fixture.paymentID, fixture.paymentNo,
		fixture.amount, fixture.platformAmount, uuid.NewString()); err != nil {
		t.Fatalf("insert settlement task: %v", err)
	}
	if withReceiver {
		receiverID := uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO settlement_receivers
			(task_id, account_id, party_type, party_name, receiver_id, ratio, amount)
			VALUES ($1,$2,'member_store','集成测试门店',$3,0.9,$4)`,
			fixture.taskID, fixture.accountID, "MID-"+fixture.accountID,
			fixture.amount-fixture.platformAmount); err != nil {
			t.Fatalf("insert settlement receiver: %v", err)
		}
		fixture.receiverIDs = []string{receiverID}
	}
	return fixture
}

// settlementTaskStatus 读回任务的状态与那几个只在 SQL 里写的列。
func settlementTaskStatus(t *testing.T, pool *pgxpool.Pool, taskID string) (status, providerTransactionID string, finishedAt *time.Time, attempts int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT status, provider_transaction_id, finished_at, attempts
		FROM settlement_tasks WHERE id=$1`, taskID).
		Scan(&status, &providerTransactionID, &finishedAt, &attempts); err != nil {
		t.Fatalf("read settlement task: %v", err)
	}
	return status, providerTransactionID, finishedAt, attempts
}

// settlementReceiverStatuses 读回这条任务下所有接收方的状态。返回切片而不是单个值：多接收方
// 时「改了一条、漏了一条」是要能看出来的，而漏掉的那一条恰恰是最容易发生的那种错。
func settlementReceiverStatuses(t *testing.T, pool *pgxpool.Pool, taskID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT status FROM settlement_receivers WHERE task_id=$1 ORDER BY created_at`, taskID)
	if err != nil {
		t.Fatalf("read settlement receivers: %v", err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatalf("scan settlement receiver: %v", err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read settlement receivers: %v", err)
	}
	return statuses
}

// TestPostgresSettlePaymentSucceedsSettlement 成功回调要把任务与接收方一起置成功。
//
// 两种夹具同一条用例里跑：**有接收方**的那张验的是那两句 UPDATE 真匹配上了（顺序写反就一条
// 也匹配不到，而它不会报错）；**没接收方**的那张（整单归平台）验的是任务本身也被置成功了——
// 只测前者的写法会让「没命中规则的单永远停在 pending」这个 bug 逃掉，而库里绝大多数任务正是
// 这一种。
func TestPostgresSettlePaymentSucceedsSettlement(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	paidAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	providerTransactionID := "UMS-INT-" + uuid.NewString()

	cases := []struct {
		name         string
		withReceiver bool
	}{
		{name: "命中了规则的单", withReceiver: true},
		{name: "整单归平台的单", withReceiver: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSettlementCallbackFixture(t, pool, tc.withReceiver)

			settlement, err := repo.SettlePayment(ctx, SettleNotificationParams{
				PaymentNo:             fixture.paymentNo,
				Provider:              "ums",
				Succeeded:             true,
				Amount:                fixture.amount,
				ProviderTransactionID: providerTransactionID,
				PaidAt:                paidAt,
			})
			if err != nil {
				t.Fatalf("SettlePayment: %v", err)
			}
			if settlement.Payment.Status != model.PaymentSucceeded {
				t.Fatalf("payment status = %s, want %s", settlement.Payment.Status, model.PaymentSucceeded)
			}

			status, gotTransactionID, finishedAt, attempts := settlementTaskStatus(t, pool, fixture.taskID)
			if status != model.SettlementStatusSucceeded {
				t.Fatalf("settlement task status = %s, want %s", status, model.SettlementStatusSucceeded)
			}
			// 渠道交易号与完结时间都必须落在这条任务上：分账任务号是我方生成的，将来对账要拿
			// 渠道的号去找这笔钱，而它唯一的来源就是这条回调。
			if gotTransactionID != providerTransactionID {
				t.Fatalf("provider_transaction_id = %q, want %q", gotTransactionID, providerTransactionID)
			}
			if finishedAt == nil {
				// 银联商务一次下发没有「完结」那一步，settlement_tasks.finished_at 的列注释要求 succeeded 时一并置上。
				t.Fatal("finished_at is null on a succeeded settlement task")
			}
			if !finishedAt.Equal(paidAt) {
				t.Fatalf("finished_at = %s, want %s", finishedAt, paidAt)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1", attempts)
			}

			statuses := settlementReceiverStatuses(t, pool, fixture.taskID)
			if len(statuses) != len(fixture.receiverIDs) {
				t.Fatalf("receiver rows = %d, want %d", len(statuses), len(fixture.receiverIDs))
			}
			for i, status := range statuses {
				if status != model.SettlementStatusSucceeded {
					t.Fatalf("receiver %d status = %s, want %s", i, status, model.SettlementStatusSucceeded)
				}
			}
		})
	}
}

// TestPostgresSettlePaymentFailureCancelsSettlement 失败回调那一支的回归。
//
// 它与成功那一支共用同一个夹具与同一个入口，改错方向时（例如把两个分支写成一个无条件置成功）
// 两条用例里必有一条会红——单独测成功那一支是看不出来的。
func TestPostgresSettlePaymentFailureCancelsSettlement(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	fixture := newSettlementCallbackFixture(t, pool, true)

	if _, err := repo.SettlePayment(ctx, SettleNotificationParams{
		PaymentNo:      fixture.paymentNo,
		Provider:       "ums",
		Succeeded:      false,
		Amount:         fixture.amount,
		FailureCode:    "TEST_FAILED",
		FailureMessage: "集成测试",
	}); err != nil {
		t.Fatalf("SettlePayment: %v", err)
	}

	status, _, finishedAt, _ := settlementTaskStatus(t, pool, fixture.taskID)
	if status != model.SettlementStatusCancelled {
		t.Fatalf("settlement task status = %s, want %s", status, model.SettlementStatusCancelled)
	}
	// 作废的任务没有「完结时间」可言：它是没发出去，不是发出去之后有个结果。
	if finishedAt != nil {
		t.Fatalf("finished_at = %s, want null on a cancelled settlement task", finishedAt)
	}
	for i, status := range settlementReceiverStatuses(t, pool, fixture.taskID) {
		if status != model.SettlementStatusCancelled {
			t.Fatalf("receiver %d status = %s, want %s", i, status, model.SettlementStatusCancelled)
		}
	}
}

// TestPostgresSettlePaymentRedeliveryKeepsSettlement 同一条成功回调投第二次。
//
// 渠道重投是常态。第二投走的是幂等那条早退路径（payment 已经是 succeeded），它**不碰分账**——
// 这里守的是那个早退真的没把已经写完的东西再改一遍：`attempts` 会涨、`finished_at` 会被重写成
// 重投那一刻。两个值都是「只能被第一次的回调定下来」的，所以它们是最好的探针。
func TestPostgresSettlePaymentRedeliveryKeepsSettlement(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	paidAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	fixture := newSettlementCallbackFixture(t, pool, true)

	notification := SettleNotificationParams{
		PaymentNo:             fixture.paymentNo,
		Provider:              "ums",
		Succeeded:             true,
		Amount:                fixture.amount,
		ProviderTransactionID: "UMS-INT-" + uuid.NewString(),
		PaidAt:                paidAt,
	}
	if _, err := repo.SettlePayment(ctx, notification); err != nil {
		t.Fatalf("first SettlePayment: %v", err)
	}

	redelivered, err := repo.SettlePayment(ctx, notification)
	if err != nil {
		t.Fatalf("second SettlePayment: %v", err)
	}
	if !redelivered.AlreadySettled {
		t.Fatal("second delivery is not marked AlreadySettled")
	}

	status, _, finishedAt, attempts := settlementTaskStatus(t, pool, fixture.taskID)
	if status != model.SettlementStatusSucceeded {
		t.Fatalf("settlement task status = %s, want %s", status, model.SettlementStatusSucceeded)
	}
	if finishedAt == nil || !finishedAt.Equal(paidAt) {
		t.Fatalf("finished_at = %v, want %s (rewritten by the redelivery)", finishedAt, paidAt)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (rewritten by the redelivery)", attempts)
	}
}

// TestPostgresGetSettlementTaskMapsNoRows 守「查无此任务」那一步真的翻成了哨兵。
//
// 为什么这条必须连库：`pgx.ErrNoRows` 是**扫描那一刻**才产生的，单元测试只能把一个现成的
// 哨兵喂给 controller 的错误映射表，整条产生错误的路径都在它外面。曾经就是这样漏的——
// controller 的表里只有 ErrPaymentNotFound，而仓储读任务那一支抛的是裸的 ErrNoRows，于是
// 手敲一个不存在的 id 回的是 500 加一句「服务暂时不可用」，而代码注释里还写着这条已经修好。
//
// 反过来说，这条用例红了就意味着一件事：又有人把「仓储抛什么」和「controller 认什么」拆成
// 了两处各自维护的清单。
func TestPostgresGetSettlementTaskMapsNoRows(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())

	// 合法 uuid、库里没有这一条——与「运维从别处复制了一个旧 id」同一个形状。不造任何夹具，
	// 所以也不需要清理。
	_, err := repo.GetSettlementTask(context.Background(), uuid.NewString())
	if !errors.Is(err, ErrSettlementTaskNotFound) {
		t.Fatalf("err = %v，期望 ErrSettlementTaskNotFound（裸的 pgx.ErrNoRows 会让页面回 500）", err)
	}
}
