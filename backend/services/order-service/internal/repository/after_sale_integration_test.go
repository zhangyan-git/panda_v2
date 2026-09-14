package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// 这些用例只用一个调用方给的库 URL：不建库、不删库、不跑迁移，夹具用唯一的 id，
// 跑完按依赖顺序删掉（照 coupon-service 的集成测试）。
//
// **不删 order_state_transitions**：那张表有 append-only 触发器，DELETE 会直接抛。
// 于是每次跑都会留下几行指向已删订单的流水——这是那张表的性质，不是漏删（要清就整库重建）。
func afterSaleIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("ORDER_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("set ORDER_DATABASE_URL or TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("order test database is unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("order test database is unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ctxWithTraceID 造一个带指定 trace id 与调用方身份的 context。
//
// 两样都不能省，理由都不是为了让用例通过：
//   - trace id：审计与领域事件都从 context 取它落库（audit.TraceIDFromContext 读的是
//     OTel span）。库是共用的 dev 库，按 event_type 筛会把别人的行也算进来，trace_id
//     是这几条用例唯一能把**自己**的行认出来的依据。
//   - 身份：审计里的 actor_id 不由入参决定，而是 audit.Recorder 从 context 的身份里取
//     （audit.ActorIDFromContext）。真请求里那个身份由认证中间件放进去；这里手工补上，
//     否则用例会「通过」却没验到 actor_id——那正是审计最不能空的一栏。
func ctxWithTraceID(t *testing.T, actorID string) (context.Context, string) {
	t.Helper()
	raw := strings.ReplaceAll(uuid.NewString(), "-", "")
	traceID, err := trace.TraceIDFromHex(raw)
	if err != nil {
		t.Fatalf("bad trace id: %v", err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanContext)
	ctx = auth.WithIdentity(ctx, auth.Identity{Subject: actorID, UserID: actorID})
	// 返回 hex 串：库里存的是它，比对时不用再转一次。
	return ctx, traceID.String()
}

type afterSaleFixture struct {
	orderID string
	lineID  string
	userID  string
	// otherLineID 是第二行，用来测「这一行不是这一单的」「同一单的别的行不受影响」。
	otherLineID string
}

// afterSaleOrderFixture 造一张可退的订单（paid、两行、已付清）。
//
// paid_amount = payable_amount 是支付落单那条路的对账结果（见 SettlePayment），这里直接
// 按那个结果造：售后算钱读的就是这两列。
func afterSaleOrderFixture(t *testing.T, pool *pgxpool.Pool, status string, fortuneCards int) *afterSaleFixture {
	t.Helper()
	ctx := context.Background()
	fixture := &afterSaleFixture{
		orderID: uuid.NewString(), lineID: uuid.NewString(),
		otherLineID: uuid.NewString(), userID: uuid.NewString(),
	}
	const payable = 3800
	snapshot := []byte(`{"base": 0}`)
	if fortuneCards > 0 {
		snapshot = []byte(`{"base": 2}`)
	}
	_, err := pool.Exec(ctx, `INSERT INTO orders
		(id, order_no, user_id, source, status, original_amount, payable_amount, paid_amount,
		 fortune_cards_expected, fortune_card_snapshot, paid_at)
		VALUES ($1,$2,$3,'miniapp',$4,$5,$5,$5,$6,$7,NOW())`,
		fixture.orderID, "INT-"+fixture.orderID, fixture.userID, status, payable, fortuneCards, snapshot)
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}
	// 两行各 1900：按行退的金额就是这一行的 payable_amount。
	//
	// 两条约束决定了这两行长什么样，都不是随便挑的：
	//   - 一单一杯：`order_lines_one_drink_per_order`（001:415）只允许一条 drink 行，
	//     所以第二行只能换个类型，这里用加购品——「同一单的另一个可退对象」就是这个意思。
	//   - 加购行必须来自活动：`order_lines_addon_needs_campaign`（001:224）要求
	//     campaign_id 非空。活动在别的服务里，这一列没有外键，给个 uuid 就成立。
	lines := []struct {
		id         string
		lineType   string
		name       string
		campaignID any
	}{
		{fixture.lineID, model.LineTypeDrink, "集成测试饮品", nil},
		{fixture.otherLineID, model.LineTypeAddon, "集成测试加购", uuid.NewString()},
	}
	for i, line := range lines {
		_, err := pool.Exec(ctx, `INSERT INTO order_lines
			(id, order_id, line_no, line_type, item_code, item_name, quantity,
			 original_unit_price, unit_price, payable_amount, campaign_id)
			VALUES ($1,$2,$3,$4,$5,$6,1,1900,1900,1900,$7)`,
			line.id, fixture.orderID, i+1, line.lineType, "INT-"+line.id, line.name, line.campaignID)
		if err != nil {
			t.Fatalf("insert order line: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM order_after_sales WHERE order_id=$1`, fixture.orderID)
		_, _ = pool.Exec(ctx, `DELETE FROM order_lines WHERE order_id=$1`, fixture.orderID)
		_, _ = pool.Exec(ctx, `DELETE FROM order_idempotency_keys WHERE idempotency_key LIKE $1`, "integration-%")
		_, _ = pool.Exec(ctx, `DELETE FROM orders WHERE id=$1`, fixture.orderID)
	})
	return fixture
}

func applyParams(t *testing.T, fixture *afterSaleFixture, scope string, lineID string) ApplyAfterSaleParams {
	t.Helper()
	params := ApplyAfterSaleParams{
		IdempotencyKey: "integration-" + uuid.NewString(),
		OrderID:        fixture.orderID,
		UserID:         fixture.userID,
		AfterSaleNo:    "REF" + uuid.NewString()[:20],
		Scope:          scope,
		Reason:         "集成测试",
		Images:         []byte(`[]`),
		ActorType:      model.ActorUser,
	}
	params.ActorID = &fixture.userID
	if lineID != "" {
		params.OrderLineID = &lineID
	}
	return params
}

// TestPostgresApplyAfterSaleWritesEverySideEffect 一次断言四类副作用：售后单本身、
// 状态流水、领域事件、幂等记录。四者缺一个都会让某条下游或某次重放出问题，而它们在
// 返回值上看不出来。
func TestPostgresApplyAfterSaleWritesEverySideEffect(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx, traceID := ctxWithTraceID(t, fixture.userID)

	params := applyParams(t, fixture, model.AfterSaleScopeAll, "")
	params.TraceID = traceID
	params.RequestID = traceID
	result, replayed, err := repo.ApplyAfterSale(ctx, params)
	if err != nil {
		t.Fatalf("ApplyAfterSale: %v", err)
	}
	if replayed {
		t.Fatal("第一次调用不该是重放")
	}
	// 整单退：金额 = 已付 3800（没有已退、没有别的未结束售后单）。
	if result.AfterSale.RefundAmount != 3800 {
		t.Fatalf("refundAmount = %d, want 3800", result.AfterSale.RefundAmount)
	}
	if result.AfterSale.Status != model.AfterSaleStatusPending || result.AfterSale.OrderNo == "" {
		t.Fatalf("结果不完整: %+v", result)
	}

	var status string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT status, refund_amount FROM order_after_sales WHERE id=$1`,
		result.AfterSale.ID).Scan(&status, &amount); err != nil {
		t.Fatalf("读回售后单: %v", err)
	}
	if status != model.AfterSaleStatusPending || amount != 3800 {
		t.Fatalf("落库的售后单 status=%s amount=%d", status, amount)
	}

	var transitions int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM order_state_transitions
		WHERE aggregate_type='after_sale' AND aggregate_id=$1 AND from_status='' AND to_status='pending'`,
		result.AfterSale.ID).Scan(&transitions); err != nil {
		t.Fatalf("读状态流水: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("状态流水 %d 条，want 1", transitions)
	}

	var events int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM message_outbox
		WHERE trace_id=$1 AND event_type=$2`, traceID, EventAfterSaleApplied).Scan(&events); err != nil {
		t.Fatalf("读 outbox: %v", err)
	}
	if events != 1 {
		t.Fatalf("%s 事件 %d 条，want 1", EventAfterSaleApplied, events)
	}

	// 幂等记录要指着刚落的这张单：重放靠它回放响应。
	var resourceID *string
	if err := pool.QueryRow(ctx, `SELECT resource_id::text FROM order_idempotency_keys
		WHERE scope=$1 AND idempotency_key=$2`, idempotencyScopeAfterSaleApply, params.IdempotencyKey).
		Scan(&resourceID); err != nil {
		t.Fatalf("读幂等记录: %v", err)
	}
	if resourceID == nil || *resourceID != result.AfterSale.ID {
		t.Fatalf("幂等记录的 resource_id = %v, want %s", resourceID, result.AfterSale.ID)
	}
}

func TestPostgresApplyAfterSaleReplaysAndRejectsDifferentBody(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx := context.Background()

	params := applyParams(t, fixture, model.AfterSaleScopeAll, "")
	first, replayed, err := repo.ApplyAfterSale(ctx, params)
	if err != nil || replayed {
		t.Fatalf("第一次 ApplyAfterSale: %+v replayed=%v err=%v", first, replayed, err)
	}

	// 同一个幂等键、同一份内容：回放，不再落第二张单。
	replay, replayed, err := repo.ApplyAfterSale(ctx, params)
	if err != nil {
		t.Fatalf("重放 ApplyAfterSale: %v", err)
	}
	if !replayed || replay.AfterSale.ID != first.AfterSale.ID {
		t.Fatalf("重放没有回放同一张单: %+v replayed=%v", replay, replayed)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM order_after_sales WHERE order_id=$1`,
		fixture.orderID).Scan(&count); err != nil {
		t.Fatalf("数售后单: %v", err)
	}
	if count != 1 {
		t.Fatalf("重放后落了 %d 张售后单，want 1", count)
	}

	// 中间发生了别的事（用户撤销了），再拿原来那个幂等键重放：回的是这张单**现在**的
	// 样子。回放当初存下来的 pending 快照是在对调用方说谎——它会以为申请还在等待审核。
	// 这条曾经是错的：重放走的是幂等表里那份 JSON，没再看库。
	if _, err := repo.CancelAfterSale(ctx, CancelAfterSaleParams{
		AfterSaleNo: first.AfterSale.AfterSaleNo, UserID: fixture.userID,
	}); err != nil {
		t.Fatalf("撤销: %v", err)
	}
	stale, replayed, err := repo.ApplyAfterSale(ctx, params)
	if err != nil {
		t.Fatalf("撤销后重放: %v", err)
	}
	if !replayed {
		t.Fatal("同一幂等键没有再重放")
	}
	if stale.AfterSale.Status != model.AfterSaleStatusCancelled {
		t.Fatalf("重放回的 status = %s, want cancelled（回的是当前状态，不是当初的快照）",
			stale.AfterSale.Status)
	}

	// 同一个键换了原因：这是两次不同的申请，重试也没用。
	changed := params
	changed.Reason = "换了个理由"
	if _, _, err := repo.ApplyAfterSale(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("同键换体 = %v, want ErrIdempotencyConflict", err)
	}
}

func TestPostgresApplyAfterSaleGuards(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	t.Run("没付款的订单不能退", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPendingPayment, 0)
		_, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
		if !errors.Is(err, ErrOrderNotRefundable) {
			t.Fatalf("err = %v, want ErrOrderNotRefundable", err)
		}
	})

	t.Run("别人的单回不存在", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		params := applyParams(t, fixture, model.AfterSaleScopeAll, "")
		params.UserID = uuid.NewString()
		if _, _, err := repo.ApplyAfterSale(ctx, params); !errors.Is(err, ErrOrderNotFound) {
			t.Fatalf("err = %v, want ErrOrderNotFound", err)
		}
	})

	t.Run("这一行不是这一单的", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		params := applyParams(t, fixture, model.AfterSaleScopeDrink, uuid.NewString())
		if _, _, err := repo.ApplyAfterSale(ctx, params); !errors.Is(err, ErrAfterSaleLineMismatch) {
			t.Fatalf("err = %v, want ErrAfterSaleLineMismatch", err)
		}
	})

	t.Run("范围与行的类型对不上", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		// 一条 addon 行、说自己是 drink：金额算得出来、约束也过得去，但这张售后单
		// 上的 scope 会是假的，而下游读的就是 scope。
		params := applyParams(t, fixture, model.AfterSaleScopeDrink, fixture.otherLineID)
		if _, _, err := repo.ApplyAfterSale(ctx, params); !errors.Is(err, ErrAfterSaleLineMismatch) {
			t.Fatalf("err = %v, want ErrAfterSaleLineMismatch", err)
		}
	})

	t.Run("按行退=该行应付额，且与同一行的另一张互斥", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		first, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeDrink, fixture.lineID))
		if err != nil {
			t.Fatalf("按行申请: %v", err)
		}
		if first.AfterSale.RefundAmount != 1900 {
			t.Fatalf("按行退的金额 = %d, want 1900（该行 payable_amount）", first.AfterSale.RefundAmount)
		}
		// 同一行再来一张：已经有未结束的，拒。
		if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeDrink, fixture.lineID)); !errors.Is(err, ErrAfterSaleAlreadyOpen) {
			t.Fatalf("同一行第二次 = %v, want ErrAfterSaleAlreadyOpen", err)
		}
		// 同一单的**另一行**可以退：互斥是按行/整单的，不是按订单的。
		second, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAddon, fixture.otherLineID))
		if err != nil {
			t.Fatalf("另一行申请: %v", err)
		}
		if second.AfterSale.RefundAmount != 1900 {
			t.Fatalf("另一行退的金额 = %d, want 1900", second.AfterSale.RefundAmount)
		}
		// 两行各占 1900，整单再来一张只剩 0 可退（3800-1900-1900），拒。
		if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, "")); !errors.Is(err, ErrAfterSaleAlreadyOpen) {
			t.Fatalf("整单退在有未结束的按行售后时 = %v, want ErrAfterSaleAlreadyOpen", err)
		}
	})

	t.Run("未结束的整单退挡住按行申请", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, "")); err != nil {
			t.Fatalf("整单申请: %v", err)
		}
		if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeDrink, fixture.lineID)); !errors.Is(err, ErrAfterSaleAlreadyOpen) {
			t.Fatalf("err = %v, want ErrAfterSaleAlreadyOpen", err)
		}
	})

	t.Run("已退过款的行不能再退", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		// 直接造一张已退款成功的售后单：那一刻在 payment-service 建了退款单，
		// 这一版走不到，但库里会有（迁移/别的服务写的）。
		if _, err := pool.Exec(ctx, `INSERT INTO order_after_sales
			(after_sale_no, order_id, order_no, user_id, type, scope, order_line_id, status, reason, refund_amount, refunded_at)
			VALUES ($1,$2,$3,$4,'refund','drink',$5,'refunded','集成测试',1900,NOW())`,
			"REF"+uuid.NewString()[:20], fixture.orderID, "INT-"+fixture.orderID, fixture.userID, fixture.lineID); err != nil {
			t.Fatalf("造已退款售后单: %v", err)
		}
		if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeDrink, fixture.lineID)); !errors.Is(err, ErrAfterSaleAlreadyRefunded) {
			t.Fatalf("err = %v, want ErrAfterSaleAlreadyRefunded", err)
		}
	})
}

// TestPostgresReviewAfterSaleFortuneCardGate 是福卡规则的现场：订单承诺过福卡、审核人没
// 确认时**不落库**（状态仍是 pending，也没有审计与事件），确认了才通过。
func TestPostgresReviewAfterSaleFortuneCardGate(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 2)
	reviewer := uuid.NewString()
	ctx, traceID := ctxWithTraceID(t, reviewer)

	applied, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
	if err != nil {
		t.Fatalf("申请: %v", err)
	}

	review := ReviewAfterSaleParams{
		AfterSaleNo: applied.AfterSale.AfterSaleNo,
		Action:      model.AfterSaleActionApprove,
		Remark:      "客服已核对",
		ReviewedBy:  reviewer,
		TraceID:     traceID,
	}
	if _, err := repo.ReviewAfterSale(ctx, review); !errors.Is(err, ErrFortuneCardConfirmationRequired) {
		t.Fatalf("没确认福卡时 = %v, want ErrFortuneCardConfirmationRequired", err)
	}
	var status string
	var reviewedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, reviewed_at FROM order_after_sales WHERE id=$1`,
		applied.AfterSale.ID).Scan(&status, &reviewedAt); err != nil {
		t.Fatalf("读回售后单: %v", err)
	}
	if status != model.AfterSaleStatusPending || reviewedAt != nil {
		t.Fatalf("被福卡规则挡下却改了库: status=%s reviewedAt=%v", status, reviewedAt)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM message_outbox
		WHERE trace_id=$1 AND event_type=$2`, traceID, audit.EventType).Scan(&audits); err != nil {
		t.Fatalf("读 outbox: %v", err)
	}
	if audits != 0 {
		t.Fatalf("被挡下的审核写了 %d 条审计，want 0", audits)
	}

	// 带上确认：这一次真的通过。
	review.FortuneCardUnusedConfirmed = true
	row, err := repo.ReviewAfterSale(ctx, review)
	if err != nil {
		t.Fatalf("带确认的审核: %v", err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusApproved {
		t.Fatalf("status = %s, want approved", row.AfterSale.Status)
	}
	if row.AfterSale.ReviewedBy == nil || *row.AfterSale.ReviewedBy != review.ReviewedBy {
		t.Fatalf("reviewedBy = %v, want %s", row.AfterSale.ReviewedBy, review.ReviewedBy)
	}
	if row.AfterSale.ReviewedAt == nil || row.AfterSale.ReviewRemark != "客服已核对" {
		t.Fatalf("审核字段没落库: %+v", row.AfterSale)
	}
	// 派生列：审核要看的是**订单上**的承诺，不是售后单自己的列。
	if row.FortuneCardsExpected != 2 {
		t.Fatalf("fortuneCardsExpected = %d, want 2", row.FortuneCardsExpected)
	}
	if err := json.Unmarshal(row.FortuneCardSnapshot, &map[string]any{}); err != nil {
		t.Fatalf("福卡快照不是合法 JSON: %v", err)
	}

	// 状态流水：pending→approved，actor 是 admin，metadata 里记着那次确认。
	var metadata []byte
	var actorType string
	if err := pool.QueryRow(ctx, `SELECT actor_type, metadata FROM order_state_transitions
		WHERE aggregate_type='after_sale' AND aggregate_id=$1 AND to_status='approved'`,
		applied.AfterSale.ID).Scan(&actorType, &metadata); err != nil {
		t.Fatalf("读状态流水: %v", err)
	}
	if actorType != model.ActorAdmin {
		t.Fatalf("actor_type = %s, want admin", actorType)
	}
	var decoded struct {
		FortuneCardUnusedConfirmed bool `json:"fortuneCardUnusedConfirmed"`
	}
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		t.Fatalf("流水 metadata 解不开: %v", err)
	}
	if !decoded.FortuneCardUnusedConfirmed {
		t.Fatal("流水里没记下那次福卡确认：事后追查不出这笔退款为什么能放行")
	}

	// 审核事件与平台审计都要在 outbox 里（后者由 relay 落到身份库 admin_operation_logs）。
	var events, auditEntries int
	if err := pool.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE event_type=$2),
		COUNT(*) FILTER (WHERE event_type=$3)
		FROM message_outbox WHERE trace_id=$1`,
		traceID, EventAfterSaleReviewed, audit.EventType).Scan(&events, &auditEntries); err != nil {
		t.Fatalf("读 outbox: %v", err)
	}
	if events != 1 || auditEntries != 1 {
		t.Fatalf("reviewed 事件 %d 条、审计 %d 条，want 1/1", events, auditEntries)
	}

	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM message_outbox
		WHERE trace_id=$1 AND event_type=$2`, traceID, audit.EventType).Scan(&payload); err != nil {
		t.Fatalf("读审计载荷: %v", err)
	}
	var entry audit.Entry
	if err := json.Unmarshal(payload, &entry); err != nil {
		t.Fatalf("审计载荷解不开: %v", err)
	}
	if entry.Module != "after_sales" || entry.Action != model.AfterSaleActionApprove {
		t.Fatalf("审计 module/action = %s/%s", entry.Module, entry.Action)
	}
	if entry.TargetID != applied.AfterSale.ID || entry.ActorID != review.ReviewedBy {
		t.Fatalf("审计没记到这张单/这个人: %+v", entry)
	}
	// before/after 是这条审计存在的理由：不看它，事后只知道「有人点了通过」。
	if !containsJSON(entry.Before, model.AfterSaleStatusPending) ||
		!containsJSON(entry.After, model.AfterSaleStatusApproved) {
		t.Fatalf("审计的 before/after 不完整: before=%s after=%s", entry.Before, entry.After)
	}

	// 再审一次：锁内那次判定会看到 approved，拒。
	if _, err := repo.ReviewAfterSale(ctx, review); !errors.Is(err, ErrAfterSaleNotPending) {
		t.Fatalf("重复审核 = %v, want ErrAfterSaleNotPending", err)
	}
}

// TestPostgresReviewAfterSaleRejectKeepsOrderStatus 盯两件事：驳回要留下理由，且审核
// **不动订单主状态**（规则 11：refunding 的含义是「退款单已经建了」，这一版没人建得了）。
func TestPostgresReviewAfterSaleRejectKeepsOrderStatus(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx := context.Background()

	applied, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
	if err != nil {
		t.Fatalf("申请: %v", err)
	}
	row, err := repo.ReviewAfterSale(ctx, ReviewAfterSaleParams{
		AfterSaleNo: applied.AfterSale.AfterSaleNo,
		Action:      model.AfterSaleActionReject,
		Remark:      "福卡已参与抽奖",
		ReviewedBy:  uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("驳回: %v", err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusRejected || row.AfterSale.ReviewRemark != "福卡已参与抽奖" {
		t.Fatalf("驳回没落库: %+v", row.AfterSale)
	}

	var orderStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1`, fixture.orderID).Scan(&orderStatus); err != nil {
		t.Fatalf("读订单: %v", err)
	}
	if orderStatus != model.OrderStatusPaid {
		t.Fatalf("订单状态被审核改成了 %s，审核通过/驳回应只动售后单", orderStatus)
	}

	// 驳回是终态、不占互斥位：用户重新申请得通。
	if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, "")); err != nil {
		t.Fatalf("驳回后重新申请: %v", err)
	}
}

func TestPostgresCancelAfterSale(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx := context.Background()

	applied, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
	if err != nil {
		t.Fatalf("申请: %v", err)
	}

	// 别人的单：回「不存在」，不是「无权限」。
	if _, err := repo.CancelAfterSale(ctx, CancelAfterSaleParams{
		AfterSaleNo: applied.AfterSale.AfterSaleNo, UserID: uuid.NewString(),
	}); !errors.Is(err, ErrAfterSaleNotFound) {
		t.Fatalf("别人撤销 = %v, want ErrAfterSaleNotFound", err)
	}

	row, err := repo.CancelAfterSale(ctx, CancelAfterSaleParams{
		AfterSaleNo: applied.AfterSale.AfterSaleNo, UserID: fixture.userID, Reason: "用户撤销退款申请",
	})
	if err != nil {
		t.Fatalf("撤销: %v", err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusCancelled {
		t.Fatalf("status = %s, want cancelled", row.AfterSale.Status)
	}
	var actorType string
	if err := pool.QueryRow(ctx, `SELECT actor_type FROM order_state_transitions
		WHERE aggregate_type='after_sale' AND aggregate_id=$1 AND to_status='cancelled'`,
		applied.AfterSale.ID).Scan(&actorType); err != nil {
		t.Fatalf("读状态流水: %v", err)
	}
	if actorType != model.ActorUser {
		t.Fatalf("actor_type = %s, want user（用户自己撤的不该记成 admin）", actorType)
	}

	// 撤销是终态：再撤一次拒，重新申请得通。
	if _, err := repo.CancelAfterSale(ctx, CancelAfterSaleParams{
		AfterSaleNo: applied.AfterSale.AfterSaleNo, UserID: fixture.userID,
	}); !errors.Is(err, ErrAfterSaleNotPending) {
		t.Fatalf("重复撤销 = %v, want ErrAfterSaleNotPending", err)
	}
	if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, "")); err != nil {
		t.Fatalf("撤销后重新申请: %v", err)
	}
}

// TestPostgresListAfterSales 只盯「筛选与分页接对了没有」：列表与详情共用一组列，
// 形状的事由上面的用例管，这里管的是条件。
func TestPostgresListAfterSales(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx := context.Background()

	applied, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
	if err != nil {
		t.Fatalf("申请: %v", err)
	}

	rows, total, err := repo.ListAfterSales(ctx, AfterSaleFilter{AfterSaleNo: applied.AfterSale.AfterSaleNo})
	if err != nil {
		t.Fatalf("按单号筛: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].AfterSale.ID != applied.AfterSale.ID {
		t.Fatalf("按单号筛 = %d 条 (total=%d)", len(rows), total)
	}
	// 整单退没有行摘要，但订单上的福卡承诺（0 张）要看得到。
	if rows[0].OrderLine != nil {
		t.Fatalf("整单退不该有行摘要: %+v", rows[0].OrderLine)
	}

	if _, total, err = repo.ListAfterSales(ctx, AfterSaleFilter{AfterSaleNo: "REF-不存在-" + uuid.NewString()}); err != nil {
		t.Fatalf("筛不存在的单号: %v", err)
	} else if total != 0 {
		t.Fatalf("不存在的单号也匹配到 %d 条", total)
	}

	rows, total, err = repo.ListAfterSales(ctx, AfterSaleFilter{UserID: fixture.userID, Status: model.AfterSaleStatusPending})
	if err != nil {
		t.Fatalf("按用户+状态筛: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("按用户+状态筛 = %d 条 (total=%d)", len(rows), total)
	}
}

// containsJSON 在快照里找某个字符串值的出现。不解析成 map 再逐键比：快照是
// audit.Snapshot 出来的行，键名是列名，但嵌套层数会随类型变，按字节找值更稳，
// 也不会因为多一层包装就漏判。
func containsJSON(raw json.RawMessage, needle string) bool {
	return bytes.Contains(raw, []byte(`"`+needle+`"`))
}
