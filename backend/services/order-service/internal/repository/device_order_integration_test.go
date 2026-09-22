package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// 这一组用例打真库，盯的是设备单那条 INSERT 上**只有真库才验得了**的三件事：
//
//  1. user_id 写进去的是 NULL，而那几条金额恒等式与 NOT NULL 列全都过得去（order/005）；
//  2. 幂等是**靠那条部分唯一索引**兜住的，不是靠先查后插——所以并发重投只落一张单；
//  3. 这条路不发任何事件。
//
// 三者都是「代码看起来对、库里其实不对」的类型，所以必须真跑一次。
//
// 夹具用唯一的对方单号，跑完按依赖顺序删掉（与 settle_payment 那边同一套做法；
// order_state_transitions 不删，那张表有 append-only 触发器）。

const deviceOrderTestPayable = 1500

func deviceOrderTestParams(thirdParty string) CreateDeviceOrderParams {
	deviceID, storeID := uuid.NewString(), uuid.NewString()
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	return CreateDeviceOrderParams{
		ThirdPartyOrderNo: thirdParty,
		OrderNo:           "INTDEV" + uuid.NewString(),
		Source:            model.SourceDevice,
		FulfillmentStatus: model.FulfillmentPending,
		StoreID:           &storeID,
		DeviceID:          &deviceID,
		DeviceNo:          "SN-" + uuid.NewString(),
		OriginalAmount:    1800,
		DiscountAmount:    300,
		PayableAmount:     deviceOrderTestPayable,
		PaymentMethod:     "card_pay",
		PaidAt:            now,
		Remark:            "设备刷卡购买",
		Line: &OrderLineInsert{
			LineNo: 1, LineType: model.LineTypeDrink,
			ItemID:                 &deviceID,
			ItemCode:               "P001",
			ItemName:               "集成测试拿铁",
			Quantity:               1,
			OriginalUnitPrice:      1800,
			UnitPrice:              deviceOrderTestPayable,
			PriceDiscountAmount:    300,
			DiscountAmount:         300,
			PayableAmount:          deviceOrderTestPayable,
			Specs:                  []byte("{}"),
			SelectionSnapshot:      []byte("{}"),
			CampaignSnapshot:       []byte("{}"),
			MembershipPlanSnapshot: []byte("{}"),
			DeviceID:               &deviceID,
		},
	}
}

// cleanupDeviceOrder 按对方单号删掉这一单的行与主表。
func cleanupDeviceOrder(t *testing.T, pool *pgxpool.Pool, thirdParty string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM order_lines WHERE order_id IN
			(SELECT id FROM orders WHERE third_party_order_no=$1)`, thirdParty)
		_, _ = pool.Exec(ctx, `DELETE FROM orders WHERE third_party_order_no=$1`, thirdParty)
	})
}

// TestPostgresCreateDeviceOrderRecordsAPaidOrderWithoutAUser 是设备单落库的正例。
func TestPostgresCreateDeviceOrderRecordsAPaidOrderWithoutAUser(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-DEV-" + uuid.NewString()
	params := deviceOrderTestParams(thirdParty)
	cleanupDeviceOrder(t, pool, thirdParty)

	result, created, err := repo.CreateDeviceOrder(ctx, params)
	if err != nil {
		t.Fatalf("CreateDeviceOrder: %v", err)
	}
	if !created || !result.Created {
		t.Fatalf("created = %v/%v, want true on the first insert", created, result.Created)
	}
	if result.OrderNo != params.OrderNo {
		t.Errorf("orderNo = %q, want %q", result.OrderNo, params.OrderNo)
	}

	// 主表：没有用户（NULL，不是空 uuid）、已支付、没有待支付期、没有支付单号。
	var userID *string
	var source, status, fulfillmentStatus, paymentMethod, paymentNo, thirdPartyNo string
	var paidAt, expiresAt *time.Time
	err = pool.QueryRow(ctx, `SELECT user_id::text, source, status, fulfillment_status,
		payment_method, payment_no, third_party_order_no, paid_at, expires_at
		FROM orders WHERE id=$1`, result.OrderID).Scan(&userID, &source, &status,
		&fulfillmentStatus, &paymentMethod, &paymentNo, &thirdPartyNo, &paidAt, &expiresAt)
	if err != nil {
		t.Fatalf("read the order back: %v", err)
	}
	if userID != nil {
		t.Errorf("user_id = %q, want NULL: a device order has no user (order/005)", *userID)
	}
	if source != model.SourceDevice || status != model.OrderStatusPaid {
		t.Errorf("(source, status) = (%q, %q), want (%q, %q)", source, status, model.SourceDevice, model.OrderStatusPaid)
	}
	if fulfillmentStatus != model.FulfillmentPending {
		t.Errorf("fulfillmentStatus = %q, want %q", fulfillmentStatus, model.FulfillmentPending)
	}
	if paymentMethod != "card_pay" || paymentNo != "" {
		t.Errorf("(paymentMethod, paymentNo) = (%q, %q), want (card_pay, \"\")", paymentMethod, paymentNo)
	}
	if thirdPartyNo != thirdParty {
		t.Errorf("thirdPartyOrderNo = %q, want %q", thirdPartyNo, thirdParty)
	}
	if paidAt == nil {
		t.Error("paid_at is NULL: this order was paid on the machine, not here")
	}
	if expiresAt != nil {
		t.Error("expires_at is set: a device order is never awaiting payment")
	}

	// 行：金额恒等式真过了库上的 CHECK（过不去这条 INSERT 会直接 23514），
	// 而且优惠整块记在标价优惠那一格（设备单没有券）。
	var lineType, itemCode string
	var originalUnit, unitPrice, priceDiscount, couponDiscount, discount, payable int64
	err = pool.QueryRow(ctx, `SELECT line_type, item_code, original_unit_price, unit_price,
		price_discount_amount, coupon_discount_amount, discount_amount, payable_amount
		FROM order_lines WHERE order_id=$1 AND line_no=1`, result.OrderID).
		Scan(&lineType, &itemCode, &originalUnit, &unitPrice, &priceDiscount, &couponDiscount, &discount, &payable)
	if err != nil {
		t.Fatalf("read the order line back: %v", err)
	}
	if lineType != model.LineTypeDrink || itemCode != "P001" {
		t.Errorf("(lineType, itemCode) = (%q, %q), want (drink, P001)", lineType, itemCode)
	}
	// quantity 恒为 1，所以行上的恒等式就是 payable = original - discount。
	if payable != originalUnit-discount {
		t.Errorf("payable %d != original %d - discount %d", payable, originalUnit, discount)
	}
	if discount != priceDiscount+couponDiscount {
		t.Errorf("discount %d != price %d + coupon %d", discount, priceDiscount, couponDiscount)
	}

	// 状态流水照写（actor 是 system：推动它的是机器，不是人）。
	var toStatus, actorType string
	var transitions int
	if err := pool.QueryRow(ctx, `SELECT count(*), min(to_status), min(actor_type)
		FROM order_state_transitions WHERE aggregate_id=$1`, result.OrderID).
		Scan(&transitions, &toStatus, &actorType); err != nil {
		t.Fatalf("read the transitions back: %v", err)
	}
	if transitions != 1 || toStatus != model.OrderStatusPaid || actorType != "system" {
		t.Errorf("transitions = (%d, %q, %q), want (1, paid, system)", transitions, toStatus, actorType)
	}

	// **不发事件**：出杯已经在机器上发生过，order.paid 是「该去做这一杯」的触发点，
	// 对着一条已经出过杯的订单发它等于让机器再出一杯（见 CreateDeviceOrder 的说明）。
	var emitted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_outbox
		WHERE convert_from(payload,'UTF8') LIKE '%'||$1||'%'`, result.OrderID).Scan(&emitted); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if emitted != 0 {
		t.Errorf("this order emitted %d event(s); the device order path emits none", emitted)
	}
}

// TestPostgresCreateDeviceOrderReplaysOnTheSameThirdPartyOrderNo 盯幂等：同一个对方单号
// 再投一次，返回**既有那张单**、created=false，而且一个字都没被改（金额也没被重写）。
func TestPostgresCreateDeviceOrderReplaysOnTheSameThirdPartyOrderNo(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-DEV-" + uuid.NewString()
	first := deviceOrderTestParams(thirdParty)
	cleanupDeviceOrder(t, pool, thirdParty)

	created, _, err := repo.CreateDeviceOrder(ctx, first)
	if err != nil {
		t.Fatalf("first CreateDeviceOrder: %v", err)
	}

	// 重投：单号、金额都换了（机器重试时不该指望它给一模一样的报文），幂等键没换。
	replay := deviceOrderTestParams(thirdParty)
	replay.PayableAmount, replay.DiscountAmount, replay.OriginalAmount = 9999, 0, 9999
	result, replayedCreated, err := repo.CreateDeviceOrder(ctx, replay)
	if err != nil {
		t.Fatalf("replayed CreateDeviceOrder: %v", err)
	}
	if replayedCreated || result.Created {
		t.Error("a replay reported itself as a new order")
	}
	if result.OrderID != created.OrderID {
		t.Errorf("replay returned order %s, want the existing %s", result.OrderID, created.OrderID)
	}
	if result.OrderNo != first.OrderNo {
		t.Errorf("orderNo = %q, want the existing %q", result.OrderNo, first.OrderNo)
	}

	var orders, lines int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE third_party_order_no=$1`, thirdParty).Scan(&orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_lines WHERE order_id=$1`, created.OrderID).Scan(&lines); err != nil {
		t.Fatalf("count order lines: %v", err)
	}
	if orders != 1 || lines != 1 {
		t.Fatalf("after a replay there are %d orders and %d lines, want 1 and 1", orders, lines)
	}

	var payable int64
	if err := pool.QueryRow(ctx, `SELECT payable_amount FROM orders WHERE id=$1`, created.OrderID).Scan(&payable); err != nil {
		t.Fatalf("read payable: %v", err)
	}
	if payable != deviceOrderTestPayable {
		t.Errorf("payableAmount = %d, want the first request's %d: a replay must not rewrite the order",
			payable, deviceOrderTestPayable)
	}
}

// TestPostgresFindDeviceOrderByThirdPartyNo 盯的是扣款之前那一次预检读的三格：
// 「没有」是 (nil, nil) 而不是错误，查到时要带回单号与 payment_method（判据就是它）。
func TestPostgresFindDeviceOrderByThirdPartyNo(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-DEV-" + uuid.NewString()
	cleanupDeviceOrder(t, pool, thirdParty)

	// 没有这一单时回 (nil, nil)：绝大多数请求都是第一次来，把「没有」做成错误会让每个
	// 调用点都要先翻译一遍。
	missing, err := repo.FindDeviceOrderByThirdPartyNo(ctx, thirdParty)
	if err != nil {
		t.Fatalf("FindDeviceOrderByThirdPartyNo on a missing order: %v", err)
	}
	if missing != nil {
		t.Fatalf("found %+v, want nil for an unused third party order no", missing)
	}

	created, _, err := repo.CreateDeviceOrder(ctx, deviceOrderTestParams(thirdParty))
	if err != nil {
		t.Fatalf("CreateDeviceOrder: %v", err)
	}
	found, err := repo.FindDeviceOrderByThirdPartyNo(ctx, thirdParty)
	if err != nil {
		t.Fatalf("FindDeviceOrderByThirdPartyNo: %v", err)
	}
	if found == nil {
		t.Fatal("found nil after the order was created")
	}
	if found.OrderID != created.OrderID {
		t.Errorf("orderID = %q, want %q", found.OrderID, created.OrderID)
	}
	if found.PaymentMethod != "card_pay" {
		t.Errorf("paymentMethod = %q, want card_pay（判据就是这一格）", found.PaymentMethod)
	}
}

// TestPostgresCreateDeviceOrderRefusesAnotherKindsOrderNo 是两条设备路共用一个单号空间的
// 那一半：单号先被一张**刷卡机**单占了，再用它建一张**取货码**单时必须报错，而不是把那张
// 单当成「你这一次的幂等命中」返回。
//
// 当成幂等命中的后果在取货码那条路上特别贵：那一单的钱**已经扣过了**（那条路是先扣后建），
// 于是钱扣了、返回给对方的却是另一张单——一笔扣款从此挂在一张与它无关的订单上。
func TestPostgresCreateDeviceOrderRefusesAnotherKindsOrderNo(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-DEV-" + uuid.NewString()
	cleanupDeviceOrder(t, pool, thirdParty)

	if _, _, err := repo.CreateDeviceOrder(ctx, deviceOrderTestParams(thirdParty)); err != nil {
		t.Fatalf("CreateDeviceOrder (card_pay): %v", err)
	}

	// 同一个单号，另一类设备单。
	intruder := deviceOrderTestParams(thirdParty)
	intruder.PaymentMethod = "pickup_code"
	intruder.Remark = "取货码购买"
	_, created, err := repo.CreateDeviceOrder(ctx, intruder)
	if !errors.Is(err, ErrThirdPartyOrderNoTaken) {
		t.Fatalf("err = %v, want ErrThirdPartyOrderNoTaken", err)
	}
	if created {
		t.Error("created = true, want false for a rejected order no")
	}

	// 一个字都没写：还是一张单、一行行，而且那一张仍然是刷卡机那一张。
	var orders, lines int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE third_party_order_no=$1`, thirdParty).Scan(&orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 1 {
		t.Fatalf("stored %d orders for one third party order number, want 1", orders)
	}
	var orderID, paymentMethod string
	if err := pool.QueryRow(ctx, `SELECT id::text, payment_method FROM orders
		WHERE third_party_order_no=$1`, thirdParty).Scan(&orderID, &paymentMethod); err != nil {
		t.Fatalf("read the order back: %v", err)
	}
	if paymentMethod != "card_pay" {
		t.Errorf("payment_method = %q, want the first order's card_pay untouched", paymentMethod)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM order_lines WHERE order_id=$1`, orderID).Scan(&lines); err != nil {
		t.Fatalf("count order lines: %v", err)
	}
	if lines != 1 {
		t.Fatalf("that order has %d lines, want the 1 it was created with", lines)
	}
}

// TestPostgresCreateDeviceOrderIsIdempotentUnderConcurrency 是「幂等靠唯一索引、不靠先查后插」
// 的唯一证据：两个事务同时插同一个对方单号，只有一个能建单。
//
// 先查后插在这里必然失败——两个请求都会查到「没有」、各自插一张；而这一条用例正是要证明
// 代码没走那条路。落库的两张单意味着钱只收了一次、账上却卖出两杯。
func TestPostgresCreateDeviceOrderIsIdempotentUnderConcurrency(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-DEV-RACE-" + uuid.NewString()
	cleanupDeviceOrder(t, pool, thirdParty)

	type outcome struct {
		result  *CreateDeviceOrderResult
		created bool
		err     error
	}
	const racers = 4
	outcomes := make([]outcome, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让几个请求同时到达
			result, created, err := repo.CreateDeviceOrder(ctx, deviceOrderTestParams(thirdParty))
			outcomes[i] = outcome{result: result, created: created, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	createdCount := 0
	orderID := ""
	for i, o := range outcomes {
		if o.err != nil {
			t.Fatalf("racer %d: %v", i, o.err)
		}
		if o.created {
			createdCount++
			orderID = o.result.OrderID
		}
	}
	if createdCount != 1 {
		t.Fatalf("%d of %d racers created an order, want exactly 1", createdCount, racers)
	}
	// 重投的那几个必须拿到**同一张**单的 id（而不是空 id 或者自己的 id）。
	for i, o := range outcomes {
		if o.result.OrderID != orderID {
			t.Errorf("racer %d got order %s, want the single order %s", i, o.result.OrderID, orderID)
		}
	}

	var orders int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE third_party_order_no=$1`, thirdParty).Scan(&orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 1 {
		t.Fatalf("stored %d orders for one third party order number, want 1", orders)
	}
}
