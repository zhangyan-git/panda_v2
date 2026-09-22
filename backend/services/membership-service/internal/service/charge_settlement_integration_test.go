package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 「钱收了、账没落成」那张待办表的两半：**落下来**（事件消费那一头，见 renewal_order 那一份里
// 的那条用例）与**补上去**（worker 这一头）。
//
// # 它钉的是「这段时间里发生了什么」
//
// 这一格在库里是**空的**——没有流水、没有订单、会员行一个字节都没动，所以任何只看最终状态的断言
// 都测不出它。这一份用例逐条断言中间那段：待办里存的是什么、什么时候被删、补上之后订单号有没有
// 进流水与事件（下游发券读的就是它）。
//
// # 时间轴是这一份用例的重点
//
// 补账发生在**很久以后**（订单域恢复的那一刻），而这一期会员该从**钱收到的那一刻**起算。所以下面
// 那条用例故意把时钟推后两天再补，再断言到期日是按待办里那个时刻算的——用重试时刻会让「这一期
// 从哪天开始」随一次订单域抖动而漂，而那是用户在会员中心看得见的东西。

// parkedCharge 读回某一条待办，没有时返回 nil。
//
// 按主键读而不是按 user 读：这一份用例要断言的正是「这一笔钱有没有落下/有没有被删」，而主键就是
// 那一笔钱的身份（渠道流水号）。
func (f *membershipFixture) parkedCharge(providerTransactionID string) *model.ChargeSettlement {
	f.t.Helper()

	settlement := &model.ChargeSettlement{}
	err := f.pool.QueryRow(context.Background(), `
		SELECT provider_transaction_id, agreement_id, user_id, target, biz_period, amount,
		       occurred_at, trace_id, attempts, next_attempt_at, last_error
		FROM membership_charge_settlements WHERE provider_transaction_id = $1`,
		providerTransactionID,
	).Scan(&settlement.ProviderTransactionID, &settlement.AgreementID, &settlement.UserID,
		&settlement.Target, &settlement.BizPeriod, &settlement.Amount, &settlement.OccurredAt,
		&settlement.TraceID, &settlement.Attempts, &settlement.NextAttemptAt, &settlement.LastError)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		f.t.Fatalf("读待办失败：%v", err)
	}
	return settlement
}

// TestIntegrationParkedChargeHealsWhenTheOrderDomainComesBack 是这张表存在的全部理由。
//
// 三段：
//
//   - 订单域连不上时，事件消费**不报错**（ack），但这一期落成一条待办，会员、订阅、流水一样都没动；
//   - 订单域回来之后，worker 把这一期补上：一张单、一期会员、一条带订单号的流水；
//   - 补完待办就没了（下一次重试不会再捞到它）。
//
// 会员是**两个月前买、一个月前就过期**的那一种，而补账发生在两天之后：这样「续期从哪一刻起算」
// 才有两个不同的候选，断言才钉得住（见文件头那段）。用实时钟的话这两个候选值几乎一样，写错了也看不出来。
func TestIntegrationParkedChargeHealsWhenTheOrderDomainComesBack(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	// 两个月前买的包月会员：此刻已经过期。
	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)
	f.mustPay(uuid.NewString(), plan, f.clock.AddDate(0, -2, 0))

	f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	before := f.membership()
	if !before.ExpireAt.Before(f.clock) {
		t.Fatalf("这一格要求会员已经过期，实际到期日是 %v（此刻 %v）", before.ExpireAt.UTC(), f.clock.UTC())
	}
	parkedAt := f.clock

	// —— 一、订单域连不上 ——
	f.orders.createErr = errors.New("订单域连不上")
	transactionID := "WXTXN-" + uuid.NewString()[:12]
	settled := chargeEventInput{
		AgreementID:           subscription.AgreementID,
		AgreementNo:           subscription.ContractCode,
		BizPeriod:             subscription.bizPeriod(),
		Amount:                plan.PriceCents,
		ProviderTransactionID: transactionID,
		Status:                dto.ChargeStatusSucceeded,
	}
	if err := f.chargeEvent(settled); err != nil {
		t.Fatalf("订单域连不上时回的是 %v，想要 nil：这一期该落成待办之后被 ack，而不是压在队列里"+
			"（平台的 5 次重投是毫秒级的，之后进那个没有人看的死信队列）", err)
	}

	parked := f.parkedCharge(transactionID)
	if parked == nil {
		t.Fatalf("订单域连不上却没有落下待办：这一笔钱在会员域与订单域两边都没有痕迹")
	}
	if parked.AgreementID != subscription.AgreementID {
		t.Errorf("待办里的协议 = %q，想要 %q", parked.AgreementID, subscription.AgreementID)
	}
	if parked.UserID != f.user {
		t.Errorf("待办里的用户 = %q，想要 %q", parked.UserID, f.user)
	}
	if parked.Target != dto.ChargeStatusSucceeded {
		t.Errorf("待办里的结论 = %q，想要 %q", parked.Target, dto.ChargeStatusSucceeded)
	}
	if parked.BizPeriod != subscription.bizPeriod() {
		t.Errorf("待办里的期次 = %q，想要 %q", parked.BizPeriod, subscription.bizPeriod())
	}
	if parked.Amount != plan.PriceCents {
		t.Errorf("待办里的金额 = %d，想要这一期实际扣的 %d", parked.Amount, plan.PriceCents)
	}
	// 存的是**事件到达的那一刻**。重试时用它当续期起点，所以它错了这一期就从错的日子开始算。
	if !parked.OccurredAt.UTC().Equal(parkedAt.UTC()) {
		t.Errorf("待办里的时刻 = %v，想要事件到达的 %v", parked.OccurredAt.UTC(), parkedAt.UTC())
	}

	// 落成待办**不是**落成了账：会员、订阅、流水一样都不能动。
	if now := f.membership(); !now.ExpireAt.UTC().Equal(before.ExpireAt.UTC()) {
		t.Errorf("建单失败却把会员续了：%v → %v", before.ExpireAt.UTC(), now.ExpireAt.UTC())
	}
	if changes := f.changes(); countOfType(changes, model.ChangeRenew) != 0 {
		t.Errorf("建单失败却写了续费流水：%s", changeTypes(changes))
	}

	// —— 二、订单域回来了，而时间已经走到两天之后 ——
	f.orders.createErr = nil
	f.clock = f.clock.Add(48 * time.Hour)

	healed, err := f.svc.SettlePendingCharges(ctx, 200)
	if err != nil {
		t.Fatalf("重试待办失败：%v", err)
	}
	if healed != 1 {
		t.Fatalf("这一轮落成了 %d 条，想要 1 条（多于 1 说明库里还有别的待办）", healed)
	}

	if len(f.orders.created) != 1 {
		t.Fatalf("订单域收到 %d 张单，想要 1 张——补账时没有建单，这一期就没有账面", len(f.orders.created))
	}
	// 幂等键还是那个渠道流水号：重试建的是同一张单，不是第二张。
	if got := f.orders.created[0].ThirdPartyOrderNo; got != transactionID {
		t.Errorf("补建的单的幂等键 = %q，想要 %q", got, transactionID)
	}
	orderID := f.orders.byThirdParty[transactionID].OrderID

	renewal := f.changeOfType(model.ChangeRenew)
	if renewal == nil {
		t.Fatalf("补账之后没有续费流水：%s", changeTypes(f.changes()))
	}
	if renewal.OrderID == nil || *renewal.OrderID != orderID {
		t.Errorf("续费流水上的订单号 = %v，想要补建的那张单 %q", renewal.OrderID, orderID)
	}

	// 续期从**待办里那个时刻**起算，不是从重试那一刻：晚两天补上不该多送两天会员。
	row := f.subscription(subscription.ID)
	want := model.AddPeriod(parkedAt, row.Period, row.PeriodCount)
	if after := f.membership(); !after.ExpireAt.UTC().Equal(want.UTC()) {
		t.Errorf("补账之后的到期日 = %v，想要 %v（从事件到达那一刻 %v 起算，不是从重试那一刻 %v）",
			after.ExpireAt.UTC(), want.UTC(), parkedAt.UTC(), f.clock.UTC())
	}

	// 事件里带的必须是那张单的号：coupon-service 拿它区分「一次真实成交」与「后台人工改了有效期」，
	// 空着它就不发券，而两边都不报错。
	var event dto.MembershipChangedEvent
	if err := json.Unmarshal([]byte(f.outboxPayloadOf(dto.EventMembershipRenewed)), &event); err != nil {
		t.Fatalf("解不开发出去的事件体：%v", err)
	}
	if event.OrderID != orderID {
		t.Errorf("membership.renewed 的 orderId = %q，想要 %q", event.OrderID, orderID)
	}

	// —— 三、落成了就不该再留一行 ——
	if f.parkedCharge(transactionID) != nil {
		t.Error("补上之后待办还在：下一轮会拿着同一笔流水把两步再做一遍")
	}
}

// TestIntegrationParkedChargeIsIdempotent 钉住「同一笔钱只落一行」。
//
// 一份事件投两次而两次都失败是常见的（broker 重连、处理完没 ack 时崩溃都会重投），而两次落下的
// 是**同一份事实**。主键是渠道流水号，第二次是 DO NOTHING——不该把第一次的 attempts 清零重来，
// 更不该落出两行（两行会让同一次补账被做两遍）。
func TestIntegrationParkedChargeIsIdempotent(t *testing.T) {
	f := newMembershipFixture(t)

	plan := f.seedMembership(t)
	f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	f.orders.createErr = errors.New("订单域连不上")
	settled := chargeEventInput{
		AgreementID:           subscription.AgreementID,
		AgreementNo:           subscription.ContractCode,
		BizPeriod:             subscription.bizPeriod(),
		Amount:                plan.PriceCents,
		ProviderTransactionID: "WXTXN-" + uuid.NewString()[:12],
		Status:                dto.ChargeStatusSucceeded,
	}
	f.mustChargeEvent(settled)
	f.mustChargeEvent(settled)

	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM membership_charge_settlements WHERE user_id = $1`, f.user).Scan(&count); err != nil {
		t.Fatalf("数待办失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("同一笔钱落下了 %d 条待办，想要 1 条", count)
	}
}

// TestIntegrationParkedChargeKeepsRetryingWithoutTheOrderDomain 钉住「不放弃」。
//
// 这个进程压根没接订单域（装配缺失）时，重试也建不成单——但**待办绝不能删**：删行等于把这一笔已经
// 收了的钱从账上抹掉，而它是一笔对不上账的钱。所以这一轮什么都没落成，行还在，attempts 涨了一次，
// last_error 记下了原因（那是唯一一条通向「有人来看」的线索）。
//
// 与事件入口的处置不同是有意的：那边有一条死信队列可以交出去（那里今天没有人看，但至少消息还在），
// 这边**没有别的地方能存这笔钱**，所以只能留在表里一直试。
func TestIntegrationParkedChargeKeepsRetryingWithoutTheOrderDomain(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.seedMembership(t)
	f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	transactionID := "WXTXN-" + uuid.NewString()[:12]
	// 直接落一条待办：没有订单域时事件入口会返回错误进死信（见 charge_gateway 那几条），落不了表
	// ——而这一条用例要测的正是「表里已经有行、而订单域还没接上」那一段时间。
	if err := f.svc.repository.ParkChargeSettlement(ctx, repository.ChargeSettleParams{
		AgreementID:           subscription.AgreementID,
		UserID:                f.user,
		Target:                dto.ChargeStatusSucceeded,
		BizPeriod:             subscription.bizPeriod(),
		Amount:                plan.PriceCents,
		ProviderTransactionID: transactionID,
		OccurredAt:            f.clock,
	}); err != nil {
		t.Fatalf("落待办失败：%v", err)
	}
	// 把订单域摘掉：这一轮的重试一定会失败。
	f.wireSigning(&fakeAgreements{}, &fakeWallets{}, nil)

	healed, err := f.svc.SettlePendingCharges(ctx, 200)
	if err != nil {
		t.Fatalf("重试待办时回了错误 %v，想要 nil：一条失败不该让整轮中断", err)
	}
	if healed != 0 {
		t.Fatalf("没有订单域却落成了 %d 条", healed)
	}

	parked := f.parkedCharge(transactionID)
	if parked == nil {
		t.Fatal("重试失败之后待办被删了：这一笔收了的钱从此没有任何地方记着")
	}
	if parked.Attempts != 1 {
		t.Errorf("试过 %d 次，想要 1 次（认领时加一）", parked.Attempts)
	}
	if parked.LastError == nil || *parked.LastError == "" {
		t.Error("失败没有留下 last_error：这条待办从此只有下一次 ERROR 日志能解释它")
	}
	if !parked.NextAttemptAt.After(f.clock) {
		t.Errorf("下一次重试时间 = %v，想要晚于此刻 %v", parked.NextAttemptAt.UTC(), f.clock.UTC())
	}
}
