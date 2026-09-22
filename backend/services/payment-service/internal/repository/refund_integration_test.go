package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 这一组用例守的是退款第一段的分摊：**退多少就冲多少，一分不多**。
//
// 只有真库能验它。判据整个在 BeginRefund 的那条分摊查询与 MarkRefundSucceeded 那条冲正
// UPDATE 里，应用层看不见也补不回来——写错的话任何单元测试都不会红（service 那侧的假仓储
// 直接返回调用方摆好的行），而后果是**按全额退给渠道**：一笔 3000 分的支付、售后按行只该
// 退 1000，渠道那边收到的却是 3000，剩下 2000 的余额还挂在账上可以再退。
//
// 与其它集成用例同一个约定：只用一个调用方给的库 URL，不建库、不删库、不跑迁移，夹具一律用
// 随机 uuid。**payment_state_transitions 跑完不清理**——那张表有 append-only 触发器，DELETE
// 会直接抛；残留行每次都是新的聚合 id，不会让断言互相串台。

// refundablePaymentFixture 是一张收妥待退的支付单，与它那几行出资。
type refundablePaymentFixture struct {
	paymentID string
	paymentNo string
	orderNo   string
	amount    int64
}

// newRefundablePaymentFixture 造一张 status='succeeded' 的支付单，并按 fundings 逐行造出资。
//
// fundings 是「行号 → (来源, 金额)」，按给的顺序落 line_no 1..n。行号必须从 1 连号：
// payment_fundings 上 (payment_id, line_no) 唯一，而分摊是**按 line_no 顺序**吃的，跳号会让
// 用例断言的顺序与生产不一致。
func newRefundablePaymentFixture(t *testing.T, pool *pgxpool.Pool, amount int64, fundings [][2]any) *refundablePaymentFixture {
	t.Helper()
	fixture := &refundablePaymentFixture{
		paymentID: uuid.NewString(),
		paymentNo: "PAY-RF-" + uuid.NewString(),
		orderNo:   "ORD-RF-" + uuid.NewString(),
		amount:    amount,
	}
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO payments
		(id, payment_no, order_no, user_id, amount, status,
		 provider, payment_method, request_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,'succeeded',$6,$7,$8,NOW() + INTERVAL '1 day')`,
		fixture.paymentID, fixture.paymentNo, fixture.orderNo, uuid.NewString(),
		amount, catalog.ChannelCodeUMS, catalog.CodeUMSH5Alipay, "req-"+fixture.paymentID)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	for i, f := range fundings {
		lineType, _ := f[0].(string)
		lineAmount, _ := f[1].(int64)
		if _, err := pool.Exec(ctx, `INSERT INTO payment_fundings
			(payment_id, line_no, line_type, amount, status, succeeded_at)
			VALUES ($1,$2,$3,$4,'succeeded',NOW())`,
			fixture.paymentID, i+1, lineType, lineAmount); err != nil {
			t.Fatalf("insert funding %d: %v", i+1, err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// 退款那两张表对出资行与支付单都是 ON DELETE RESTRICT，先删它们。
		_, _ = pool.Exec(ctx, `DELETE FROM payment_refund_fundings WHERE refund_id IN
			(SELECT id FROM payment_refunds WHERE payment_id=$1)`, fixture.paymentID)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_refunds WHERE payment_id=$1`, fixture.paymentID)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_fundings WHERE payment_id=$1`, fixture.paymentID)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_transactions WHERE payment_no=$1`, fixture.paymentNo)
		_, _ = pool.Exec(ctx, `DELETE FROM payments WHERE id=$1`, fixture.paymentID)
	})
	return fixture
}

// beginRefund 调一次 BeginRefund，任何 error 都直接判失败。
func beginRefund(t *testing.T, repo *PostgresRepository, fixture *refundablePaymentFixture, amount int64) (*model.Refund, []*model.RefundFunding) {
	t.Helper()
	refund, fundings, replayed, err := repo.BeginRefund(context.Background(), BeginRefundParams{
		RefundNo:    "REF-" + uuid.NewString(),
		PaymentNo:   fixture.paymentNo,
		AfterSaleNo: "AS-" + uuid.NewString(),
		Amount:      amount,
		Reason:      "integration test",
		RequestID:   "req-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("BeginRefund(%d): %v", amount, err)
	}
	if replayed {
		t.Fatalf("BeginRefund(%d) reported an idempotency hit on a fresh after-sale-no", amount)
	}
	return refund, fundings
}

// allocation 把退款出资行收成「行号 → 金额」，断言时按行比对而不是按切片下标。
func allocation(fundings []*model.RefundFunding) map[int]int64 {
	out := make(map[int]int64, len(fundings))
	for _, f := range fundings {
		out[f.LineNo] = f.Amount
	}
	return out
}

// fundingStatus 读回某一行出资的状态。
func fundingStatus(t *testing.T, pool *pgxpool.Pool, paymentID string, lineNo int) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM payment_fundings WHERE payment_id=$1 AND line_no=$2`,
		paymentID, lineNo).Scan(&status); err != nil {
		t.Fatalf("read funding %d: %v", lineNo, err)
	}
	return status
}

// TestPostgresPartialRefundAllocatesOnlyTheRequestedAmount 是这条链上最贵的那一条回归。
//
// 分摊写错（照抄出资行的全额）时，一笔 3000 分的支付按行退 1000，退款出资行会是 3000——
// 而发起退款时发给渠道的金额正是把这些行累加出来的（service/refund.go 的
// splitRefundFundings），于是渠道收到 3000，剩下 2000 的余额还挂在账上可以再退。
func TestPostgresPartialRefundAllocatesOnlyTheRequestedAmount(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := newRefundablePaymentFixture(t, pool, 3000, [][2]any{
		{catalog.CodeUMSH5Alipay, int64(3000)},
	})

	_, fundings := beginRefund(t, repo, fixture, 1000)

	got := allocation(fundings)
	if len(got) != 1 || got[1] != 1000 {
		t.Fatalf("partial refund of 1000 on a 3000 payment allocated %v, want {1:1000}", got)
	}
}

// TestPostgresPartialRefundSpreadsAcrossFundingsInLineOrder 混合出资（渠道 + 豆）时的分摊。
//
// 退 2500 要横跨两行：第一行 2000 全吃下，第二行吃 500。两个来源各退各的那一份是这条链
// 存在的意义——账户那一份不能跑到渠道去，反之亦然。
func TestPostgresPartialRefundSpreadsAcrossFundingsInLineOrder(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := newRefundablePaymentFixture(t, pool, 3000, [][2]any{
		{catalog.CodeUMSH5Alipay, int64(2000)},
		{"coffee_bean", int64(1000)},
	})

	_, fundings := beginRefund(t, repo, fixture, 2500)

	got := allocation(fundings)
	if len(got) != 2 || got[1] != 2000 || got[2] != 500 {
		t.Fatalf("refund of 2500 across 2000+1000 allocated %v, want {1:2000, 2:500}", got)
	}
}

// TestPostgresSecondPartialRefundAllocatesTheRemainderAndThenReverses 两次按行退打在同一笔
// 支付上——这正是售后按行退的正常形状，也是「照抄全额」那个 bug 最容易被触发的路径。
//
// 三件事必须同时成立：
//   - 第二次只分摊剩下的 2000，不会把已经退掉的 1000 再退一遍；
//   - 退到一半时出资行**仍然是 succeeded**（钱还没退干净，置 reversed 会让对账以为冲光了）；
//   - 退干净那一刻它才变 reversed。
func TestPostgresSecondPartialRefundAllocatesTheRemainderAndThenReverses(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()
	fixture := newRefundablePaymentFixture(t, pool, 3000, [][2]any{
		{catalog.CodeUMSH5Alipay, int64(3000)},
	})

	first, firstFundings := beginRefund(t, repo, fixture, 1000)
	if _, err := repo.MarkRefundSucceeded(ctx, MarkRefundSucceededParams{
		RefundID: first.ID, ProviderRefundID: "PRF-1",
		SucceededAt: time.Now().UTC(), NoOpLineTypes: []string{},
	}); err != nil {
		t.Fatalf("MarkRefundSucceeded(first): %v", err)
	}
	if got := allocation(firstFundings); got[1] != 1000 {
		t.Fatalf("first refund allocated %v, want {1:1000}", got)
	}
	if status := fundingStatus(t, pool, fixture.paymentID, 1); status != "succeeded" {
		t.Fatalf("funding after refunding 1000 of 3000 = %q, want %q (it is not fully returned yet)",
			status, "succeeded")
	}

	second, secondFundings := beginRefund(t, repo, fixture, 2000)
	got := allocation(secondFundings)
	if len(got) != 1 || got[1] != 2000 {
		t.Fatalf("second refund of 2000 allocated %v, want {1:2000} (only the remainder is left)", got)
	}
	if _, err := repo.MarkRefundSucceeded(ctx, MarkRefundSucceededParams{
		RefundID: second.ID, ProviderRefundID: "PRF-2",
		SucceededAt: time.Now().UTC(), NoOpLineTypes: []string{},
	}); err != nil {
		t.Fatalf("MarkRefundSucceeded(second): %v", err)
	}
	if status := fundingStatus(t, pool, fixture.paymentID, 1); status != "reversed" {
		t.Fatalf("funding after refunding 1000+2000 of 3000 = %q, want %q", status, "reversed")
	}
}

// TestPostgresRefundCannotExceedTheUnreturnedPartOfAFunding 第二笔退得比剩下的还多时必须
// 被可退余额挡下来——挡不住的话分摊会从已经退光的出资行上再吃一次。
//
// 这条同时验 4 与 5 的边界：第一笔 1000 成功了，剩下的可退额是 2000，第 3 笔要 2500 必须
// 以 ErrRefundExceedsRefundable 被拒。
func TestPostgresRefundCannotExceedTheUnreturnedPartOfAFunding(t *testing.T) {
	pool := paymentIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()
	fixture := newRefundablePaymentFixture(t, pool, 3000, [][2]any{
		{catalog.CodeUMSH5Alipay, int64(3000)},
	})

	first, _ := beginRefund(t, repo, fixture, 1000)
	if _, err := repo.MarkRefundSucceeded(ctx, MarkRefundSucceededParams{
		RefundID: first.ID, ProviderRefundID: "PRF-1",
		SucceededAt: time.Now().UTC(), NoOpLineTypes: []string{},
	}); err != nil {
		t.Fatalf("MarkRefundSucceeded(first): %v", err)
	}

	_, _, _, err := repo.BeginRefund(ctx, BeginRefundParams{
		RefundNo:    "REF-" + uuid.NewString(),
		PaymentNo:   fixture.paymentNo,
		AfterSaleNo: "AS-" + uuid.NewString(),
		Amount:      2500,
		Reason:      "integration test",
		RequestID:   "req-" + uuid.NewString(),
	})
	if err == nil {
		t.Fatal("BeginRefund(2500) succeeded after 1000 was already refunded; want it rejected")
	}
	if !errors.Is(err, ErrRefundExceedsRefundable) {
		t.Fatalf("BeginRefund(2500) error = %v, want ErrRefundExceedsRefundable", err)
	}
}
