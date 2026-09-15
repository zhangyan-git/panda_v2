package repository

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// settlePaymentFixture 是一笔等待支付的订单：一张主表、一行饮品，没别的。
//
// 与 after_sale 那边的夹具不同，这里刻意**不给** order_payment_lines 预置任何行——
// 出资流水正是这几条用例要观察的对象。
type settlePaymentFixture struct {
	orderID string
	orderNo string
	userID  string
}

const settlePayable = 3800

func newSettlePaymentFixture(t *testing.T, pool *pgxpool.Pool) *settlePaymentFixture {
	t.Helper()
	ctx := context.Background()
	fixture := &settlePaymentFixture{
		orderID: uuid.NewString(),
		orderNo: "INT-" + uuid.NewString(),
		userID:  uuid.NewString(),
	}
	_, err := pool.Exec(ctx, `INSERT INTO orders
		(id, order_no, user_id, source, status, original_amount, payable_amount, paid_amount)
		VALUES ($1,$2,$3,'miniapp','pending_payment',$4,$4,0)`,
		fixture.orderID, fixture.orderNo, fixture.userID, settlePayable)
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO order_lines
		(id, order_id, line_no, line_type, item_code, item_name, quantity,
		 original_unit_price, unit_price, payable_amount)
		VALUES ($1,$2,1,$3,'INT-drink','集成测试饮品',1,$4,$4,$4)`,
		uuid.NewString(), fixture.orderID, model.LineTypeDrink, settlePayable)
	if err != nil {
		t.Fatalf("insert order line: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 出资流水对 orders 是 ON DELETE RESTRICT，必须先删它。
		// order_state_transitions 不删：那张表有 append-only 触发器，DELETE 会直接抛，
		// 所以每次跑都会留下几行（见 after_sale_integration_test.go 的同一条说明）。
		_, _ = pool.Exec(ctx, `DELETE FROM order_payment_lines WHERE order_id=$1`, fixture.orderID)
		_, _ = pool.Exec(ctx, `DELETE FROM order_lines WHERE order_id=$1`, fixture.orderID)
		_, _ = pool.Exec(ctx, `DELETE FROM orders WHERE id=$1`, fixture.orderID)
	})
	return fixture
}

// settlePaymentLines 读回这张订单上的出资流水行号，按行号排好。
func settlePaymentLines(t *testing.T, pool *pgxpool.Pool, orderID string) []struct {
	lineNo int
	status string
} {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT line_no, status FROM order_payment_lines WHERE order_id=$1 ORDER BY line_no`, orderID)
	if err != nil {
		t.Fatalf("query payment lines: %v", err)
	}
	defer rows.Close()
	var out []struct {
		lineNo int
		status string
	}
	for rows.Next() {
		var row struct {
			lineNo int
			status string
		}
		if err := rows.Scan(&row.lineNo, &row.status); err != nil {
			t.Fatalf("scan payment line: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate payment lines: %v", err)
	}
	return out
}

// TestPostgresSettlePaymentAfterAFailedAttemptKeepsGoing 守住「失败的支付尝试之后还能接着付」。
//
// 这是最普通的一条路：用户用微信付失败（订单仍是 pending_payment，只是多留了一行
// status='failed' 的出资流水），然后改用咖啡豆付成功。两条路径各自算 line_no——
// 失败那条接着 MAX 往下排，成功那条早先从 1 重数——于是第二次落单撞上
// UNIQUE(order_id, line_no)，整个事务回滚。
//
// 后果不对称，所以必须有用例：钱在支付侧已经收了，本地订单却永远停在 pending_payment，
// 而那条失败行还在，事件重放也一样撞，只能人工改数据。这个错在任何单元测试里都看不见，
// 只有真库上的唯一索引会拦。
func TestPostgresSettlePaymentAfterAFailedAttemptKeepsGoing(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	fixture := newSettlePaymentFixture(t, pool)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx, _ := ctxWithTraceID(t, fixture.userID)

	failed, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-failed-1", Amount: settlePayable,
		PaymentMethod: model.FundingWechat, Outcome: "payment.failed",
		FailureCode: "USER_CANCEL", FailureMessage: "用户取消支付",
		RequestID: uuid.NewString(), TraceID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("首次失败落单: %v", err)
	}
	if failed.Status != "pending_payment" {
		t.Fatalf("一次支付失败不该动主状态，got %q", failed.Status)
	}

	// 换一种方式再付。修复前这一步会撞 order_payment_lines 的唯一索引。
	paid, applied, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-success-1", Amount: settlePayable,
		PaymentMethod: model.FundingCoffeeBean, Outcome: "payment.succeeded",
		Fundings:  []FundingLine{{LineType: model.FundingCoffeeBean, Amount: settlePayable, PaymentNo: "PAY-success-1"}},
		RequestID: uuid.NewString(), TraceID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("失败之后重付: %v", err)
	}
	if !applied || paid.Status != "paid" {
		t.Fatalf("重付后订单应为 paid，got %+v", paid)
	}

	lines := settlePaymentLines(t, pool, fixture.orderID)
	if len(lines) != 2 {
		t.Fatalf("出资流水应有 2 行（一次失败、一次成功），got %+v", lines)
	}
	// 行号必须严格递增且不重复：唯一索引保证不重复，「接着最大值排」保证有序。
	if lines[0].lineNo != 1 || lines[1].lineNo != 2 {
		t.Fatalf("行号应为 1,2，got %+v", lines)
	}
	if lines[0].status != model.PaymentLineFailed || lines[1].status != model.PaymentLineSucceeded {
		t.Fatalf("流水状态应为 failed,succeeded，got %+v", lines)
	}
}

// TestPostgresSettlePaymentNumbersFundingsAfterExistingLines 守住分摊多笔时的行号。
//
// 一次成功可以有多笔出资（微信 + 咖啡豆混合支付），它们整体接着已有的最大值往下排，
// 而不是每一笔都问一次数据库——两者在单笔时看不出区别，多笔时才会撞。
func TestPostgresSettlePaymentNumbersFundingsAfterExistingLines(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	fixture := newSettlePaymentFixture(t, pool)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx, _ := ctxWithTraceID(t, fixture.userID)

	if _, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-failed-1", Amount: settlePayable,
		PaymentMethod: model.FundingWechat, Outcome: "payment.failed",
		FailureCode: "CHANNEL_ERROR", FailureMessage: "渠道报错",
		RequestID: uuid.NewString(), TraceID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("失败落单: %v", err)
	}

	// 一笔豆 + 一笔消费金，合起来正好是应付额。
	fundings := []FundingLine{
		{LineType: model.FundingCoffeeBean, Amount: 2000, PaymentNo: "PAY-bean"},
		{LineType: model.FundingWallet, Amount: settlePayable - 2000, PaymentNo: "PAY-wallet"},
	}
	if _, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-mixed", Amount: settlePayable,
		PaymentMethod: model.FundingCoffeeBean, Outcome: "payment.succeeded",
		Fundings: fundings, RequestID: uuid.NewString(), TraceID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("混合支付落单: %v", err)
	}

	lines := settlePaymentLines(t, pool, fixture.orderID)
	if len(lines) != 3 {
		t.Fatalf("出资流水应有 3 行，got %+v", lines)
	}
	for i, want := range []int{1, 2, 3} {
		if lines[i].lineNo != want {
			t.Fatalf("第 %d 行行号应为 %d，got %+v", i, want, lines)
		}
	}
}
