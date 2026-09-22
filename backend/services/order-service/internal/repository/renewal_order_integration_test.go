package repository

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// 这一组用例打真库，盯的是续费单那条 INSERT 上**只有真库才验得了**的四件事：
//
//  1. `source='renewal'` 真的过得了 orders_source_check——也就是说 order/009 **真的 apply 过**。
//     这是整条路上唯一一处「代码对了、库没改就是不行」的地方，而它在集成用例里是免费验的；
//  2. user_id 非空、点位与设备为空、履约汇总 none、expires_at 为 NULL 这一组形状；
//  3. 幂等仍然靠**那条部分唯一索引**兜（不是先查后插），且命中时返回的是既有那张单；
//  4. 与设备那两条**共用同一个单号空间**：撞上一张刷卡机单时要报 ErrThirdPartyOrderNoTaken，
//     而不是把它当成「你这一次的幂等命中」。
//
// 夹具用唯一的对方单号，跑完按依赖顺序删掉（与设备单那组同一套做法）。

const renewalOrderTestAmount = 990

// renewalOrderTestSnapshot 是一份形状正确的套餐快照。
//
// 这里刻意用**字面量**而不是 dto.MembershipPlanSnapshot：仓储这一层不该为了造一份夹具去依赖
// 业务层的 DTO，而它要验的也不是那份结构的形状（那是 service 用例的事），是「这一列存得进、
// 取得出」。
const renewalOrderTestSnapshot = `{"planId":"1a2b3c4d-1111-4222-8333-444455556666",` +
	`"planCode":"MONTHLY_990","planName":"连续包月","priceCents":990,"period":"month",` +
	`"periodCount":1,"autoRenew":true,"memberPriceMode":"coupon",` +
	`"memberPriceCouponTemplateId":"2b3c4d5e-1111-4222-8333-444455556666",` +
	`"memberPriceCouponsPerPeriod":4}`

func renewalOrderTestParams(thirdParty string) CreateRenewalOrderParams {
	membershipID := uuid.NewString()
	return CreateRenewalOrderParams{
		ThirdPartyOrderNo: thirdParty,
		OrderNo:           "INTREN" + uuid.NewString(),
		UserID:            uuid.NewString(),
		MembershipID:      &membershipID,
		Amount:            renewalOrderTestAmount,
		PaymentMethod:     "wechat_papay",
		PaidAt:            time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		Remark:            "会员续费扣款",
		TransitionReason:  "会员续费扣款",
		Line: &OrderLineInsert{
			LineNo:   1,
			LineType: model.LineTypeMembership,
			ItemID:   stringPtr("1a2b3c4d-1111-4222-8333-444455556666"),
			ItemCode: "MONTHLY_990",
			ItemName: "连续包月",
			Quantity: 1,
			// 三个价格同值、优惠为零：会员套餐没有标价与成交价之分。
			OriginalUnitPrice: renewalOrderTestAmount,
			UnitPrice:         renewalOrderTestAmount,
			PayableAmount:     renewalOrderTestAmount,
			Specs:             []byte("{}"),
			SelectionSnapshot: []byte("{}"),
			CampaignSnapshot:  []byte("{}"),
			// 这一列是这一单唯一有内容的一份快照。
			MembershipPlanSnapshot: []byte(renewalOrderTestSnapshot),
		},
	}
}

func stringPtr(value string) *string { return &value }

// jsonEqual 比两份 JSON **内容**是否相同（jsonb 会重排键的顺序，也比字节更严格地对待空白，
// 所以不能直接比字符串）。解不出来就是不等——夹具本身写坏了不该静默通过。
func jsonEqual(t *testing.T, left, right string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(left), &a); err != nil {
		t.Fatalf("the stored value is not JSON: %v (%s)", err, left)
	}
	if err := json.Unmarshal([]byte(right), &b); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	return reflect.DeepEqual(a, b)
}

// cleanupRenewalOrder 按对方单号删掉这一单的行与主表。
func cleanupRenewalOrder(t *testing.T, pool *pgxpool.Pool, thirdParty string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM order_lines WHERE order_id IN
			(SELECT id FROM orders WHERE third_party_order_no=$1)`, thirdParty)
		_, _ = pool.Exec(ctx, `DELETE FROM orders WHERE third_party_order_no=$1`, thirdParty)
	})
}

// TestPostgresCreateRenewalOrderRecordsAPaidMembershipOrder 是续费单落库的正例。
//
// 它同时是 order/009 的验证：`source='renewal'` 过不了 CHECK 的话，第一句 INSERT 就会以
// 23514 失败，而这条用例会带着「迁移没 apply」的提示红掉。
func TestPostgresCreateRenewalOrderRecordsAPaidMembershipOrder(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-REN-" + uuid.NewString()
	params := renewalOrderTestParams(thirdParty)
	cleanupRenewalOrder(t, pool, thirdParty)

	result, created, err := repo.CreateRenewalOrder(ctx, params)
	if err != nil {
		t.Fatalf("CreateRenewalOrder: %v（source='renewal' 被拒说明 order/009 没 apply 到这个库）", err)
	}
	if !created || !result.Created {
		t.Fatalf("created = %v/%v, want true on the first insert", created, result.Created)
	}
	if result.OrderNo != params.OrderNo {
		t.Errorf("orderNo = %q, want %q", result.OrderNo, params.OrderNo)
	}

	// 主表：**有**用户、已支付、没有点位与设备、没有待支付期、没有支付单号。
	var userID, storeID, deviceID, membershipID *string
	var source, status, fulfillmentStatus, paymentMethod, paymentNo, thirdPartyNo string
	var paidAt, expiresAt *time.Time
	var paidAmount int64
	err = pool.QueryRow(ctx, `SELECT user_id::text, store_id::text, device_id::text, membership_id::text,
		source, status, fulfillment_status, payment_method, payment_no, third_party_order_no,
		paid_at, expires_at, paid_amount FROM orders WHERE id=$1`, result.OrderID).
		Scan(&userID, &storeID, &deviceID, &membershipID, &source, &status, &fulfillmentStatus,
			&paymentMethod, &paymentNo, &thirdPartyNo, &paidAt, &expiresAt, &paidAmount)
	if err != nil {
		t.Fatalf("read the order back: %v", err)
	}
	// 与设备单相反的那一格：这一单**有主人**。
	if userID == nil || *userID != params.UserID {
		t.Errorf("user_id = %v, want %q: a renewal order belongs to the user who was charged", userID, params.UserID)
	}
	if membershipID == nil || *membershipID != *params.MembershipID {
		t.Errorf("membership_id = %v, want %q", membershipID, *params.MembershipID)
	}
	if source != model.SourceRenewal || status != model.OrderStatusPaid {
		t.Errorf("(source, status) = (%q, %q), want (%q, %q)", source, status, model.SourceRenewal, model.OrderStatusPaid)
	}
	// 纯会员订单：没有任何需要履约的行。
	if fulfillmentStatus != model.FulfillmentNone {
		t.Errorf("fulfillmentStatus = %q, want %q", fulfillmentStatus, model.FulfillmentNone)
	}
	if paymentMethod != "wechat_papay" {
		t.Errorf("paymentMethod = %q, want wechat_papay", paymentMethod)
	}
	// 没有支付单：一条 payments 行的语义是「用户发起了一次支付」，而代扣没有那一次发起。
	if paymentNo != "" {
		t.Errorf("paymentNo = %q, want empty: a renewal has no payment record", paymentNo)
	}
	if thirdPartyNo != thirdParty {
		t.Errorf("thirdPartyOrderNo = %q, want %q", thirdPartyNo, thirdParty)
	}
	if paidAt == nil {
		t.Error("paid_at is NULL: the money was taken before this order was written")
	}
	if expiresAt != nil {
		t.Error("expires_at is set: a renewal order is never awaiting payment")
	}
	// 点位与设备都为空：纯会员订单没有成交点位（老系统那张单上的门店是**会员归属**门店，
	// 在本服务里的对应物是 memberships.store_id，不复制进订单）。
	if storeID != nil || deviceID != nil {
		t.Errorf("(store_id, device_id) = (%v, %v), want both NULL", storeID, deviceID)
	}
	// 金额：original = payable = paid = amount，优惠恒为 0。
	if paidAmount != renewalOrderTestAmount {
		t.Errorf("paidAmount = %d, want %d", paidAmount, renewalOrderTestAmount)
	}

	// 行：会员行，带套餐快照；金额恒等式真过了库上的 CHECK（过不去这条 INSERT 会直接 23514）。
	var lineType, itemName, snapshot string
	var originalUnit, unitPrice, discount, payable int64
	err = pool.QueryRow(ctx, `SELECT line_type, item_name, original_unit_price, unit_price,
		discount_amount, payable_amount, membership_plan_snapshot::text
		FROM order_lines WHERE order_id=$1 AND line_no=1`, result.OrderID).
		Scan(&lineType, &itemName, &originalUnit, &unitPrice, &discount, &payable, &snapshot)
	if err != nil {
		t.Fatalf("read the order line back: %v", err)
	}
	if lineType != model.LineTypeMembership || itemName != "连续包月" {
		t.Errorf("(lineType, itemName) = (%q, %q), want (membership, 连续包月)", lineType, itemName)
	}
	if payable != originalUnit*1-discount {
		t.Errorf("payable %d != original %d - discount %d", payable, originalUnit, discount)
	}
	// 快照原样落进那一列：jsonb 会重排键的顺序，所以这里比的是「解出来是什么」而不是字节。
	if !jsonEqual(t, snapshot, renewalOrderTestSnapshot) {
		t.Errorf("membership_plan_snapshot = %s, want %s", snapshot, renewalOrderTestSnapshot)
	}

	// 状态流水照写（actor 是 system：推动它的是到期扫描与渠道的扣款通知，不是人）。
	var toStatus, actorType, reason string
	var transitions int
	if err := pool.QueryRow(ctx, `SELECT count(*), min(to_status), min(actor_type), min(reason)
		FROM order_state_transitions WHERE aggregate_id=$1`, result.OrderID).
		Scan(&transitions, &toStatus, &actorType, &reason); err != nil {
		t.Fatalf("read the transitions back: %v", err)
	}
	if transitions != 1 || toStatus != model.OrderStatusPaid || actorType != "system" {
		t.Errorf("transitions = (%d, %q, %q), want (1, paid, system)", transitions, toStatus, actorType)
	}
	if reason != "会员续费扣款" {
		t.Errorf("reason = %q, want 会员续费扣款", reason)
	}

	// **不发事件**：这一单的后果（会员延期、发券）在调用方那边已经做完了，再发一条 order.paid
	// 会让下游按订单快照再开一次会员——同一期扣款被兑现两次（见 CreateRenewalOrder 的说明）。
	var emitted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_outbox
		WHERE convert_from(payload,'UTF8') LIKE '%'||$1||'%'`, result.OrderID).Scan(&emitted); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if emitted != 0 {
		t.Errorf("this order emitted %d event(s); the renewal path emits none", emitted)
	}
}

// TestPostgresCreateRenewalOrderReplaysOnTheSameChannelTransactionID 盯幂等：同一个渠道流水号
// 再投一次，返回**既有那张单**、created=false，而且一个字都没被改。
//
// 这条路上的重投是常态而不是异常——扣款结果通知是 MQ 消息，投递语义是至少一次。
func TestPostgresCreateRenewalOrderReplaysOnTheSameChannelTransactionID(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-REN-" + uuid.NewString()
	first := renewalOrderTestParams(thirdParty)
	cleanupRenewalOrder(t, pool, thirdParty)

	created, _, err := repo.CreateRenewalOrder(ctx, first)
	if err != nil {
		t.Fatalf("first CreateRenewalOrder: %v", err)
	}

	// 重投：金额与订单号都换了（不该指望重投时给一模一样的报文），幂等键没换。
	replay := renewalOrderTestParams(thirdParty)
	replay.Amount = 9999
	replay.Line.OriginalUnitPrice, replay.Line.UnitPrice, replay.Line.PayableAmount = 9999, 9999, 9999
	result, replayedCreated, err := repo.CreateRenewalOrder(ctx, replay)
	if err != nil {
		t.Fatalf("replayed CreateRenewalOrder: %v", err)
	}
	if replayedCreated || result.Created {
		t.Error("a replay reported itself as a new order")
	}
	if result.OrderID != created.OrderID {
		t.Errorf("replay returned order %s, want the existing %s", result.OrderID, created.OrderID)
	}
	// 回的是**既有那一张**的单号：调用方要拿它写进自己的续费流水，两边必须是同一个。
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
	if payable != renewalOrderTestAmount {
		t.Errorf("payableAmount = %d, want the first request's %d: a replay must not rewrite the order",
			payable, renewalOrderTestAmount)
	}
}

// TestPostgresCreateRenewalOrderRefusesAnotherKindsOrderNo 是「三条路共用一个单号空间」的
// 另一半：这个流水号上已经挂着一张**刷卡机**单时，续费建单必须报 ErrThirdPartyOrderNoTaken。
//
// 为什么这一条在续费路上比在设备那两条路上更要紧：设备那两条的键是合作方生成的商户单号，
// 而这一条用的是**微信的流水号**——两个空间的取值风格完全不同，撞车意味着有人把别的单号
// 填进了这条 RPC（或者反过来），那是一次要立刻查的串线，不是一次重投。
func TestPostgresCreateRenewalOrderRefusesAnotherKindsOrderNo(t *testing.T) {
	pool := afterSaleIntegrationPool(t)
	ctx := context.Background()
	repo := NewPostgresRepository(pool, audit.Noop{})

	thirdParty := "INT-REN-" + uuid.NewString()
	cleanupRenewalOrder(t, pool, thirdParty)
	cleanupDeviceOrder(t, pool, thirdParty)

	// 先用这个单号落一张刷卡机单。
	if _, _, err := repo.CreateDeviceOrder(ctx, deviceOrderTestParams(thirdParty)); err != nil {
		t.Fatalf("CreateDeviceOrder: %v", err)
	}

	// 再拿它当渠道流水号建续费单。
	result, created, err := repo.CreateRenewalOrder(ctx, renewalOrderTestParams(thirdParty))
	if !errors.Is(err, ErrThirdPartyOrderNoTaken) {
		t.Fatalf("err = %v, want ErrThirdPartyOrderNoTaken", err)
	}
	if created || result != nil {
		t.Errorf("result = %+v, created = %v; want nothing back for a rejected order no", result, created)
	}
}
