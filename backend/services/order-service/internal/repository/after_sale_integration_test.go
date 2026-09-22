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
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
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
//
// payment_no 必须给：申请退款那条路**在入口就收窄**（见 ApplyAfterSale 里的
// ErrOrderHasNoPayment）——没有支付单的订单（设备单）根本走不到退款，而这一整套夹具描述的
// 是「一张能退的正常订单」。要测那条例外用 clearOrderPaymentNo。
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
		 fortune_cards_expected, fortune_card_snapshot, paid_at, payment_method, payment_no)
		VALUES ($1,$2,$3,'miniapp',$4,$5,$5,$5,$6,$7,NOW(),'wechat_miniapp',$8)`,
		fixture.orderID, "INT-"+fixture.orderID, fixture.userID, status, payable, fortuneCards, snapshot,
		"PAY-"+fixture.orderID)
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

	t.Run("没有支付单的订单不能退", func(t *testing.T) {
		// 线下刷卡机与取货码那两类设备单：钱在机器上收过了，订单库里没有 payments 行。
		// 退款这条链**结构上**装不下它们（payment_refunds.payment_id 是 NOT NULL），
		// 所以在这里就拒——放到审核之后再炸，用户拿到的是一句「已同意退款」而钱退不出去。
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		if _, err := pool.Exec(ctx, `UPDATE orders SET payment_no='' WHERE id=$1`, fixture.orderID); err != nil {
			t.Fatalf("清 payment_no: %v", err)
		}
		_, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
		if !errors.Is(err, ErrOrderHasNoPayment) {
			t.Fatalf("err = %v, want ErrOrderHasNoPayment", err)
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

// TestPostgresFortuneCardFreezeGate 验受理之前那次只读预看的两件事：**要冻哪几笔**（按退款
// 范围分层）与**什么时候不该做这个判断**。
//
// 这一问的分量不在它自己身上，而在它下游：订单域拿 EntryKeys 去问账户域「这几笔还冻得上
// 吗」，答不上来的申请会被拒。所以键算错（退加购行却给了 base 那张）等于拿着别人的答案
// 去回答这一单的问题，而两个方向都会错——多要一张会把一条本来能退的单拒掉，少要一张会
// 把追不回来的卡退出去。
//
// 下半段（checkable=false 的三种）同样重要：它们是「这次判断做不了，别拿它去回一句话」。
// 少了这道闸，一张还没付款的订单来申请退款会收到「福卡已使用」——一句不相干的冤枉话，
// 而且是在真正的拒绝理由（订单不可退）之前说出来的。
func TestPostgresFortuneCardFreezeGate(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	// 一张承诺 3 张的快照：基础 2 张 + 某次加购活动加赠 1 张。夹具默认写的是只有 base 的
	// 快照，这里换成能拆出两层的，才测得到「按范围分层」。
	newSplitOrder := func(t *testing.T, status string) *afterSaleFixture {
		t.Helper()
		fixture := afterSaleOrderFixture(t, pool, status, 3)
		if _, err := pool.Exec(ctx, `UPDATE orders SET fortune_card_snapshot = $2 WHERE id = $1`,
			fixture.orderID, []byte(`{"base":2,"bonus":{"id":"campaign-gate","name":"加赠活动","reward":1}}`)); err != nil {
			t.Fatalf("改快照: %v", err)
		}
		return fixture
	}

	t.Run("整单退冻两笔、共三张", func(t *testing.T) {
		fixture := newSplitOrder(t, model.OrderStatusPaid)
		plan, checkable, err := repo.FortuneCardFreezeGate(ctx, fixture.orderID, fixture.userID, model.AfterSaleScopeAll)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if !checkable {
			t.Fatal("一张已付款、有福卡的订单说不该判")
		}
		want := []string{baseGrantKey(fixture.orderID), bonusGrantKey(fixture.orderID, "campaign-gate")}
		if len(plan.EntryKeys) != len(want) {
			t.Fatalf("entryKeys = %v, want %v", plan.EntryKeys, want)
		}
		for i := range want {
			if plan.EntryKeys[i] != want[i] {
				t.Fatalf("entryKeys = %v, want %v", plan.EntryKeys, want)
			}
		}
		if plan.Cards != 3 {
			t.Fatalf("cards = %d, want 3（张数必须与键出自同一次拆分）", plan.Cards)
		}
	})

	t.Run("退饮品行只冻基础那张", func(t *testing.T) {
		fixture := newSplitOrder(t, model.OrderStatusPaid)
		plan, checkable, err := repo.FortuneCardFreezeGate(ctx, fixture.orderID, fixture.userID, model.AfterSaleScopeDrink)
		if err != nil || !checkable {
			t.Fatalf("gate: %v / checkable=%v", err, checkable)
		}
		if len(plan.EntryKeys) != 1 || plan.EntryKeys[0] != baseGrantKey(fixture.orderID) {
			t.Fatalf("entryKeys = %v, want 只有基础那一笔", plan.EntryKeys)
		}
		// Cards 与 EntryKeys 同源：退加购行时拿整单的 3 张去比一张卡，会让判据整体偏松。
		if plan.Cards != 2 {
			t.Fatalf("cards = %d, want 2", plan.Cards)
		}
	})

	t.Run("退加购行只冻加赠那张", func(t *testing.T) {
		fixture := newSplitOrder(t, model.OrderStatusPaid)
		plan, checkable, err := repo.FortuneCardFreezeGate(ctx, fixture.orderID, fixture.userID, model.AfterSaleScopeAddon)
		if err != nil || !checkable {
			t.Fatalf("gate: %v / checkable=%v", err, checkable)
		}
		if len(plan.EntryKeys) != 1 || plan.EntryKeys[0] != bonusGrantKey(fixture.orderID, "campaign-gate") {
			t.Fatalf("entryKeys = %v, want 只有加赠那一笔", plan.EntryKeys)
		}
		if plan.Cards != 1 {
			t.Fatalf("cards = %d, want 1", plan.Cards)
		}
	})

	t.Run("已完成但还没发卡的单仍然要判", func(t *testing.T) {
		// completed 是发卡事件的那一刻；发卡落库之前先申请退款是支持的（那时账户域答 0/0，
		// 由订单域判成「还没发」放行）。所以这个状态必须 checkable，不能当成「没什么可冻的」。
		fixture := newSplitOrder(t, model.OrderStatusCompleted)
		plan, checkable, err := repo.FortuneCardFreezeGate(ctx, fixture.orderID, fixture.userID, model.AfterSaleScopeAll)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		if !checkable || plan.Cards != 3 {
			t.Fatalf("checkable=%v cards=%d, want true/3", checkable, plan.Cards)
		}
	})

	t.Run("没承诺福卡的单不判", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		plan, checkable, err := repo.FortuneCardFreezeGate(ctx, fixture.orderID, fixture.userID, model.AfterSaleScopeAll)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		// checkable 仍为 true：这一单可判，只是判出来是空的。调用方看的是 Cards > 0，
		// 所以这里不把「没福卡」说成「不该判」——那是两件事，混了会让日志里读不出原因。
		if !checkable || plan.Cards != 0 || len(plan.EntryKeys) != 0 {
			t.Fatalf("checkable=%v plan=%+v, want true 且空计划", checkable, plan)
		}
	})

	t.Run("别人的单、没付款的单、不存在的单都不判", func(t *testing.T) {
		paid := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 2)
		unpaid := afterSaleOrderFixture(t, pool, model.OrderStatusPendingPayment, 2)
		cases := []struct {
			name    string
			orderID string
			userID  string
		}{
			{"不是本人的", paid.orderID, uuid.NewString()},
			{"还没付款的", unpaid.orderID, unpaid.userID},
			{"查不到的单", uuid.NewString(), uuid.NewString()},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				plan, checkable, err := repo.FortuneCardFreezeGate(ctx, tc.orderID, tc.userID, model.AfterSaleScopeAll)
				if err != nil {
					// 「不存在」不是错误：这是一次查询，不是一次写入。报错会让调用方
					// 把「查无此单」翻成 5xx，而它真正的答复在下面的 ApplyAfterSale 里。
					t.Fatalf("gate 报错了: %v", err)
				}
				if checkable {
					t.Fatal("这三种情形都不该在这里下结论")
				}
				if len(plan.EntryKeys) != 0 || plan.Cards != 0 {
					t.Fatalf("不该判却给了计划: %+v", plan)
				}
			})
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

// —— 退款推进（StartRefund / AdvanceRefund，见 after_sale.go 里那两段）——

// approvedAfterSale 走完「申请 → 审核通过」，返回一张 approved 的售后单。
//
// 它停在 approved 而不是 refunding，是因为这两条是**仓储层**的两段：审核（事务 A）与
// 发起退款（事务 B）中间隔着一次调支付域的网络调用，而仓储只管自己那两段。
func approvedAfterSale(t *testing.T, repo *PostgresRepository,
	ctx context.Context, fixture *afterSaleFixture, scope, lineID string) *AfterSaleRow {
	t.Helper()
	applied, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, scope, lineID))
	if err != nil {
		t.Fatalf("申请: %v", err)
	}
	reviewed, err := repo.ReviewAfterSale(ctx, ReviewAfterSaleParams{
		AfterSaleNo: applied.AfterSale.AfterSaleNo,
		Action:      model.AfterSaleActionApprove,
		Remark:      "客服已核对",
		ReviewedBy:  uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("审核: %v", err)
	}
	if reviewed.AfterSale.Status != model.AfterSaleStatusApproved {
		t.Fatalf("审核后的状态 = %s, want approved", reviewed.AfterSale.Status)
	}
	return reviewed
}

func orderStatusOf(t *testing.T, pool *pgxpool.Pool, orderID string) (string, int64) {
	t.Helper()
	var status string
	var refunded int64
	if err := pool.QueryRow(context.Background(),
		`SELECT status, refunded_amount FROM orders WHERE id=$1`, orderID).Scan(&status, &refunded); err != nil {
		t.Fatalf("读订单: %v", err)
	}
	return status, refunded
}

// outboxEventsForSale 数一张售后单下某个主题的 outbox 行。
//
// 按**售后单号**而不是 trace 数：这张单上一次结论只能有一条这样的事件，重投多发一条在这里
// 现形，而按 trace 数会把「重投那条没带 trace」的那种多发悄悄放过。
func outboxEventsForSale(t *testing.T, pool *pgxpool.Pool, eventType, afterSaleNo string) int {
	t.Helper()
	var count int
	// payload 是 bytea（信封整体序列化后的字节），要读字段得先转成文本再当 jsonb 解。
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM message_outbox
		WHERE event_type=$1 AND (convert_from(payload,'UTF8')::jsonb)->>'afterSaleNo'=$2`,
		eventType, afterSaleNo).Scan(&count); err != nil {
		t.Fatalf("读 outbox: %v", err)
	}
	return count
}

// outboxEvents 数一条 trace 下某个主题的 outbox 行。
//
// 退款结果这两条事件是**别的域唯一能知道钱退成没退成的信号**（account-service 靠它们决定
// 追回还是解冻福卡），所以断言不能停在「售后单状态变了」——那件事下游看不见。发没发出去、
// 发的哪一条，只能从 outbox 里读。
func outboxEvents(t *testing.T, pool *pgxpool.Pool, traceID, eventType string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM message_outbox
		WHERE trace_id=$1 AND event_type=$2`, traceID, eventType).Scan(&count); err != nil {
		t.Fatalf("读 outbox: %v", err)
	}
	return count
}

// outboxPayload 读某条主题的载荷，用来验字段（尤其是金额与时刻）。
func outboxPayload(t *testing.T, pool *pgxpool.Pool, traceID, eventType string) []byte {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM message_outbox
		WHERE trace_id=$1 AND event_type=$2`, traceID, eventType).Scan(&payload); err != nil {
		t.Fatalf("读 outbox 载荷: %v", err)
	}
	return payload
}

// TestPostgresStartRefundMovesOrderAndIsIdempotent 是事务 B 的现场：售后单从 approved
// 走到 refunding、退款单号落下来、订单跟着进退款中，而重复推一次不会写出第二段历史。
func TestPostgresStartRefundMovesOrderAndIsIdempotent(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx, traceID := ctxWithTraceID(t, uuid.NewString())

	approved := approvedAfterSale(t, repo, ctx, fixture, model.AfterSaleScopeAll, "")
	refundNo := "RF" + uuid.NewString()[:20]

	row, replayed, err := repo.StartRefund(ctx, StartRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo,
		RefundNo:    refundNo, ActorID: uuid.NewString(), RequestID: traceID,
	})
	if err != nil {
		t.Fatalf("StartRefund: %v", err)
	}
	if replayed {
		t.Fatal("第一次推进不该是重放")
	}
	if row.AfterSale.Status != model.AfterSaleStatusRefunding || row.AfterSale.RefundNo != refundNo {
		t.Fatalf("售后单 = %s / %s, want refunding / %s", row.AfterSale.Status, row.AfterSale.RefundNo, refundNo)
	}
	if status, _ := orderStatusOf(t, pool, fixture.orderID); status != model.OrderStatusRefunding {
		t.Fatalf("订单状态 = %s, want refunding", status)
	}

	// 重放：同一张售后单、同一个退款单号。它**什么都不写**——多写一条 refunding→refunding
	// 的流水会让「这张单被推过几次」查不出来。
	again, replayed, err := repo.StartRefund(ctx, StartRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo, ActorID: uuid.NewString(),
	})
	if err != nil || !replayed {
		t.Fatalf("重放 StartRefund: replayed=%v err=%v", replayed, err)
	}
	if again.AfterSale.RefundNo != refundNo {
		t.Fatalf("重放回的退款单号 = %s, want %s", again.AfterSale.RefundNo, refundNo)
	}

	// 同一个售后单换一个退款单号：这不是重放，是「这张单上记的是另一张退款单」，要人来看。
	if _, _, err := repo.StartRefund(ctx, StartRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: "RF-other", ActorID: uuid.NewString(),
	}); !errors.Is(err, ErrAfterSaleRefundMismatch) {
		t.Fatalf("换退款单号 = %v, want ErrAfterSaleRefundMismatch", err)
	}
}

// TestPostgresStartRefundOnlyFromApproved：待审核与已结束（驳回/撤销/失败）的单都推不动。
// 前者是绕过审核，后者是同一笔钱的第二张退款单。
func TestPostgresStartRefundOnlyFromApproved(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	ctx := context.Background()

	t.Run("还没审核通过", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		applied, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
		if err != nil {
			t.Fatalf("申请: %v", err)
		}
		_, _, err = repo.StartRefund(ctx, StartRefundParams{
			AfterSaleNo: applied.AfterSale.AfterSaleNo, RefundNo: "RF1",
		})
		if !errors.Is(err, ErrAfterSaleNotApproved) {
			t.Fatalf("err = %v, want ErrAfterSaleNotApproved", err)
		}
		if status, _ := orderStatusOf(t, pool, fixture.orderID); status != model.OrderStatusPaid {
			t.Fatalf("订单状态被改成了 %s", status)
		}
	})

	t.Run("已经驳回", func(t *testing.T) {
		fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
		applied, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, ""))
		if err != nil {
			t.Fatalf("申请: %v", err)
		}
		if _, err := repo.ReviewAfterSale(ctx, ReviewAfterSaleParams{
			AfterSaleNo: applied.AfterSale.AfterSaleNo, Action: model.AfterSaleActionReject, Remark: "不符合规则",
			// 审核人不能空：reviewed_by 是 uuid 列，空串会被 PG 当场拒掉（而这条路只有
			// 后台走得到，身份一定在令牌里——见 controller.review 从 identity 取审核人）。
			ReviewedBy: uuid.NewString(),
		}); err != nil {
			t.Fatalf("驳回: %v", err)
		}
		_, _, err = repo.StartRefund(ctx, StartRefundParams{
			AfterSaleNo: applied.AfterSale.AfterSaleNo, RefundNo: "RF1",
		})
		if !errors.Is(err, ErrAfterSaleNotApproved) {
			t.Fatalf("err = %v, want ErrAfterSaleNotApproved", err)
		}
	})
}

// TestPostgresAdvanceRefundSucceedsOnAWholeOrder：整单退成功 → 售后 refunded（记退款时刻）、
// 订单 refunded、已退金额累加。事件重投一次不会退两次钱。
func TestPostgresAdvanceRefundSucceedsOnAWholeOrder(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx, traceID := ctxWithTraceID(t, uuid.NewString())

	approved := approvedAfterSale(t, repo, ctx, fixture, model.AfterSaleScopeAll, "")
	refundNo := "RF" + uuid.NewString()[:20]
	if _, _, err := repo.StartRefund(ctx, StartRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo,
	}); err != nil {
		t.Fatalf("StartRefund: %v", err)
	}

	refundedAt := time.Now().UTC().Truncate(time.Second)
	// TraceID 就是 service 层从事件里取来再传下来的那个（见 service/refund.go），这里按同
	// 一条路给：少传它，outbox 行在链路上就断了线。
	row, replayed, err := repo.AdvanceRefund(ctx, AdvanceRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo,
		Succeeded: true, RefundedAt: refundedAt, RequestID: traceID, TraceID: traceID,
	})
	if err != nil {
		t.Fatalf("AdvanceRefund: %v", err)
	}
	if replayed {
		t.Fatal("第一次落结论不该是重放")
	}
	if row.AfterSale.Status != model.AfterSaleStatusRefunded || row.AfterSale.RefundedAt == nil {
		t.Fatalf("售后单 = %s / refundedAt=%v", row.AfterSale.Status, row.AfterSale.RefundedAt)
	}
	status, refunded := orderStatusOf(t, pool, fixture.orderID)
	if status != model.OrderStatusRefunded {
		t.Fatalf("订单状态 = %s, want refunded（整单退成功）", status)
	}
	if refunded != 3800 {
		t.Fatalf("已退金额 = %d, want 3800（不累加的话同一笔钱能再退一次）", refunded)
	}

	// 钱退成了 ⇒ 一条 order.after_sale.refunded 必须发出去。它是 account-service 追回福卡
	// 的唯一信号：发了它那些卡才被注销，不发就永远是「钱退了、赠品还在」。
	if got := outboxEventsForSale(t, pool, EventAfterSaleRefunded, approved.AfterSale.AfterSaleNo); got != 1 {
		t.Fatalf("%s 事件 %d 条，want 1", EventAfterSaleRefunded, got)
	}
	// trace 也必须落在那一行上，否则这条消息在链路上找不到上游。
	if got := outboxEvents(t, pool, traceID, EventAfterSaleRefunded); got != 1 {
		t.Fatalf("带 trace %s 的 %s 事件 %d 条，want 1", traceID, EventAfterSaleRefunded, got)
	}
	// 失败那条**不能**搭车发出去：两个结局做的是两件事（追回 vs 解冻），绑错主题的下游
	// 会照着一条「退款成功」去解冻。
	if got := outboxEventsForSale(t, pool, EventAfterSaleRefundFailed, approved.AfterSale.AfterSaleNo); got != 0 {
		t.Fatalf("成功的退款发了 %d 条 %s", got, EventAfterSaleRefundFailed)
	}

	var refundPayload dto.AfterSaleRefundEventPayload
	if err := json.Unmarshal(outboxPayload(t, pool, traceID, EventAfterSaleRefunded), &refundPayload); err != nil {
		t.Fatalf("退款事件载荷解不开: %v", err)
	}
	// 账户域按售后单号找冻结行，按订单号把冲正挂到订单详情那一屏上，两者缺一不可。
	if refundPayload.AfterSaleNo != approved.AfterSale.AfterSaleNo || refundPayload.OrderNo != row.AfterSale.OrderNo {
		t.Fatalf("退款事件的单号不对: %+v", refundPayload)
	}
	if refundPayload.RefundNo != refundNo || refundPayload.RefundAmount != 3800 {
		t.Fatalf("退款事件的退款单号/金额不对: %+v", refundPayload)
	}
	// 时刻取自渠道（这里是调用方给的 refundedAt），不是发消息那一刻——补投旧事件时账上的
	// 顺序必须还是那几天。
	if refundPayload.RefundedAtUnix != refundedAt.Unix() {
		t.Fatalf("refundedAtUnix = %d, want %d", refundPayload.RefundedAtUnix, refundedAt.Unix())
	}

	// 重投同一条事件：消息队列保证的是至少一次，重复是常态而不是异常。
	_, replayed, err = repo.AdvanceRefund(ctx, AdvanceRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo, Succeeded: true,
	})
	if err != nil || !replayed {
		t.Fatalf("重投: replayed=%v err=%v", replayed, err)
	}
	if _, refunded := orderStatusOf(t, pool, fixture.orderID); refunded != 3800 {
		t.Fatalf("重投把已退金额加成了 %d", refunded)
	}
	// 重投**不再发第二遍**：再发一次会让账户域把同一笔追回走第二趟——今天靠冻结行的状态
	// 挡住（no-op），但那是下游替发送方兜底，不该当成发送方的正确性。
	if got := outboxEventsForSale(t, pool, EventAfterSaleRefunded, approved.AfterSale.AfterSaleNo); got != 1 {
		t.Fatalf("重投后 %s 事件变成了 %d 条", EventAfterSaleRefunded, got)
	}

	// 换一个退款单号的事件：这张单上记的是另一张退款单，要人来看。
	if _, _, err := repo.AdvanceRefund(ctx, AdvanceRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: "RF-other", Succeeded: true,
	}); !errors.Is(err, ErrAfterSaleRefundMismatch) {
		t.Fatalf("换退款单号的事件 = %v, want ErrAfterSaleRefundMismatch", err)
	}
}

// TestPostgresAdvanceRefundFailureRestoresThePreviousStatus 是「退款失败后订单回到哪」：
// **回到退款前那个状态**，不是一律回 paid。这一单当初已经取过杯（completed），失败之后
// 它必须还是 completed——降回 paid 的意思是「还没做完」，与事实正好相反。
func TestPostgresAdvanceRefundFailureRestoresThePreviousStatus(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusCompleted, 0)
	ctx, traceID := ctxWithTraceID(t, uuid.NewString())

	approved := approvedAfterSale(t, repo, ctx, fixture, model.AfterSaleScopeAll, "")
	refundNo := "RF" + uuid.NewString()[:20]
	if _, _, err := repo.StartRefund(ctx, StartRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo,
	}); err != nil {
		t.Fatalf("StartRefund: %v", err)
	}

	row, replayed, err := repo.AdvanceRefund(ctx, AdvanceRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo,
		Succeeded: false, FailureCode: "ACQ.TRADE_NOT_EXIST", FailureMessage: "原交易不存在",
		RequestID: traceID, TraceID: traceID,
	})
	if err != nil || replayed {
		t.Fatalf("AdvanceRefund: replayed=%v err=%v", replayed, err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusFailed {
		t.Fatalf("售后单状态 = %s, want failed", row.AfterSale.Status)
	}
	if row.AfterSale.FailureCode != "ACQ.TRADE_NOT_EXIST" {
		t.Fatalf("failureCode = %q, 后台与客服靠它解释「为什么没退成」", row.AfterSale.FailureCode)
	}
	if row.AfterSale.FailureMessage != "原交易不存在" {
		t.Fatalf("failureMessage = %q, want 渠道原文——码给机器认，这句话才是给人读的",
			row.AfterSale.FailureMessage)
	}
	status, refunded := orderStatusOf(t, pool, fixture.orderID)
	if status != model.OrderStatusCompleted {
		t.Fatalf("订单状态 = %s, want completed（回到退款前那个状态）", status)
	}
	if refunded != 0 {
		t.Fatalf("没退成却记了已退 %d", refunded)
	}

	// 钱没退成**也要发事件**，而且必须是失败那一条。它是账户域解冻福卡的唯一信号：不发，
	// 那些卡就永远冻着；发成成功那条，账户域会把卡真的扣掉——钱没退，赠品先没了。
	if got := outboxEventsForSale(t, pool, EventAfterSaleRefundFailed, approved.AfterSale.AfterSaleNo); got != 1 {
		t.Fatalf("%s 事件 %d 条，want 1", EventAfterSaleRefundFailed, got)
	}
	if got := outboxEvents(t, pool, traceID, EventAfterSaleRefundFailed); got != 1 {
		t.Fatalf("带 trace %s 的 %s 事件 %d 条，want 1", traceID, EventAfterSaleRefundFailed, got)
	}
	if got := outboxEventsForSale(t, pool, EventAfterSaleRefunded, approved.AfterSale.AfterSaleNo); got != 0 {
		t.Fatalf("失败的退款发了 %d 条 %s", got, EventAfterSaleRefunded)
	}
	var failurePayload dto.AfterSaleRefundEventPayload
	if err := json.Unmarshal(outboxPayload(t, pool, traceID, EventAfterSaleRefundFailed), &failurePayload); err != nil {
		t.Fatalf("退款失败事件载荷解不开: %v", err)
	}
	// 渠道为什么拒要跟着事件走：后台与客服就是靠这两格解释「这笔钱为什么没退成」。
	if failurePayload.FailureCode != "ACQ.TRADE_NOT_EXIST" || failurePayload.FailureMessage != "原交易不存在" {
		t.Fatalf("失败原因没进事件: %+v", failurePayload)
	}
	if failurePayload.AfterSaleNo != approved.AfterSale.AfterSaleNo {
		t.Fatalf("退款失败事件的售后单号 = %q", failurePayload.AfterSaleNo)
	}
	// 没退成，就没有退款时刻——调用方给的是零值，unixOrZero 把它压成 0。这里也不能编一个
	// 出来：一个凭空的时间会让账上的顺序变成「失败发生在刚才」。
	if failurePayload.RefundedAtUnix != 0 {
		t.Fatalf("失败的退款带了时刻 %d", failurePayload.RefundedAtUnix)
	}

	// 失败之后那张单确实**回到了可退的状态**：钱没出去，用户本来就该能再申请一次。
	if _, _, err := repo.ApplyAfterSale(ctx, applyParams(t, fixture, model.AfterSaleScopeAll, "")); err != nil {
		t.Fatalf("退款失败后重新申请: %v", err)
	}
}

// TestPostgresAdvanceRefundLineScopeKeepsTheOrderAlive：按行退成功**不把整单判死**。
// 只退了一杯的订单还活着，剩下那行还要履约——标成 refunded 会让它做不下去。
func TestPostgresAdvanceRefundLineScopeKeepsTheOrderAlive(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx, traceID := ctxWithTraceID(t, uuid.NewString())

	approved := approvedAfterSale(t, repo, ctx, fixture, model.AfterSaleScopeDrink, fixture.lineID)
	refundNo := "RF" + uuid.NewString()[:20]
	if _, _, err := repo.StartRefund(ctx, StartRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo,
	}); err != nil {
		t.Fatalf("StartRefund: %v", err)
	}

	if _, _, err := repo.AdvanceRefund(ctx, AdvanceRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo,
		Succeeded: true, RefundedAt: time.Now().UTC(), RequestID: traceID,
	}); err != nil {
		t.Fatalf("AdvanceRefund: %v", err)
	}
	status, refunded := orderStatusOf(t, pool, fixture.orderID)
	if status != model.OrderStatusPaid {
		t.Fatalf("订单状态 = %s, want paid（只退了一杯，整单还活着）", status)
	}
	if refunded != 1900 {
		t.Fatalf("已退金额 = %d, want 1900（这一行的应付额）", refunded)
	}
}

// TestPostgresAdvanceRefundAdoptsAnEventThatBeatTransactionB：审核通过 → 建退款单 → 写
// refunding 这三步之间挂过一次，而钱已经退成了。
//
// 这条事件**不能扔**：钱是真的出去了，扔掉它订单域就永远不知道。补写那一步，让流水上
// 每一段迁移都是合法的（approved → refunding → refunded），结论照常落。
func TestPostgresAdvanceRefundAdoptsAnEventThatBeatTransactionB(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	repo := NewPostgresRepository(pool, audit.NewRecorder())
	fixture := afterSaleOrderFixture(t, pool, model.OrderStatusPaid, 0)
	ctx, traceID := ctxWithTraceID(t, uuid.NewString())

	approved := approvedAfterSale(t, repo, ctx, fixture, model.AfterSaleScopeAll, "")
	refundNo := "RF" + uuid.NewString()[:20]
	// 注意：**没有调 StartRefund**，这就是那个窗口。

	row, replayed, err := repo.AdvanceRefund(ctx, AdvanceRefundParams{
		AfterSaleNo: approved.AfterSale.AfterSaleNo, RefundNo: refundNo,
		Succeeded: true, RefundedAt: time.Now().UTC(), RequestID: traceID,
	})
	if err != nil || replayed {
		t.Fatalf("AdvanceRefund: replayed=%v err=%v", replayed, err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusRefunded || row.AfterSale.RefundNo != refundNo {
		t.Fatalf("售后单 = %s / %s", row.AfterSale.Status, row.AfterSale.RefundNo)
	}
	if status, refunded := orderStatusOf(t, pool, fixture.orderID); status != model.OrderStatusRefunded || refunded != 3800 {
		t.Fatalf("订单 = %s / 已退 %d", status, refunded)
	}
	// 补写那一步要留下痕迹：两段迁移都要在流水里看得到，否则「这张单怎么从 approved
	// 直接跳到 refunded 的」事后查不出来。
	var toRefunding int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM order_state_transitions
		WHERE aggregate_type='after_sale' AND aggregate_id=$1 AND to_status='refunding'`,
		approved.AfterSale.ID).Scan(&toRefunding); err != nil {
		t.Fatalf("读状态流水: %v", err)
	}
	if toRefunding != 1 {
		t.Fatalf("approved→refunding 的流水有 %d 条，want 1", toRefunding)
	}
}
