package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// 代扣续费里**订单那一半**：一期扣款成功之后先在订单域记一张单，再把那个订单号带进流水与
// membership.renewed 事件。
//
// # 它为什么要单独一份（而不是并进 charge_integration_test.go）
//
// 因为这一份钉的不是「会员续上了没有」（那一份钉了），而是**另外两件在库里看不出来的事**：
//
//	订单域被叫了几次、拿到的报文对不对    —— 一个假客户端才答得出来
//	事件里的 orderId 是不是那个订单号     —— 它是 coupon-service 发券的判据，空着就静默不发券
//
// 后者正是这一刀修的缺陷：这一条链上没有一处会报错，包月用户每期扣了钱、券一张都没发，两个服务
// 各自的日志里都只有正常记录。所以这里断言的不是「事件落了没有」（那一份已经断言过），而是**事件
// 体里那个字段的值**。
//
// # 假订单域不能比真订单域笨
//
// fakeOrders **照订单域那条真实规则的形状**实现幂等：同一个渠道流水号只会建出一张单，重发拿回的
// 是同一张（created=false，见 order-service 的 orders_third_party_order_no_key）。一个每次都新建
// 的假实现会让「重投只建一单」这条断言假绿——它测的是假实现的健忘，不是服务的幂等，而真的那一侧
// 一旦坏了（比如换个幂等键）这一份测试也不会红。

// fakeOrders 是订单域的替身。
type fakeOrders struct {
	// byThirdParty 是订单域那条唯一键的替身：渠道流水号 → 订单。既是幂等的依据，也是 GetOrder 的
	// 索引（首月支付信息那一段会走这条）。
	byThirdParty map[string]*dto.OrderSummary
	// attempts 是**调用次数**（含幂等命中），created 是真正建出来的那几张单。两者分开是为了让
	// 「重投只建一单」能同时断言「又叫了一次」与「没多出一张」。
	attempts int
	created  []dto.RenewalOrderParams
	// createErr 非 nil 时建单一律失败，用来演「钱收了、账记不上」那一格。
	createErr error
}

func newFakeOrders() *fakeOrders {
	return &fakeOrders{byThirdParty: map[string]*dto.OrderSummary{}}
}

func (o *fakeOrders) CreateRenewal(_ context.Context, in dto.RenewalOrderParams) (*dto.RenewalOrder, error) {
	if o == nil {
		return nil, errors.New("fakeOrders 没挂上")
	}
	o.attempts++
	if o.createErr != nil {
		return nil, o.createErr
	}
	if existing, ok := o.byThirdParty[in.ThirdPartyOrderNo]; ok {
		return &dto.RenewalOrder{OrderID: existing.OrderID, OrderNo: existing.OrderNo, Created: false}, nil
	}
	summary := &dto.OrderSummary{
		OrderID:       uuid.NewString(),
		OrderNo:       "SUB" + strings.ReplaceAll(uuid.NewString(), "-", "")[:14],
		UserID:        in.UserID,
		Source:        "renewal",
		Status:        "paid",
		PaidAmount:    in.Amount,
		PaymentMethod: "wechat_papay",
	}
	o.byThirdParty[in.ThirdPartyOrderNo] = summary
	o.created = append(o.created, in)
	return &dto.RenewalOrder{OrderID: summary.OrderID, OrderNo: summary.OrderNo, Created: true}, nil
}

func (o *fakeOrders) GetOrder(_ context.Context, orderID string) (*dto.OrderSummary, error) {
	for _, summary := range o.byThirdParty {
		if summary.OrderID == orderID {
			return summary, nil
		}
	}
	return nil, errors.New("order not found")
}

// outboxPayloadOf 读回本用例发出去的那条事件的事件体。
//
// 按 f.code（这个用例独占的套餐编码）收窄：dev 库是共享的，别的用例、或者上一次跑剩下的行也会
// 落在同一张表里，按 event_type 单取会取到别人的。
func (f *membershipFixture) outboxPayloadOf(eventType string) string {
	f.t.Helper()

	var payload string
	err := f.pool.QueryRow(context.Background(), `
		SELECT convert_from(payload, 'UTF8') FROM message_outbox
		WHERE event_type = $1 AND convert_from(payload, 'UTF8') LIKE $2
		ORDER BY created_at DESC LIMIT 1`, eventType, fmt.Sprintf("%%%q%%", f.code)).Scan(&payload)
	if err != nil {
		f.t.Fatalf("读回 %s 的事件体失败：%v", eventType, err)
	}
	return payload
}

// ============================================================
// 扣款成功：建一张单，且订单号进流水与事件
// ============================================================

// TestIntegrationChargeSucceededRecordsARenewalOrder 是这一刀的主用例。
//
// 它走一遍真实的扣款成功：到期扫描发起这一期 → 渠道回成功 → 事件进来。四件事：
//
//   - 订单域**被叫了一次**，报文里是这一期的钱与签约时冻结的那份快照；
//   - 续费流水上的 order_id 是**那一张单**——不是任意非空值；
//   - membership.renewed 事件体里的 orderId 是**同一个**值（下游 coupon-service 拿它发券，
//     也因此把它当成幂等键的一部分）；
//   - 重投同一条事件：订单域又被叫了一次（这次是幂等命中），**没有多出第二张单**，事件也不再发。
func TestIntegrationChargeSucceededRecordsARenewalOrder(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.seedMembership(t)
	gateway := f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	if _, err := f.svc.ChargeDue(ctx, 200, uuid.NewString()); err != nil {
		t.Fatalf("到期扫描失败：%v", err)
	}
	sent := chargesFor(gateway, subscription.ContractCode)
	if len(sent) != 1 {
		t.Fatalf("这一条订阅发起过 %d 次，想要 1 次", len(sent))
	}

	transactionID := "WXTXN-" + uuid.NewString()[:12]
	settled := chargeEventInput{
		AgreementID:           subscription.AgreementID,
		AgreementNo:           subscription.ContractCode,
		BizPeriod:             sent[0].BizPeriod,
		Amount:                plan.PriceCents,
		ProviderTransactionID: transactionID,
		Status:                dto.ChargeStatusSucceeded,
	}
	f.mustChargeEvent(settled)

	// —— 订单域收到了什么 ——
	if f.orders.attempts != 1 {
		t.Fatalf("订单域被叫了 %d 次，想要 1 次：扣到钱却不记账，后台订单管理里就没有这笔钱",
			f.orders.attempts)
	}
	if len(f.orders.created) != 1 {
		t.Fatalf("建出来 %d 张单，想要 1 张", len(f.orders.created))
	}
	params := f.orders.created[0]
	// 幂等键是**渠道流水号**而不是期次：期次是本服务按 next_charge_at 派生的，同一期在一次重试里
	// 可能算出不同的值，而流水号是渠道给的、一次扣款只有一个。用错了的话重投会建出第二张单。
	if params.ThirdPartyOrderNo != transactionID {
		t.Errorf("订单的幂等键 = %q，想要渠道流水号 %q", params.ThirdPartyOrderNo, transactionID)
	}
	// 金额取**事件里那个**（这一期实际扣了多少），不是订阅上的 price_cents。
	if params.Amount != plan.PriceCents {
		t.Errorf("订单金额 = %d，想要这一期实际扣的 %d", params.Amount, plan.PriceCents)
	}
	// 下单人取自订阅行，不是事件体——订单是对着本域这一行记的账。
	if params.UserID != f.user {
		t.Errorf("订单的下单人 = %q，想要订阅上那个人 %q", params.UserID, f.user)
	}
	if params.MembershipID != f.membership().ID {
		t.Errorf("订单挂的会员 = %q，想要 %q", params.MembershipID, f.membership().ID)
	}

	// 快照是**签约时冻结的那份**：套餐后来调价，已签约用户这一期买的还是原来那些东西。
	row := f.subscription(subscription.ID)
	if params.Plan.PlanID != row.PlanID {
		t.Errorf("快照的套餐 id = %q，想要 %q", params.Plan.PlanID, row.PlanID)
	}
	if params.Plan.PriceCents != row.PriceCents {
		t.Errorf("快照的一期价 = %d，想要订阅上冻结的 %d", params.Plan.PriceCents, row.PriceCents)
	}
	if params.Plan.Period != row.Period || params.Plan.PeriodCount != row.PeriodCount {
		t.Errorf("快照的时长 = %s×%d，想要订阅上的 %s×%d",
			params.Plan.Period, params.Plan.PeriodCount, row.Period, row.PeriodCount)
	}
	if params.Plan.PlanCode != f.membership().PlanCode {
		t.Errorf("快照的套餐编码 = %q，想要会员行上的 %q", params.Plan.PlanCode, f.membership().PlanCode)
	}
	// 会员价那三列照原样搬：coupon-service 发券读的就是快照里的这三格。
	if params.Plan.MemberPriceMode == "" {
		t.Error("快照里没有会员价模式：下游发券判的就是它")
	}

	orderID := f.orders.byThirdParty[transactionID].OrderID

	// —— 订单号落到了哪里 ——
	renewal := f.changeOfType(model.ChangeRenew)
	if renewal == nil {
		t.Fatalf("没有续费流水：%s", changeTypes(f.changes()))
	}
	if renewal.OrderID == nil || *renewal.OrderID != orderID {
		t.Fatalf("续费流水上的订单号 = %v，想要刚建的那张单 %q", renewal.OrderID, orderID)
	}

	// 事件体逐字解一遍再看那一格：拿字符串搜 "orderId" 会把别处的同一个 uuid 也算命中。
	var event dto.MembershipChangedEvent
	if err := json.Unmarshal([]byte(f.outboxPayloadOf(dto.EventMembershipRenewed)), &event); err != nil {
		t.Fatalf("解不开发出去的事件体：%v", err)
	}
	if event.OrderID != orderID {
		t.Fatalf("membership.renewed 的 orderId = %q，想要 %q——**空着下游就不发券**，而两边都不报错",
			event.OrderID, orderID)
	}
	if event.MemberPriceMode != params.Plan.MemberPriceMode {
		t.Errorf("事件里的会员价模式 = %q，想要快照上的 %q", event.MemberPriceMode, params.Plan.MemberPriceMode)
	}

	// —— 重投 ——
	f.mustChargeEvent(settled)

	if f.orders.attempts != 2 {
		t.Errorf("重投之后订单域被叫了 %d 次，想要 2 次（重投会重走一遍 build 那一步，靠幂等挡）",
			f.orders.attempts)
	}
	if len(f.orders.created) != 1 {
		t.Fatalf("重投之后建出来 %d 张单，想要还是 1 张：钱只收了一次，单也只能有一张", len(f.orders.created))
	}
	if got := countOfType(f.changes(), model.ChangeRenew); got != 1 {
		t.Errorf("重投之后续费流水 %d 条，想要 1 条", got)
	}
	if got := f.subscription(subscription.ID).ChargeCount; got != 1 {
		t.Errorf("重投之后扣款次数 = %d，想要还是 1", got)
	}
}

// TestIntegrationChargeSucceededParksWithoutAnOrder 是「钱收了、账记不上」那一格。
//
// 建单失败时**一步都不往下走**：会员不续、订阅不推进、流水不写、事件不发。给用户续上会员只会让
// 账面更难对——钱在渠道那边已经动了，而订单管理里查不到这一笔，事后对账的人手上没有任何东西能
// 解释这个人的会员是哪来的。
//
// 但它**不再返回错误让平台重投**：那 5 次重投是毫秒级的、之后进 panda.events.dlq，而那个队列今天
// 既没有消费者也没有监控（见 charge_settlement.go 的文件头）。订单域重启三分钟，这三分钟里到的
// 扣款成功事件会全部消失，而两边都不报错。所以这一期落成一条**待办**（本用例断言它落了什么），
// 由 worker 重试到订单域回来为止——补上去的那一半在 charge_settlement_integration_test.go 里。
//
// 反过来「先结算、建单失败只记个日志」看着更友好，但那个顺序还有个更硬的毛病：结算一旦成功，
// 重投就会先撞上它的幂等（那一笔渠道流水已经续过了）被安静地 ack 掉，**建单那一步永远轮不到**，
// 订单从此静默地少一张。这正是这一刀要修的那个缺陷的形状。
func TestIntegrationChargeSucceededParksWithoutAnOrder(t *testing.T) {
	f := newMembershipFixture(t)

	plan := f.seedMembership(t)
	f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	before := f.membership()
	rowBefore := f.subscription(subscription.ID)

	orderErr := errors.New("订单域连不上")
	f.orders.createErr = orderErr

	transactionID := "WXTXN-" + uuid.NewString()[:12]
	err := f.chargeEvent(chargeEventInput{
		AgreementID:           subscription.AgreementID,
		AgreementNo:           subscription.ContractCode,
		BizPeriod:             subscription.bizPeriod(),
		Amount:                plan.PriceCents,
		ProviderTransactionID: transactionID,
		Status:                dto.ChargeStatusSucceeded,
	})
	if err != nil {
		t.Fatalf("建单失败时回的是 %v，想要 nil：这一期该落成待办之后被 ack（重投那条路只走毫秒级的 "+
			"5 次，之后进那个没有人看的死信队列）", err)
	}
	if f.parkedCharge(transactionID) == nil {
		t.Fatal("建单失败却没有落下待办：这一笔钱在会员域与订单域两边都没有痕迹")
	}

	// 落成待办不是落成了账：会员一分没续。这一条与「钱到了、权益必须跟上」那条（归属对不上时仍然
	// 续期）是**两种不同的失败**：那一次是账记完了才发现人不对，这一次是账根本没记成。
	if now := f.membership(); !now.ExpireAt.UTC().Equal(before.ExpireAt.UTC()) {
		t.Errorf("建单失败却把会员续了：%v → %v", before.ExpireAt.UTC(), now.ExpireAt.UTC())
	}
	rowAfter := f.subscription(subscription.ID)
	if rowAfter.ChargeCount != rowBefore.ChargeCount || rowAfter.LastChargeAt != nil {
		t.Errorf("建单失败却推进了订阅：chargeCount=%d lastChargeAt=%v",
			rowAfter.ChargeCount, rowAfter.LastChargeAt)
	}
	if changes := f.changes(); countOfType(changes, model.ChangeRenew) != 0 {
		t.Errorf("建单失败却写了续费流水：%s", changeTypes(changes))
	}
}

// TestIntegrationChargeFailedRecordsNoOrder 钉住失败那一支**不建单**。
//
// 那笔钱没收上来，没有订单可言（老系统同此：它只在成功那一支调 createRenewalOrder）。失败的期次
// 留在支付域的 payment_agreement_charges 里，后台订阅详情的「续费明细」看的就是它——那里有金额、
// 失败原因和已试次数，比一张标着「已支付」的空单有用得多。
func TestIntegrationChargeFailedRecordsNoOrder(t *testing.T) {
	f := newMembershipFixture(t)

	plan := f.seedMembership(t)
	f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	f.mustChargeEvent(chargeEventInput{
		AgreementID:    subscription.AgreementID,
		AgreementNo:    subscription.ContractCode,
		BizPeriod:      subscription.bizPeriod(),
		Amount:         plan.PriceCents,
		Status:         dto.ChargeStatusFailed,
		FailureCode:    "NOTENOUGH",
		FailureMessage: "余额不足",
	})

	if f.orders.attempts != 0 {
		t.Errorf("这一期没扣到钱，订单域却被叫了 %d 次", f.orders.attempts)
	}
	if len(f.orders.created) != 0 {
		t.Errorf("失败的一期建出了 %d 张单：那笔钱没收上来", len(f.orders.created))
	}
	// 失败本身照常记账（这一条是 charge_integration_test.go 钉的，这里只确认建单那一步没被牵连）。
	if got := countOfType(f.changes(), model.ChangeChargeFailed); got != 1 {
		t.Errorf("失败流水 %d 条，想要 1 条", got)
	}
}

// TestIntegrationChargeWithoutAnyOrderClientFailsLoudly 钉住「没装订单域」这个装配缺失的处置。
//
// 它不降级成「记不了账就先续期」：那等于把「钱收了、订单没有」变成常态。与签约那条路同一个处置
// ——宁可直接说渠道不可用。这一条用例里**故意不挂订单域**（fixture 默认的 svc 就没有 Orders）。
func TestIntegrationChargeWithoutAnyOrderClientFailsLoudly(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)
	// 不调 f.chargeGateway()：那条路会挂上假订单域。这里要的是「只接了渠道」的进程。
	f.wireSigning(&fakeAgreements{}, &fakeWallets{}, nil)
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	before := f.membership()
	err := f.chargeEvent(chargeEventInput{
		AgreementID:           subscription.AgreementID,
		AgreementNo:           subscription.ContractCode,
		BizPeriod:             subscription.bizPeriod(),
		Amount:                plan.PriceCents,
		ProviderTransactionID: "WXTXN-" + uuid.NewString()[:12],
		Status:                dto.ChargeStatusSucceeded,
	})
	if !errors.Is(err, ErrChannelUnavailable) {
		t.Fatalf("没有订单域时回的是 %v，想要 ErrChannelUnavailable", err)
	}
	if now := f.membership(); !now.ExpireAt.UTC().Equal(before.ExpireAt.UTC()) {
		t.Error("没有订单域却把会员续了：钱收了、账没记，事后对不上")
	}
}
