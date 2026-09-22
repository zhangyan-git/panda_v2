package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
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

// settleMembershipSnapshot 是会员行上那份套餐快照。内容照着 membership-service 的
// dto.OrderMembershipSnapshot 写——字段名对不上那边会整条进死信，所以这里刻意写全，
// 少一个字段的改动在这条用例上会立刻显形。
const settleMembershipSnapshot = `{"planId":"7f0f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
	"planCode":"MONTHLY","planName":"连续包月会员","priceCents":990,"period":"month","periodCount":1,
	"autoRenew":true,"memberPriceMode":"coupon","memberPriceCouponTemplateId":"TPL-1",
	"memberPriceCouponsPerPeriod":2}`

// addMembershipLine 往这张订单上补一行会员行。
//
// 不改 newSettlePaymentFixture 的默认形状：那条路（一单只有饮品行）是**绝大多数订单**，
// 而「没有会员段」正是下面那条用例要盯的另一种结果。
func addMembershipLine(t *testing.T, pool *pgxpool.Pool, fixture *settlePaymentFixture) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO order_lines
		(id, order_id, line_no, line_type, item_code, item_name, quantity,
		 original_unit_price, unit_price, payable_amount, membership_plan_snapshot)
		VALUES ($1,$2,2,$3,'MONTHLY','连续包月会员',1,990,990,990,$4::jsonb)`,
		uuid.NewString(), fixture.orderID, model.LineTypeMembership, settleMembershipSnapshot)
	if err != nil {
		t.Fatalf("insert membership line: %v", err)
	}
}

// orderPaidMembership 读回这一单 order.paid 事件里的 membership 段，没有就返回 nil。
func orderPaidMembership(t *testing.T, pool *pgxpool.Pool, traceID string) json.RawMessage {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM message_outbox
		WHERE trace_id=$1 AND event_type=$2`, traceID, EventOrderPaid).Scan(&payload); err != nil {
		t.Fatalf("读 order.paid 载荷: %v", err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("order.paid 载荷解不开: %v", err)
	}
	return event["membership"]
}

// TestPostgresOrderPaidCarriesTheMembershipSnapshot 守住下单买会员这条链的下半截。
//
// 会员域开通会员、发会员价券，靠的全是 order.paid 里的这一段：它不在，钱收了而会员永远
// 开不出来——而且**我们这边一切正常**，没有报错、没有重试、没有指标，只有用户回头来问
// 「我买的会员呢」。所以这一段必须有用例盯着，而不是靠读一遍代码。
//
// 同时钉住两件事：写上去了，以及**是原样搬过去的**。事件里若重新拼一次快照（而不是搬
// order_lines 上那一份），套餐改了价或改了时长就会让用户拿到与下单时不一致的会员——
// 快照存在订单行上的全部意义就是不让这件事发生。
func TestPostgresOrderPaidCarriesTheMembershipSnapshot(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	fixture := newSettlePaymentFixture(t, pool)
	addMembershipLine(t, pool, fixture)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx, traceID := ctxWithTraceID(t, fixture.userID)

	if _, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-member", Amount: settlePayable,
		PaymentMethod: "ums_h5_wechat", Outcome: "payment.succeeded",
		Fundings:  []FundingLine{{LineType: "ums_h5_wechat", Amount: settlePayable, PaymentNo: "PAY-member"}},
		RequestID: uuid.NewString(), TraceID: traceID,
	}); err != nil {
		t.Fatalf("落单: %v", err)
	}

	raw := orderPaidMembership(t, pool, traceID)
	if len(raw) == 0 {
		t.Fatal("order.paid 里没有 membership 段：会员域收不到套餐，这一单永远不会开通会员")
	}
	var got dto.MembershipPlanSnapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("membership 段解不开: %v", err)
	}
	var want dto.MembershipPlanSnapshot
	if err := json.Unmarshal([]byte(settleMembershipSnapshot), &want); err != nil {
		t.Fatalf("夹具快照解不开: %v", err)
	}
	if got != want {
		t.Fatalf("membership 段与订单行上的快照不一致:\n got %+v\nwant %+v", got, want)
	}
	// 少了这几个字段会员域就开不了会员、发不了券，所以单独再说一次——struct 比较已经覆盖，
	// 但失败时要让人一眼看出是哪一类字段丢了。
	if got.PlanID == "" || got.MemberPriceCouponTemplateID == "" || got.MemberPriceCouponsPerPeriod == 0 {
		t.Fatalf("membership 段缺了开通所需的字段: %+v", got)
	}
}

// TestPostgresOrderPaidWithoutMembershipHasNoSegment 是上一条的另一半，而且这一半更重要。
//
// 没有会员行时**不能**出现空壳的 membership 段：会员域开着 DisallowUnknownFields 且把
// 空 planId 当坏消息，{} 发出去会让一单普通咖啡订单的事件进死信——用户付了钱，履约停摆。
// 「没有」的正确表达是这一段根本不存在。
func TestPostgresOrderPaidWithoutMembershipHasNoSegment(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	fixture := newSettlePaymentFixture(t, pool)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx, traceID := ctxWithTraceID(t, fixture.userID)

	if _, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-drink", Amount: settlePayable,
		PaymentMethod: "ums_h5_wechat", Outcome: "payment.succeeded",
		Fundings:  []FundingLine{{LineType: "ums_h5_wechat", Amount: settlePayable, PaymentNo: "PAY-drink"}},
		RequestID: uuid.NewString(), TraceID: traceID,
	}); err != nil {
		t.Fatalf("落单: %v", err)
	}

	raw := orderPaidMembership(t, pool, traceID)
	if len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		t.Fatalf("没有会员行的订单不该带 membership 段，got %s", raw)
	}
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
		PaymentMethod: "ums_h5_wechat", Outcome: "payment.failed",
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
		PaymentMethod: "coffee_bean", Outcome: "payment.succeeded",
		Fundings:  []FundingLine{{LineType: "coffee_bean", Amount: settlePayable, PaymentNo: "PAY-success-1"}},
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
		PaymentMethod: "ums_h5_wechat", Outcome: "payment.failed",
		FailureCode: "CHANNEL_ERROR", FailureMessage: "渠道报错",
		RequestID: uuid.NewString(), TraceID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("失败落单: %v", err)
	}

	// 一笔豆 + 一笔消费金，合起来正好是应付额。
	fundings := []FundingLine{
		{LineType: "coffee_bean", Amount: 2000, PaymentNo: "PAY-bean"},
		{LineType: "ums_h5_upqr", Amount: settlePayable - 2000, PaymentNo: "PAY-wallet"},
	}
	if _, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-mixed", Amount: settlePayable,
		PaymentMethod: "coffee_bean", Outcome: "payment.succeeded",
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

// TestPostgresSettlePaymentStoresOneMethodValue 钉住「一个值写两处」。
//
// orders.payment_method（后台订单列表那一列）与 order_payment_lines.line_type 存的是**同一个
// 值**：用户实际选的那种支付方式，catalog 的 code（如 ums_h5_alipay）。从前这是两套词表——
// 订单表存 code，出资行存「出资渠道」，于是支付宝那条路的 line_type 只能是 other，后台把一笔
// 支付宝单显示成「其他」。那套词表已经退场（见 order/008），两边同值这条不变量值得有用例盯着：
// 它一旦重新分岔，就又要有一种支付方式找不到档位。
//
// 用例刻意**不带 fundings**：那正是订单侧自己补一行的那条路，也就是唯一会把 PaymentMethod
// 直接当 line_type 插进去的地方。
func TestPostgresSettlePaymentStoresOneMethodValue(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	fixture := newSettlePaymentFixture(t, pool)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx, _ := ctxWithTraceID(t, fixture.userID)

	if _, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-alipay", Amount: settlePayable,
		PaymentMethod: "ums_h5_alipay",
		Outcome:       "payment.succeeded",
		RequestID:     uuid.NewString(), TraceID: uuid.NewString(),
	}); err != nil {
		t.Fatalf("落单: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx, `SELECT payment_method FROM orders WHERE id=$1`, fixture.orderID).Scan(&stored); err != nil {
		t.Fatalf("读订单: %v", err)
	}
	if stored != "ums_h5_alipay" {
		t.Errorf("orders.payment_method = %q，期望 ums_h5_alipay（后台列表展示的就是这一列）", stored)
	}

	var lineType string
	if err := pool.QueryRow(ctx,
		`SELECT line_type FROM order_payment_lines WHERE order_id=$1 ORDER BY line_no LIMIT 1`,
		fixture.orderID).Scan(&lineType); err != nil {
		t.Fatalf("读出资流水: %v", err)
	}
	if lineType != stored {
		t.Errorf("line_type = %q，orders.payment_method = %q，两者应当是同一个值", lineType, stored)
	}
}

// TestPostgresSettlePaymentWithoutAMethodIsRejected 钉住「事件没带支付方式」那一格。
//
// 从前这里会回落成 `other`（那套词表的兜底值）。今天 `other` 不再是合法值，而更要紧的是那句
// 话本身：凭空写一个值等于替用户编一句「这笔钱从哪出」。所以这一格为空时整条落单报错——
// 消息进死信由人来看，而不是让订单带着一笔编造的出资记录变成 paid。
//
// 这条路径本该走不到：payments.payment_method 是 NOT NULL。
func TestPostgresSettlePaymentWithoutAMethodIsRejected(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	fixture := newSettlePaymentFixture(t, pool)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx, _ := ctxWithTraceID(t, fixture.userID)

	if _, _, err := repo.SettlePayment(ctx, SettlePaymentParams{
		OrderNo: fixture.orderNo, PaymentNo: "PAY-nomethod", Amount: settlePayable,
		Outcome: "payment.succeeded", RequestID: uuid.NewString(), TraceID: uuid.NewString(),
	}); err == nil {
		t.Fatal("事件没带支付方式却落单成功了，期望报错")
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, fixture.orderID).Scan(&status); err != nil {
		t.Fatalf("读订单: %v", err)
	}
	if status != "pending_payment" {
		t.Errorf("订单状态 = %q，期望仍是 pending_payment（整条事务应当回滚）", status)
	}
}
