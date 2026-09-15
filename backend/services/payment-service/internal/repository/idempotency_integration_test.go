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

// 这一组用例守的是 payments_request_id_key（`request_id 非空时唯一`）之上那套回收逻辑：
// 一条陈旧的 processing 幂等键必须能被**真正**重开，而不是回收完必然撞索引。
//
// 只有真库能验它。判据是三条语句撞在一起的结果（幂等行的 DELETE+INSERT、支付单的 UPDATE、
// 支付单的 INSERT），应用层看不见唯一索引——把 retireAbandonedPayment 整个删掉，任何单元
// 测试都不会红，而线上那个「进程死在两次提交之间」的窗口（MarkPaymentPending 的注释写着它，
// 回收就是它唯一的出路）会变成一次永远重试不动的 500。
//
// 与其它集成用例同一个约定：只用一个调用方给的库 URL，不建库、不删库、不跑迁移，
// payment_state_transitions 跑完不清理（append-only 触发器挡 DELETE）。
type staleAttemptFixture struct {
	requestID string
	hash      string
	paymentID string
	paymentNo string
	orderNo   string
}

// newStaleAttemptFixture 造一次「发起支付中途死掉」留下的现场：一张挂在 requestID 上的支付单，
// 外加一条比回收窗口更旧的 processing 幂等行。
//
// status 由调用方给。created 是这个窗口本来的样子（BeginPayment 提交了、MarkPaymentPending
// 没有）；succeeded 是回调在窗口中间插进来的那个少见分支，见 retireAbandonedPayment 的注释。
func newStaleAttemptFixture(t *testing.T, pool *pgxpool.Pool, status string) *staleAttemptFixture {
	t.Helper()
	fixture := &staleAttemptFixture{
		requestID: "req-" + uuid.NewString(),
		// 哈希只要是个稳定的非空串就行：这里验的是回收，不是「换了请求体」那条判定。
		hash:      RequestHash(map[string]string{"probe": uuid.NewString()}),
		paymentID: uuid.NewString(),
		paymentNo: "PAY-IDEM-" + uuid.NewString(),
		orderNo:   "ORD-IDEM-" + uuid.NewString(),
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO payments
		(id, payment_no, order_no, user_id, amount, funding_type, status, request_id, expires_at)
		VALUES ($1,$2,$3,$4,1980,$5,$6,$7, NOW() + INTERVAL '10 minutes')`,
		fixture.paymentID, fixture.paymentNo, fixture.orderNo, uuid.NewString(),
		model.FundingCoffeeBean, status, fixture.requestID); err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	// 幂等行要比 IdempotencyRecoveryWindow 旧，否则回的是「还在处理中」而不是回收。窗口比对的
	// 是 Go 那一侧的 time.Now()（见 beginIdempotentOperation），所以留两倍余量，别让库与进程
	// 之间的时钟差把它推回窗口里面去。
	if _, err := pool.Exec(ctx, `INSERT INTO payment_idempotency_keys
		(scope, idempotency_key, request_hash, resource_type, status, created_at)
		VALUES ($1,$2,$3,'payment','processing', NOW() - $4::interval)`,
		ScopeCreatePayment, fixture.requestID, fixture.hash,
		(2 * IdempotencyRecoveryWindow).String()); err != nil {
		t.Fatalf("insert idempotency key: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 回收会把这行删掉再插一行新的，按 key 删能把两种状态都清掉。
		_, _ = pool.Exec(c, `DELETE FROM payment_idempotency_keys WHERE scope=$1 AND idempotency_key=$2`,
			ScopeCreatePayment, fixture.requestID)
		// 重开成功时新单持着同一个 request_id，所以按 key 也要删得到它。
		_, _ = pool.Exec(c, `DELETE FROM payment_fundings WHERE payment_id IN
			(SELECT id FROM payments WHERE request_id=$1 OR id=$2)`, fixture.requestID, fixture.paymentID)
		_, _ = pool.Exec(c, `DELETE FROM payments WHERE request_id=$1 OR id=$2`,
			fixture.requestID, fixture.paymentID)
	})
	return fixture
}

// beginPaymentProbe 用同一个 request_id 与哈希再发起一次支付，返回仓储的原始输出。
func beginPaymentProbe(t *testing.T, repo *PostgresRepository, requestID, hash string) (*model.Payment, []byte, bool, error) {
	t.Helper()
	return repo.BeginPayment(context.Background(), BeginPaymentParams{
		PaymentNo:   "PAY-IDEM-" + uuid.NewString(),
		OrderNo:     "ORD-IDEM-" + uuid.NewString(),
		UserID:      uuid.NewString(),
		Amount:      1980,
		FundingType: model.FundingCoffeeBean,
		Subject:     "probe",
		RequestID:   requestID,
		RequestHash: hash,
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	})
}

// countPaymentsWithRequestID 数一数有几张单持着这个幂等键——索引保证它最多是一张。
func countPaymentsWithRequestID(t *testing.T, pool *pgxpool.Pool, requestID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payments WHERE request_id=$1`, requestID).Scan(&count); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	return count
}

// TestPostgresBeginPaymentRecyclesAStaleRequestID 陈旧的幂等键要能重开成一张**新**支付单。
//
// 这条用例就是这次修复本身：不把上一次那张单从键上摘下来，重开的 INSERT 必然撞
// payments_request_id_key，回收路径等于不存在。
func TestPostgresBeginPaymentRecyclesAStaleRequestID(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	stale := newStaleAttemptFixture(t, pool, model.PaymentCreated)

	payment, replay, hit, err := beginPaymentProbe(t, repo, stale.requestID, stale.hash)
	if err != nil {
		t.Fatalf("重开一次陈旧幂等键失败：%v", err)
	}
	if hit {
		t.Fatalf("这是一次全新的尝试，不该命中幂等回放")
	}
	if replay != nil {
		t.Fatalf("全新的尝试不该带回放体，got %s", replay)
	}
	if payment == nil || payment.ID == stale.paymentID {
		t.Fatalf("应当建出一张新的支付单，got %+v", payment)
	}
	if payment.RequestID != stale.requestID {
		t.Fatalf("新单的 request_id = %q, want %q（键要跟着新单走）", payment.RequestID, stale.requestID)
	}
	if got := countPaymentsWithRequestID(t, pool, stale.requestID); got != 1 {
		t.Fatalf("持有这个 request_id 的支付单有 %d 张，want 1", got)
	}

	// 老单必须还在，只是不再持有这个键：一次发起过的事实不该因为没人付款就消失。
	var oldRequestID, oldStatus string
	if err := pool.QueryRow(ctx, `SELECT request_id, status FROM payments WHERE id=$1`, stale.paymentID).
		Scan(&oldRequestID, &oldStatus); err != nil {
		t.Fatalf("上一次那张单应当留着，却读不到了：%v", err)
	}
	if oldRequestID != "" {
		t.Fatalf("老单的 request_id = %q，应当被摘空", oldRequestID)
	}
	if oldStatus != model.PaymentCreated {
		t.Fatalf("老单的状态变成了 %q，摘键不该动状态", oldStatus)
	}
}

// TestPostgresBeginPaymentDoesNotDetachASucceededPayment 已经成功收款的单绝不能被摘掉幂等键。
//
// 现场是：渠道回调在两次提交之间插了进来，按 payment_no 把那张 created 的单结掉了，而回调
// 那条路不碰幂等行（见 retireAbandonedPayment 的注释）。这时重开必须**失败**，不能又建一张
// ——键一旦摘掉，这个 request_id 再来就会去找第二张单，而钱已经在第一张上收过了。
//
// 断言的是**错误的形状**而不只是「出错了」：必须是 ErrPaymentNotPending（rpc 层翻成可重试的
// Aborted），不是裸的 pgconn.PgError（那会变成 500，而这件事跟服务器故障无关）。
func TestPostgresBeginPaymentDoesNotDetachASucceededPayment(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	stale := newStaleAttemptFixture(t, pool, model.PaymentSucceeded)

	if _, _, _, err := beginPaymentProbe(t, repo, stale.requestID, stale.hash); !errors.Is(err, ErrPaymentNotPending) {
		t.Fatalf("err = %v, want ErrPaymentNotPending", err)
	}

	var requestID string
	if err := pool.QueryRow(ctx, `SELECT request_id FROM payments WHERE id=$1`, stale.paymentID).
		Scan(&requestID); err != nil {
		t.Fatalf("read payment: %v", err)
	}
	if requestID != stale.requestID {
		t.Fatalf("成功单的 request_id 被摘成了 %q，它与自己的幂等键断了", requestID)
	}
	if got := countPaymentsWithRequestID(t, pool, stale.requestID); got != 1 {
		t.Fatalf("持有这个 request_id 的支付单有 %d 张，want 1", got)
	}
}
