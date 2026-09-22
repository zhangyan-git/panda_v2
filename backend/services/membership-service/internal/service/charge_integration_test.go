package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// 委托代扣里**扣款那一半**：到期扫描发起、扣款结果回来之后怎么落。
//
// # 与 signing_integration_test.go 同一套家当
//
// 渠道还是假的（复用那个 fakeAgreements），库是真的。真库在这一份里多担一件事：结算走的是
// repository.SettleCharge——它一个事务里改订阅计数、续会员、写流水、落事件，而「同一笔渠道
// 流水只续一期」钉在 membership_changes_request_unique 那条部分唯一索引上。这些错（列名写错、
// 计数少加一次、幂等键撞不上）编译期看不出来，只在真库上浮出来，且浮出来的方式是线上多送一个
// 月或者少送一个月。
//
// # 绝不真连 payment-service
//
// 扣款那条路往下挂的是**真实的微信扣款**（支付那边的适配器打的是 mTLS 的 pappayapply）。所以
// 这里注入的永远是一个假的 AgreementGateway，没有任何用例能在这台机器上真扣到谁的钱。
//
// 假渠道默认回的是 charging 而不是 succeeded（见 fakeAgreements.Charge 的说明）：受理不等于
// 扣到钱，而这条链上一半的判据就是为了不把受理当成钱。
//
// 门禁同上：没有 MEMBERSHIP_DATABASE_URL 就 skip。

// ============================================================
// 造一条「能扣」的订阅
// ============================================================

// signedSubscription 是一条带代扣协议的订阅在用例里的形状。
type signedSubscription struct {
	ID          string
	AgreementID string
	// ContractCode 是商户协议号，即交给渠道的那个 contract_code，也是发起扣款时用的 agreement_no。
	ContractCode string
	NextChargeAt time.Time
}

// bizPeriod 是这一期的期次。
//
// 用例里各处用它而不是自己抄一份期望值：期次是**从 next_charge_at 派生的**这件事本身就是要钉
// 的东西，抄一份字面量等于把那条规则在测试里再写一遍，两处迟早走偏。
func (s *signedSubscription) bizPeriod() string {
	return model.ChargePeriod(s.NextChargeAt)
}

// dueSubscription 造一条**到点的**、带代扣协议的 active 订阅。
//
// 协议那两个值由 mustSubscribe 一起写（真实签约在发起那一刻就落库了），这里把它们读回来给断言
// 用：chargeSubscription 拿 contract_code 当商户协议号发出去，回来的事件拿 agreement_id 命回
// 这一行——两处断言都不该自己去编一个值。
func (f *membershipFixture) dueSubscription(plan *model.Plan, nextChargeAt time.Time) *signedSubscription {
	f.t.Helper()

	subscription := &signedSubscription{
		ID:           f.mustSubscribe(plan, model.SubscriptionStatusActive, &nextChargeAt, f.clock),
		NextChargeAt: nextChargeAt,
	}
	row := f.subscription(subscription.ID)
	if row.AgreementID == nil || *row.AgreementID == "" || row.ContractCode == "" {
		f.t.Fatalf("订阅上没有协议信息（agreement_id=%v contract_code=%q）：这一条链在第一步就断了",
			row.AgreementID, row.ContractCode)
	}
	subscription.AgreementID, subscription.ContractCode = *row.AgreementID, row.ContractCode
	return subscription
}

// chargeGateway 挂上一份假的渠道**与一份假的订单域**，返回渠道那份供断言「有没有真的发起」。
//
// 订单域在这里是**必需**的：扣款成功那一支会先去订单域记一张续费单（见 service.recordRenewalOrder），
// 不挂的话每一条成功用例都会撞在 ErrChannelUnavailable 上——那不是「这一份用例不关心订单」，那是
// 一个连渠道都没接全的进程。假订单域本身在 renewal_order_integration_test.go 里，断言它收到了
// 什么也在那一份里。
//
// Wallets 给的是「这个人没绑小程序」：这一份用例一次都不该走到签约那条路上去，真走到了会撞在
// ErrWalletIdentityRequired 上，而不是静默通过。
func (f *membershipFixture) chargeGateway() *fakeAgreements {
	f.t.Helper()
	agreements := &fakeAgreements{}
	f.orders = newFakeOrders()
	f.wireSigning(agreements, &fakeWallets{}, f.orders)
	return agreements
}

// chargesFor 挑出打给**这一份协议**的那些请求。
//
// 不按全部调用数断言：dev 库是共享的，别的用例（或上一次跑剩的）留下的到期订阅也会被同一轮扫描
// 捞到，而它们打的是同一份假渠道。判据要能只看自己那一条。
func chargesFor(gateway *fakeAgreements, contractCode string) []dto.ChargeAgreementParams {
	var matched []dto.ChargeAgreementParams
	for _, charge := range gateway.charges {
		if charge.AgreementNo == contractCode {
			matched = append(matched, charge)
		}
	}
	return matched
}

// autoRenewOn 把会员行的自动续费开关拨开。
//
// 它平时由签约生效那一步在同一个事务里拨（见 repository.applySettle 落到 active 那一支），而
// 这里造出来的订阅是直接插的，所以补一下——不然「关开关」这个动作在库里看不出任何变化。
func (f *membershipFixture) autoRenewOn() {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE memberships SET auto_renew = TRUE WHERE id = $1`, f.membership().ID); err != nil {
		f.t.Fatalf("拨开自动续费失败：%v", err)
	}
}

// ============================================================
// 关自动续费就是解约
// ============================================================

// offAutoRenew 是会员中心那个「关闭自动续费」按钮上的一件事：**真的去微信解约**。
//
// 这三条用例合起来钉的是一个顺序不变式——先解渠道、成功了才改本地（见
// service.terminateForSubscription）。它反过来的写法在库里看不出任何异常：后台与小程序都显示
// 「已关闭」，而微信那边的协议还挂着，下个月照扣。所以这里三条各自看一件事：解成了本地跟着变、
// 解不成**本地一个字节都不变**、压根没有协议时不去麻烦渠道。
func TestIntegrationTurningOffAutoRenewTerminatesTheAgreement(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()
	plan := f.seedMembership(t)

	sub := f.dueSubscription(plan, f.clock.Add(24*time.Hour))
	f.autoRenewOn()

	agreements := f.chargeGateway()
	membership, err := f.svc.SetAutoRenewByUser(ctx, f.user, false, uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("关闭自动续费失败：%v", err)
	}

	// 渠道那一份真的解掉了，解的是**这条订阅上那个协议号**——本地换一行协议号而渠道解了另一份，
	// 这一整条链就白走了。原因也得是同一句：它同时落库（下面断言）并发给微信，客服在两边看到的
	// 必须是同一件事。
	if len(agreements.terminates) != 1 {
		t.Fatalf("解约调用 = %d 次，想要 1 次（不去解约就只是翻了个本地开关，下个月照样会扣）",
			len(agreements.terminates))
	}
	terminated := agreements.terminates[0]
	if terminated.AgreementNo != sub.ContractCode {
		t.Errorf("交给渠道的协议号 = %q，想要订阅上那个 %q", terminated.AgreementNo, sub.ContractCode)
	}
	if terminated.Reason != "用户关闭自动续费" {
		t.Errorf("交给渠道的原因 = %q，想要「用户关闭自动续费」", terminated.Reason)
	}

	// 本地收口：订阅结束，发起人是**用户自己**（与「渠道说这份协议作废了」那条路区分开——那一列
	// 留空，见 SettleParams），开关跟着灭。三者是 settle 那一个事务里一起改的。
	settled := f.subscription(sub.ID)
	if settled.Status != model.SubscriptionStatusCancelled {
		t.Errorf("订阅状态 = %q，想要 cancelled", settled.Status)
	}
	if settled.CancelAt == nil {
		t.Error("cancel_at 应当有值（库上那条 CHECK 就是这么要求的）")
	}
	if settled.CancelReason != "用户关闭自动续费" {
		t.Errorf("库里的取消原因 = %q", settled.CancelReason)
	}
	if settled.CancelledBy == nil || *settled.CancelledBy != f.user {
		t.Errorf("解约发起人 = %v，想要的用户是 %s", settled.CancelledBy, f.user)
	}
	if membership.AutoRenew {
		t.Error("接口回读的会员上自动续费还是开着的——小程序那边会继续显示「自动续费：开」")
	}
	if f.membership().AutoRenew {
		t.Error("库里的会员开关没灭")
	}

	// 流水：客服要查的是「他什么时候关的」，所以这一条必须留下。
	off := f.changeOfType(model.ChangeAutoRenewOff)
	if off == nil {
		t.Fatalf("没有 auto_renew_off 流水（全部：%s）", changeTypes(f.changes()))
	}
	if off.OperatorType != model.OperatorUser {
		t.Errorf("关开关的是用户自己，流水的 operator_type = %q，想要 %q", off.OperatorType, model.OperatorUser)
	}
	// **一次**：解约与写流水是同一个事务，重放或两处各写一次在这条断言下都会露出来。
	if n := countOfType(f.changes(), model.ChangeAutoRenewOff); n != 1 {
		t.Errorf("auto_renew_off 流水 = %d 条，想要 1 条", n)
	}
}

// TestIntegrationTurningOffAutoRenewChangesNothingWhenTheChannelFails 钉住那条顺序不变式的
// 反面，也是这三条里最要紧的一条：**渠道没解成，本地什么都不改**。
//
// 「什么都不改」是这里唯一的正确结局。改成「先改本地、解约失败只记个日志」看着更友好（用户不
// 会看到报错），但那种友好是假的：微信那边的协议还在，下一轮到期扫描照样为这条 active 的订阅
// 发起扣款，而用户手机里显示的是「已关闭自动续费」。
func TestIntegrationTurningOffAutoRenewChangesNothingWhenTheChannelFails(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()
	plan := f.seedMembership(t)

	sub := f.dueSubscription(plan, f.clock.Add(24*time.Hour))
	f.autoRenewOn()

	agreements := f.chargeGateway()
	channelErr := errors.New("渠道连接超时")
	agreements.terminateErr = channelErr

	if _, err := f.svc.SetAutoRenewByUser(ctx, f.user, false, uuid.NewString(), uuid.NewString()); !errors.Is(err, channelErr) {
		// 原样抛出去的那条错：吞掉它换一句「操作失败」也行，但支付侧超时与「渠道明确拒绝」是
		// 两回事（后者见下一个用例），把两者的因果掐掉，日志里就只剩一句没有来由的失败。
		t.Fatalf("渠道失败时回的是 %v，想要把 %v 原样带出来", err, channelErr)
	}

	if len(agreements.terminates) != 1 {
		t.Errorf("解约调用 = %d 次，想要 1 次（它该去试一次，只是没成）", len(agreements.terminates))
	}
	if !f.membership().AutoRenew {
		t.Error("解约没成却把开关灭了：小程序显示「已关闭」，而微信那边的协议还挂着，下个月照扣")
	}
	settled := f.subscription(sub.ID)
	if settled.Status != model.SubscriptionStatusActive || settled.CancelAt != nil {
		t.Errorf("解约没成却改了本地订阅：status=%q cancel_at=%v", settled.Status, settled.CancelAt)
	}
	if f.changeOfType(model.ChangeAutoRenewOff) != nil {
		t.Error("解约没成却写了 auto_renew_off 流水")
	}

	// 渠道**明确拒绝**（FailureCode）是业务结论，不是故障：同样什么都不改，但回的错要能让运营
	// 看出来「是渠道说不行」，出口是后台的「同步」——它拿渠道的原话纠正本地。
	agreements.terminateErr = nil
	agreements.terminate = &dto.AgreementTermination{
		AgreementNo:    sub.ContractCode,
		Status:         dto.AgreementStatusActive,
		FailureCode:    "CONTRACT_NOT_EXIST",
		FailureMessage: "商户协议号不存在",
	}
	if _, err := f.svc.SetAutoRenewByUser(ctx, f.user, false, uuid.NewString(), uuid.NewString()); !errors.Is(err, ErrAgreementTerminateRefused) {
		t.Fatalf("渠道明确拒绝时回的是 %v，想要 ErrAgreementTerminateRefused", err)
	}
	if !f.membership().AutoRenew || f.subscription(sub.ID).Status != model.SubscriptionStatusActive {
		t.Error("渠道拒绝了却改了本地：这条订阅在微信那边还活着，本地先「关掉」只是把扣款藏了起来")
	}
}

// TestIntegrationTurningOffAutoRenewWithoutASubscriptionOnlyFlipsTheFlag 钉住另一条路：名下
// 没有代扣订阅时，这个开关就是开关本身。
//
// 券发放与后台开通那两条路给的是一段会员，不是一份代扣授权（库上 agreement_id 为空）。这里
// **故意不挂渠道**（fixture 默认的 svc 就没有 Agreements）：这条路上压根不需要支付域那条连接，
// 挂上反而会把「它有没有偷偷去问渠道」这件事盖住。
func TestIntegrationTurningOffAutoRenewWithoutASubscriptionOnlyFlipsTheFlag(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	f.seedMembership(t)
	f.autoRenewOn()

	membership, err := f.svc.SetAutoRenewByUser(ctx, f.user, false, uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("没有订阅时关闭自动续费失败：%v（这条路上不该需要支付域）", err)
	}
	if membership.AutoRenew {
		t.Error("开关没灭")
	}
	if f.changeOfType(model.ChangeAutoRenewOff) == nil {
		t.Errorf("没有 auto_renew_off 流水（全部：%s）", changeTypes(f.changes()))
	}
}

// ============================================================
// 扣款结果事件
// ============================================================

// chargeEventInput 是一条扣款结果事件在用例里的形状。
type chargeEventInput struct {
	// EventType 留空时由 Status 推：succeeded 推成功那条、failed 推失败那条。要故意让两者对不上
	// （验发出方拼错了字段会怎样）时才显式给。
	EventType   string
	AgreementID string
	AgreementNo string
	BizPeriod   string
	Amount      int64
	// ProviderTransactionID 成功那条必须有——它是那条续期流水的幂等键；失败那条按渠道的真实
	// 行为可能是空的。
	ProviderTransactionID string
	Status                string
	FailureCode           string
	FailureMessage        string
	// UserID 留空时用 f.user。要造「签约人对不上」时才给别的值。
	UserID string
}

// chargeEvent 投一条扣款结果事件给消费者。
//
// payload **手写 JSON**，与 pay 那条同一个理由：这一层要钉的正是两边 json tag 对得上（消费者用
// DisallowUnknownFields 解它），拿自己的结构体序列化再解回来是个自证。
func (f *membershipFixture) chargeEvent(in chargeEventInput) error {
	f.t.Helper()

	eventType := in.EventType
	if eventType == "" {
		eventType = dto.EventAgreementChargeSucceeded
		if in.Status == dto.ChargeStatusFailed {
			eventType = dto.EventAgreementChargeFailed
		}
	}
	userID := in.UserID
	if userID == "" {
		userID = f.user
	}
	payload := fmt.Sprintf(`{
		"agreementId": %q,
		"agreementNo": %q,
		"userId": %q,
		"bizPeriod": %q,
		"amount": %d,
		"providerTransactionId": %q,
		"status": %q,
		"failureCode": %q,
		"failureMessage": %q
	}`, in.AgreementID, in.AgreementNo, userID, in.BizPeriod, in.Amount,
		in.ProviderTransactionID, in.Status, in.FailureCode, in.FailureMessage)

	return f.svc.HandleEvent(context.Background(), messaging.Envelope{
		EventID:      uuid.NewString(),
		EventType:    eventType,
		EventVersion: dto.EventVersion,
		TraceID:      uuid.NewString(),
		Payload:      []byte(payload),
	})
}

// mustChargeEvent 是 chargeEvent 的失败即终止版本。
func (f *membershipFixture) mustChargeEvent(in chargeEventInput) {
	f.t.Helper()
	if err := f.chargeEvent(in); err != nil {
		f.t.Fatalf("消费 %s 那条事件失败：%v", in.Status, err)
	}
}

// ============================================================
// 到期扫描：发起
// ============================================================

// TestIntegrationChargeDueSendsThePeriodDerivedFromNextChargeAt 走一遍到期扫描。
//
// 三件事：
//
//   - 发出去的期次是**从 next_charge_at 派生的**（不是今天），金额与账单上的那句话取订阅/套餐
//     上的快照；
//   - 这一趟**一个本地字段都不改**——受理不等于扣到钱，推进只发生在扣款成功那条事件里；
//   - 第二轮扫描算出来的是**同一个期次**。这条是整条链的地基：支付侧的幂等键是 (协议, 期次)，
//     期次一变，重试就成了重复扣款。
func TestIntegrationChargeDueSendsThePeriodDerivedFromNextChargeAt(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.seedMembership(t)
	gateway := f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	before := f.subscription(subscription.ID)

	charged, err := f.svc.ChargeDue(ctx, 200, uuid.NewString())
	if err != nil {
		t.Fatalf("到期扫描失败：%v", err)
	}
	if charged < 1 {
		t.Fatal("扫描说一条都没发起：这一行明明是到点的 active")
	}

	sent := chargesFor(gateway, subscription.ContractCode)
	if len(sent) != 1 {
		t.Fatalf("这一条订阅发起过 %d 次，想要 1 次", len(sent))
	}
	request := sent[0]
	if request.BizPeriod != subscription.bizPeriod() {
		t.Errorf("期次 = %q，想要由 next_charge_at（%s）派生的 %q",
			request.BizPeriod, subscription.NextChargeAt.UTC(), subscription.bizPeriod())
	}
	if request.Amount != plan.PriceCents {
		t.Errorf("金额 = %d，想要订阅上那个签约时的快照 %d", request.Amount, plan.PriceCents)
	}
	if request.Subject != plan.Name {
		t.Errorf("账单上的那句话 = %q，想要套餐名 %q", request.Subject, plan.Name)
	}
	// 流水号只进支付侧的流水（幂等键是协议 + 期次），用处是排查时从那边回到本服务这一行。
	if want := "charge:" + subscription.ID + ":" + subscription.bizPeriod(); request.RequestID != want {
		t.Errorf("排查用的流水号 = %q，想要 %q", request.RequestID, want)
	}

	// 假渠道回的是 charging（受理），本地因此一个字节都不该动。
	after := f.subscription(subscription.ID)
	if after.Status != model.SubscriptionStatusActive {
		t.Errorf("发起之后的订阅状态 = %q，想要还是 %q", after.Status, model.SubscriptionStatusActive)
	}
	if after.NextChargeAt == nil || !after.NextChargeAt.Equal(*before.NextChargeAt) {
		t.Errorf("发起就把下一期扣款时间推走了：%v → %v（受理不等于扣到钱）",
			before.NextChargeAt, after.NextChargeAt)
	}
	if after.ChargeCount != 0 || after.LastChargeAt != nil {
		t.Errorf("还没扣到钱就记了扣款次数：count=%d lastChargeAt=%v", after.ChargeCount, after.LastChargeAt)
	}
	// 一条流水都不该多：这一趟除了那次出网请求什么都没发生。
	if changes := f.changes(); countOfType(changes, model.ChangeRenew) != 0 ||
		countOfType(changes, model.ChangeChargeFailed) != 0 {
		t.Errorf("只是发起了一次扣款，流水里却多了东西：%s", changeTypes(changes))
	}

	// 第二轮：这一期还没扣成，next_charge_at 没动，所以算出来的必须还是同一个期次。
	if _, err := f.svc.ChargeDue(ctx, 200, uuid.NewString()); err != nil {
		t.Fatalf("第二轮扫描失败：%v", err)
	}
	again := chargesFor(gateway, subscription.ContractCode)
	if len(again) != 2 {
		t.Fatalf("两轮扫描之后这一条订阅共发起 %d 次，想要 2 次（本服务不记「这一期发过了」，挡住重复的是支付侧那条唯一键）", len(again))
	}
	if again[1].BizPeriod != again[0].BizPeriod {
		t.Fatalf("第二轮换了期次：%q → %q；期次一变，(协议, 期次) 那条唯一键就拦不住重复扣款了",
			again[0].BizPeriod, again[1].BizPeriod)
	}
}

// ============================================================
// 扣款成功：续一期，且只续一期
// ============================================================

// TestIntegrationChargeSucceededRenewsOnceAndOnlyOnce 是成功那一支。
//
// 钉住：会员到期日与 next_charge_at 各往前推一个周期（基点还是那条「有剩余就叠加」的老规则）、
// 三个计数器落到该有的值、流水上挂着渠道流水号与**上游建的那张续费单**，以及**同一次扣款的两条
// 事件只续一期**——挡住第二次的不是服务层的自觉，是那条唯一索引。
//
// 订单域收到了什么、事件里的 orderId 是什么，在 renewal_order_integration_test.go 里断言；这里
// 只看它落了库。
func TestIntegrationChargeSucceededRenewsOnceAndOnlyOnce(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.seedMembership(t)
	gateway := f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

	// 先让这一期真的发起出去（假渠道回 charging，钱还没到）。
	if _, err := f.svc.ChargeDue(ctx, 200, uuid.NewString()); err != nil {
		t.Fatalf("到期扫描失败：%v", err)
	}
	sent := chargesFor(gateway, subscription.ContractCode)
	if len(sent) != 1 {
		t.Fatalf("这一条订阅发起过 %d 次，想要 1 次", len(sent))
	}

	before := f.membership()
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

	// 会员：从原到期日叠加一个周期。会员此刻还没到期（还有一年），所以基点就是原到期日——这正是
	// 与 order.paid 那条续费共用的规则，代扣不该另算一套。
	renewed := f.membership()
	wantExpire := model.AddPeriod(before.ExpireAt, plan.Period, plan.PeriodCount)
	if !renewed.ExpireAt.UTC().Equal(wantExpire.UTC()) {
		t.Fatalf("续期后的到期日 = %s，想要 %s（从原到期日 %s 叠加一个周期）",
			renewed.ExpireAt.UTC(), wantExpire.UTC(), before.ExpireAt.UTC())
	}
	if renewed.RenewalCount != before.RenewalCount+1 {
		t.Errorf("续费次数 = %d，想要 %d", renewed.RenewalCount, before.RenewalCount+1)
	}
	if renewed.ID != before.ID {
		t.Error("代扣续期新开了一行会员：得还是同一个人，否则积分、券与历史流水都对不上")
	}

	// 订阅：下一期扣款时间就是新的会员到期日（两处必须是同一个值，各算一遍迟早走偏）。
	row := f.subscription(subscription.ID)
	if row.NextChargeAt == nil || !row.NextChargeAt.UTC().Equal(renewed.ExpireAt.UTC()) {
		t.Errorf("下一期扣款时间 = %v，想要新的会员到期日 %s", row.NextChargeAt, renewed.ExpireAt.UTC())
	}
	if row.ChargeCount != 1 {
		t.Errorf("扣款次数 = %d，想要 1", row.ChargeCount)
	}
	if row.LastChargeAt == nil {
		t.Error("扣到了钱却没记最后一次扣款时间")
	}
	if row.FailedCount != 0 || row.ConsecutiveFailedCount != 0 {
		t.Errorf("扣成了之后失败计数没归零：failed=%d consecutive=%d", row.FailedCount, row.ConsecutiveFailedCount)
	}

	// 流水：一条 renew，挂着渠道流水号（那就是它的幂等键）与**上游建的那张续费单的 id**。
	renewal := f.changeOfType(model.ChangeRenew)
	if renewal == nil {
		t.Fatalf("没有续费流水：%s", changeTypes(f.changes()))
	}
	if renewal.RequestID != transactionID {
		t.Errorf("续费流水的幂等键 = %q，想要渠道流水号 %q", renewal.RequestID, transactionID)
	}
	if renewal.OrderID == nil {
		t.Error("续费流水上没有订单号：代扣是**有订单的**（source=renewal，后台订单管理里能查到），" +
			"客服拿这一行去翻订单时会翻不到")
	}
	if renewal.OperatorType != model.OperatorSystem {
		t.Errorf("操作人类型 = %q，想要 %q（钱是渠道扣的，没有人点过）", renewal.OperatorType, model.OperatorSystem)
	}
	if renewal.ToExpireAt == nil || !renewal.ToExpireAt.UTC().Equal(renewed.ExpireAt.UTC()) {
		t.Errorf("续费流水的 to_expire_at = %v，想要 %s", renewal.ToExpireAt, renewed.ExpireAt.UTC())
	}
	assertOutboxEvent(t, f, dto.EventMembershipRenewed)

	// 重投：同一次扣款的另一条事件（**新的 event id**，同一条渠道流水）。挡住它的必须是那条唯一
	// 索引，不能只是平台收件箱——补发、重放、换一个 event id 的第二次，都会走到这里。
	f.mustChargeEvent(settled)

	replayed := f.membership()
	if !replayed.ExpireAt.UTC().Equal(renewed.ExpireAt.UTC()) {
		t.Fatalf("重投把会员又续了一期：%s → %s", renewed.ExpireAt.UTC(), replayed.ExpireAt.UTC())
	}
	if got := f.subscription(subscription.ID).ChargeCount; got != 1 {
		t.Errorf("重投之后扣款次数 = %d，想要还是 1", got)
	}
	if got := countOfType(f.changes(), model.ChangeRenew); got != 1 {
		t.Errorf("重投之后续费流水 %d 条，想要 1 条", got)
	}
}

// ============================================================
// 扣款失败：计数，到阈值停扣
// ============================================================

// TestIntegrationChargeFailedCountsThenSuspendsAtTheThreshold 是失败那一支。
//
// 三件事：
//
//   - 每一次失败都**不动 next_charge_at**——这一期还没扣成、还没走完，下一轮扫描该拿同一个期次
//     接着试。把它推走等于这一期就此作废；
//   - 每一次失败都**不动权益**：扣款失败改不了这个人的会员资格（会员价、券都还跟着 expire_at）；
//   - 连到阈值就停扣。**停的是扣款，不是解约**——协议还在微信上挂着，本服务不替用户撤回授权。
func TestIntegrationChargeFailedCountsThenSuspendsAtTheThreshold(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.seedMembership(t)
	gateway := f.chargeGateway()
	subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))
	before := f.membership()

	failure := chargeEventInput{
		AgreementID: subscription.AgreementID,
		AgreementNo: subscription.ContractCode,
		BizPeriod:   subscription.bizPeriod(),
		Amount:      plan.PriceCents,
		Status:      dto.ChargeStatusFailed,
		FailureCode: "NOTENOUGH",
		// 渠道拒一笔时不给流水号，这里照实留空：失败那条**没有幂等键**（一期合理地失败三次）。
		FailureMessage: "余额不足",
	}

	// 阈值之前的那几次：只累加。
	for attempt := 1; attempt < model.MaxConsecutiveChargeFailures; attempt++ {
		f.mustChargeEvent(failure)

		row := f.subscription(subscription.ID)
		if row.Status != model.SubscriptionStatusActive {
			t.Fatalf("第 %d 次失败之后订阅状态 = %q，想要还是 %q（阈值是 %d）",
				attempt, row.Status, model.SubscriptionStatusActive, model.MaxConsecutiveChargeFailures)
		}
		if int(row.ConsecutiveFailedCount) != attempt {
			t.Fatalf("第 %d 次失败之后连续失败计数 = %d", attempt, row.ConsecutiveFailedCount)
		}
		if row.NextChargeAt == nil || !row.NextChargeAt.Equal(subscription.NextChargeAt) {
			t.Fatalf("第 %d 次失败之后下一期扣款时间被改了：%v（这一期还没成，该拿同一个期次接着试）",
				attempt, row.NextChargeAt)
		}
		if row.SuspendedAt != nil {
			t.Fatalf("第 %d 次失败就把订阅停了", attempt)
		}
		// 钱没动，权益一个字都不该改。
		if now := f.membership(); !now.ExpireAt.UTC().Equal(before.ExpireAt.UTC()) {
			t.Fatalf("第 %d 次失败动到了会员到期日：%v → %v", attempt, before.ExpireAt.UTC(), now.ExpireAt.UTC())
		}
	}

	// 第 3 次：到阈值。
	f.mustChargeEvent(failure)

	suspended := f.subscription(subscription.ID)
	if suspended.Status != model.SubscriptionStatusSuspended {
		t.Fatalf("连续第 %d 次失败之后订阅状态 = %q，想要 %q",
			model.MaxConsecutiveChargeFailures, suspended.Status, model.SubscriptionStatusSuspended)
	}
	if suspended.SuspendedAt == nil {
		t.Error("停扣没记下时刻（库上 suspended 时它是 NOT NULL 的 CHECK）")
	}
	if int(suspended.ConsecutiveFailedCount) != model.MaxConsecutiveChargeFailures ||
		int(suspended.FailedCount) != model.MaxConsecutiveChargeFailures {
		t.Errorf("失败计数 = 累计 %d / 连续 %d，想要都是 %d",
			suspended.FailedCount, suspended.ConsecutiveFailedCount, model.MaxConsecutiveChargeFailures)
	}
	if now := f.membership(); !now.ExpireAt.UTC().Equal(before.ExpireAt.UTC()) {
		t.Errorf("停扣动到了会员到期日：%v → %v", before.ExpireAt.UTC(), now.ExpireAt.UTC())
	}

	// 流水：每一次失败各一条（**request_id 故意留空**，见 repository.SettleCharge 那段：拿期次当
	// 幂等键会让第二、三条撞上唯一索引、被静默丢掉，计数永远到不了 3），外加一条停扣。
	changes := f.changes()
	if got := countOfType(changes, model.ChangeChargeFailed); got != model.MaxConsecutiveChargeFailures {
		t.Fatalf("失败流水 %d 条，想要 %d 条：%s", got, model.MaxConsecutiveChargeFailures, changeTypes(changes))
	}
	if got := countOfType(changes, model.ChangeSuspend); got != 1 {
		t.Fatalf("停扣流水 %d 条，想要 1 条——运营在时间线上要能看出「从这一刻起不再扣了」：%s",
			got, changeTypes(changes))
	}

	// 停扣的全部效果就是这一行不再被扫描捞到。
	sentBefore := len(chargesFor(gateway, subscription.ContractCode))
	if _, err := f.svc.ChargeDue(ctx, 200, uuid.NewString()); err != nil {
		t.Fatalf("到期扫描失败：%v", err)
	}
	if got := len(chargesFor(gateway, subscription.ContractCode)); got != sentBefore {
		t.Fatalf("停扣之后又发起了 %d 次", got-sentBefore)
	}
}

// ============================================================
// 那三条事件本身
// ============================================================

// TestIntegrationChargeEventRefusesWhatItCannotTrust 是那三条事件的三道闸。
//
// 每一条挡的都是一种钱上的错，所以分开断言：把失败读成成功是**凭一条没扣到钱的事件白送一个月**、
// 定位不到订阅时让整条队列堵住是不划算、而归属对不上是「两边记的签约人不是同一个」。
func TestIntegrationChargeEventRefusesWhatItCannotTrust(t *testing.T) {
	t.Run("事件名与载荷里的状态对不上", func(t *testing.T) {
		f := newMembershipFixture(t)
		plan := f.seedMembership(t)
		f.chargeGateway()
		subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))
		before := f.membership()

		// 事件名说成功，载荷说失败。**不 ack**：认错了的后果是给一个没付钱的人续一期。
		err := f.chargeEvent(chargeEventInput{
			EventType:             dto.EventAgreementChargeSucceeded,
			AgreementID:           subscription.AgreementID,
			AgreementNo:           subscription.ContractCode,
			BizPeriod:             subscription.bizPeriod(),
			Amount:                plan.PriceCents,
			ProviderTransactionID: "WXTXN-" + uuid.NewString()[:12],
			Status:                dto.ChargeStatusFailed,
		})
		if !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("错误 = %v，想要 ErrInvalidEvent（进死信，不能 ack）", err)
		}
		if now := f.membership(); !now.ExpireAt.UTC().Equal(before.ExpireAt.UTC()) {
			t.Errorf("对不上的一条事件还是把会员续了：%v → %v", before.ExpireAt.UTC(), now.ExpireAt.UTC())
		}
		if got := countOfType(f.changes(), model.ChangeChargeFailed); got != 0 {
			t.Errorf("对不上的一条事件却被当成失败记了 %d 条流水", got)
		}
	})

	t.Run("没有期次", func(t *testing.T) {
		f := newMembershipFixture(t)
		plan := f.seedMembership(t)
		f.chargeGateway()
		subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))

		// 期次只进流水，但它同时是对账时手里拿的那个号；缺了它这条流水答不出「续的是哪一期」，
		// 而这不是重投能补上的。
		err := f.chargeEvent(chargeEventInput{
			AgreementID:           subscription.AgreementID,
			AgreementNo:           subscription.ContractCode,
			Amount:                plan.PriceCents,
			ProviderTransactionID: "WXTXN-" + uuid.NewString()[:12],
			Status:                dto.ChargeStatusSucceeded,
		})
		if !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("错误 = %v，想要 ErrInvalidEvent", err)
		}
	})

	t.Run("协议名下一份订阅都没有", func(t *testing.T) {
		f := newMembershipFixture(t)
		f.chargeGateway()

		// **ack 而不是报错**：协议是支付域的通用能力，别的域将来也可能签，为一条不是本域的事件让
		// 整条队列堵在重投上不划算。
		err := f.chargeEvent(chargeEventInput{
			AgreementID:           uuid.NewString(),
			AgreementNo:           "AGR-nobody",
			BizPeriod:             "20261001",
			Amount:                990,
			ProviderTransactionID: "WXTXN-nobody",
			Status:                dto.ChargeStatusSucceeded,
		})
		if err != nil {
			t.Fatalf("没有对应订阅的扣款事件报错了：%v（它该被 ack 掉）", err)
		}
	})

	t.Run("签约人与订阅上那个人对不上", func(t *testing.T) {
		f := newMembershipFixture(t)
		plan := f.seedMembership(t)
		f.chargeGateway()
		subscription := f.dueSubscription(plan, f.clock.Add(-time.Minute))
		before := f.membership()

		err := f.chargeEvent(chargeEventInput{
			AgreementID:           subscription.AgreementID,
			AgreementNo:           subscription.ContractCode,
			BizPeriod:             subscription.bizPeriod(),
			Amount:                plan.PriceCents,
			ProviderTransactionID: "WXTXN-" + uuid.NewString()[:12],
			Status:                dto.ChargeStatusSucceeded,
			// 支付域记的签约人是另一个人。
			UserID: uuid.NewString(),
		})
		if !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("错误 = %v，想要 ErrInvalidEvent（进死信让人看）", err)
		}
		// 但**续期已经做完了**：定位靠协议 id、不看 userId，所以改的是对的那一行。报错不是为了
		// 重做（重投还是同一条），是为了让这次数据对不上停在有人看得见的地方——钱已经到了，权益
		// 必须跟上。
		if now := f.membership(); !now.ExpireAt.After(before.ExpireAt) {
			t.Error("归属对不上时连续期都没做：钱已经到了，权益却被留在了原地")
		}
	})
}
