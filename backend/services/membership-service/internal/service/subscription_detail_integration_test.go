package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 订阅详情抽屉那一次读：订阅本身来自本库，两块附带的回显来自支付域与订单域。
//
// # 这一份钉的是**降级**，不是那两块的内容
//
// 内容对不对是拼装的事，看一眼就知道；真正会在生产里咬人的是「支付域抖一下，运营打不开订阅
// 详情」——那一页的权限码是 membership:read，要回答的是「谁签的连续包月、下一次什么时候扣」，而
// 那在本库里。所以下面的用例一半在验「拿得到的时候拿得到」，另一半在验「拿不到的时候整页照常」。

// fakeChargeReader 是支付域那两块只读的替身。
type fakeChargeReader struct {
	charges      []dto.AgreementCharge
	chargesErr   error
	askedAbout   []string
	payment      *dto.PaymentSummary
	paymentErr   error
	paymentAsked []string
}

func (r *fakeChargeReader) ListAgreementCharges(_ context.Context, agreementNo string) ([]dto.AgreementCharge, error) {
	r.askedAbout = append(r.askedAbout, agreementNo)
	if r.chargesErr != nil {
		return nil, r.chargesErr
	}
	return r.charges, nil
}

func (r *fakeChargeReader) GetPayment(_ context.Context, paymentNo string) (*dto.PaymentSummary, error) {
	r.paymentAsked = append(r.paymentAsked, paymentNo)
	if r.paymentErr != nil {
		return nil, r.paymentErr
	}
	return r.payment, nil
}

// wireDetail 把业务层重装一遍，只接支付域那两块只读（订单域用 fixture 上那份假的，没挂就是 nil）。
//
// 不复用 wireSigning：那一个是「签约那条路要的全部依赖」，而这一份用例恰恰要的是一台**除了本库
// 与两个只读以外什么都没有**的进程——那正是后台读路径上线时的形状。
func (f *membershipFixture) wireDetail(charges ChargeReader) {
	f.t.Helper()
	f.svc = New(repository.NewPostgresRepository(f.pool, nil), Options{
		Now:     func() time.Time { return f.clock },
		Orders:  f.orders,
		Charges: charges,
	})
}

// nextCharge 是「下一期该扣」的那一刻。
//
// active 的订阅库上要求它非空（membership_subscriptions_check2），所以每一条造 active 行的用例都
// 得给一个。取一天之后而不是当下：这些用例一条都不该被到期扫描捞到，而判据是 next_charge_at。
func (f *membershipFixture) nextCharge() *time.Time {
	at := f.clock.Add(24 * time.Hour)
	return &at
}

// detail 取本用例那条订阅的详情，失败即终止。
func (f *membershipFixture) detail(id string) *dto.SubscriptionDetailResponse {
	f.t.Helper()
	detail, err := f.svc.GetSubscription(context.Background(), id)
	if err != nil {
		f.t.Fatalf("取订阅详情失败：%v", err)
	}
	return detail
}

// TestIntegrationSubscriptionDetailCarriesCharges 是「两块都拿得到」那一支。
//
// 三处对照：协议号来自**本库那一列**（不是从别处推的）、期次是按协议号去支付域要的、而会员那
// 十几个字段一个不少——嵌入而不是复制字段那一手，坏了就坏在这里。
func TestIntegrationSubscriptionDetailCarriesCharges(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)
	subscriptionID := f.mustSubscribe(plan, model.SubscriptionStatusActive, f.nextCharge(), f.clock)
	row := f.subscription(subscriptionID)

	charges := &fakeChargeReader{charges: []dto.AgreementCharge{
		{BizPeriod: "20260901", Amount: plan.PriceCents, Status: dto.ChargeStatusSucceeded,
			AttemptCount: 1, ChargedAt: "2026-09-01T02:00:00Z", ProviderTransactionID: "WXTXN-1"},
		{BizPeriod: "20261001", Amount: plan.PriceCents, Status: dto.ChargeStatusFailed,
			AttemptCount: 3, FailureCode: "NOTENOUGH", FailureMessage: "余额不足"},
		// 还没扣成的那一期也要出来：后台的「续费明细」要能看见这一期为什么一直没扣到。
		{BizPeriod: "20261101", Amount: plan.PriceCents, Status: dto.ChargeStatusCharging,
			AttemptCount: 1, NextRetryAt: "2026-11-03T02:00:00Z"},
	}}
	f.wireDetail(charges)

	detail := f.detail(subscriptionID)
	if len(charges.askedAbout) != 1 || charges.askedAbout[0] != row.ContractCode {
		t.Fatalf("拿出去的协议号 = %v，想要订阅上那一列 %q", charges.askedAbout, row.ContractCode)
	}
	if detail.ContractCode != row.ContractCode {
		t.Errorf("详情里的协议号 = %q，想要 %q（老后台抽屉里就有这一格）",
			detail.ContractCode, row.ContractCode)
	}
	if len(detail.Charges) != 3 {
		t.Fatalf("续费明细 %d 期，想要 3 期", len(detail.Charges))
	}
	if detail.Charges[1].Status != dto.ChargeStatusFailed || detail.Charges[1].FailureMessage != "余额不足" {
		t.Errorf("第二期的失败原因丢了：%+v", detail.Charges[1])
	}
	if detail.Charges[2].NextRetryAt == "" {
		t.Error("退避重试中的那一期没有下一次重试时刻：这正是运营要看的「它还在试」")
	}
	// 会员本身那几个字段照旧（嵌入那一段）。只挑两个断言：嵌入若被换成复制字段，这一条会先红。
	if detail.SubscriptionResponse == nil {
		t.Fatal("详情里没有订阅本身")
	}
	if detail.PlanName != row.PlanName || detail.Status != row.Subscription.Status {
		t.Errorf("订阅本身的信息不完整：planName=%q status=%q", detail.PlanName, detail.Status)
	}
	// first_payment_order_id 今天恒为空（小程序端未接），所以这一块是 nil，前端整块不渲染。
	if detail.FirstPayment != nil {
		t.Errorf("没有首月订单却给出了一块首月支付信息：%+v", detail.FirstPayment)
	}
}

// TestIntegrationSubscriptionDetailDegradesWhenThePaymentDomainIsDown 是这一份里最要紧的一条。
//
// 支付域读不到时：**整页照常返回**，只有那一块空着。所以断言分两半——详情本身拿得到（订阅的字
// 段一个不少、不报错），以及续费明细是一个**空数组**而不是 nil。
//
// 后半条是给前端看的：nil 序列化成 JSON 的 null，而前端那一块照着数组写渲染，null 会把它打回
// 错误分支——「暂无续费记录」与「这一页坏了」在界面上就成了两种不同的红。
func TestIntegrationSubscriptionDetailDegradesWhenThePaymentDomainIsDown(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)
	subscriptionID := f.mustSubscribe(plan, model.SubscriptionStatusActive, f.nextCharge(), f.clock)

	charges := &fakeChargeReader{chargesErr: errors.New("支付域连不上")}
	f.wireDetail(charges)

	detail := f.detail(subscriptionID)
	if detail.SubscriptionResponse == nil || detail.ID != subscriptionID {
		t.Fatalf("支付域读不到就把订阅本身也丢了：%+v", detail.SubscriptionResponse)
	}
	if detail.Charges == nil {
		t.Fatal("续费明细是 nil（序列化出去是 null）：前端会把它当成错误而不是「暂无续费记录」")
	}
	if len(detail.Charges) != 0 {
		t.Errorf("读失败却给出了 %d 期：%+v", len(detail.Charges), detail.Charges)
	}
	// 协议号仍然给出来：它在本库里，与支付域那条连接一点关系都没有。
	if detail.ContractCode != f.subscription(subscriptionID).ContractCode {
		t.Error("支付域读不到，连本库那一列的协议号都没了")
	}
}

// TestIntegrationSubscriptionDetailWithoutAChargeReader 钉住「没装支付域」这一格：空数组、不报错。
//
// 它与上一条是两件事：上一条是连上了但读失败，这一条是压根没接（比如只跑后台读路径的进程）。
// 两种在界面上都该是「暂无续费记录」，但只有后者是部署的常态。
func TestIntegrationSubscriptionDetailWithoutAChargeReader(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)
	subscriptionID := f.mustSubscribe(plan, model.SubscriptionStatusActive, f.nextCharge(), f.clock)
	f.wireDetail(nil)

	detail := f.detail(subscriptionID)
	if detail.Charges == nil || len(detail.Charges) != 0 {
		t.Errorf("没接支付域时的续费明细 = %#v，想要一个空数组", detail.Charges)
	}
}

// TestIntegrationSubscriptionDetailSkipsTheChannelWithoutAnAgreement 钉住「没有协议号就不去问」。
//
// 店铺码活动发放与后台开通那两条路给的是一段会员，不是一份代扣授权（库上 agreement_id 为空、
// contract_code 是空串）。对它去问「这份协议的扣款期次」只会换来一句「这份约不存在」，而那既不是
// 故障也不是信息——所以判据是**本库里那个值空不空**，不是「问了之后支付域怎么答」。
func TestIntegrationSubscriptionDetailSkipsTheChannelWithoutAnAgreement(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)
	subscriptionID := f.mustSubscribe(plan, model.SubscriptionStatusActive, f.nextCharge(), f.clock)

	// 把协议那两列清掉：那就是「券发放 / 后台开通」那两条路留下的形状。
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE membership_subscriptions SET agreement_id = NULL, contract_code = '' WHERE id = $1`,
		subscriptionID); err != nil {
		t.Fatalf("清协议信息失败：%v", err)
	}

	charges := &fakeChargeReader{}
	f.wireDetail(charges)

	detail := f.detail(subscriptionID)
	if len(charges.askedAbout) != 0 {
		t.Errorf("没有协议号却去问了支付域 %v：那是白跑一次跨服务往返", charges.askedAbout)
	}
	if detail.ContractCode != "" || len(detail.Charges) != 0 {
		t.Errorf("没有渠道协议的订阅给出了协议号 %q / %d 期", detail.ContractCode, len(detail.Charges))
	}
}

// TestIntegrationSubscriptionDetailCarriesTheFirstPayment 走一遍首月那一段的**两跳**：
// 订单 id → 订单域拿 payment_no → 支付域拿渠道流水。
//
// 这一格今天恒为空（first_payment_order_id 要小程序端写进来），所以这一条用例是拿手工写进去的
// 一行来验链路本身——它一旦接上小程序端就该原样工作，而不是那时候才发现两跳里断了一跳。
//
// 顺带钉住一件容易被「优化」掉的事：**金额与支付方式取订单上的**，只有渠道流水取自支付单。
// 两处都填会让这一页出现两套可能对不上的数。
func TestIntegrationSubscriptionDetailCarriesTheFirstPayment(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)
	subscriptionID := f.mustSubscribe(plan, model.SubscriptionStatusActive, f.nextCharge(), f.clock)

	f.orders = newFakeOrders()
	thirdPartyNo := "SUB-FIRST-" + uuid.NewString()[:8]
	order, err := f.orders.CreateRenewal(context.Background(), dto.RenewalOrderParams{
		ThirdPartyOrderNo: thirdPartyNo,
		UserID:            f.user,
		Amount:            plan.PriceCents,
		MembershipID:      f.membership().ID,
	})
	if err != nil {
		t.Fatalf("造首月订单失败：%v", err)
	}
	// 真实链路上这两个值由支付那一刻写进订单（payment_no 与 paid_at），这里补上。
	first := f.orders.byThirdParty[thirdPartyNo]
	first.PaymentNo = "PAY-" + uuid.NewString()[:8]
	first.PaidAt = "2026-09-01T02:00:00Z"

	// 挂到订阅的首月那一列上（小程序端将来写的就是这一列）。
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE membership_subscriptions SET order_id = $2 WHERE id = $1`, subscriptionID, order.OrderID); err != nil {
		t.Fatalf("写首月订单号失败：%v", err)
	}

	charges := &fakeChargeReader{payment: &dto.PaymentSummary{
		// 故意给一个与订单上不同的单号：下面断言的是**拿订单上那个去查的**。
		PaymentNo:             "PAY-irrelevant",
		Status:                "succeeded",
		Amount:                1, // 同样故意与订单上的金额不同：块里用的是订单那个值。
		PaymentMethod:         "some_other_method",
		ProviderTransactionID: "WXTXN-first-" + uuid.NewString()[:8],
	}}
	f.wireDetail(charges)

	detail := f.detail(subscriptionID)
	if detail.FirstPayment == nil {
		t.Fatal("订阅上有首月订单号，详情里却没有那一块")
	}
	block := detail.FirstPayment
	if block.OrderID != order.OrderID || block.OrderNo != order.OrderNo {
		t.Errorf("首月订单 = %s / %s，想要 %s / %s", block.OrderID, block.OrderNo, order.OrderID, order.OrderNo)
	}
	if block.PaidAmount != plan.PriceCents {
		t.Errorf("实付金额 = %d，想要订单上那个 %d（不是支付单上的）", block.PaidAmount, plan.PriceCents)
	}
	if block.PaymentMethod != "wechat_papay" {
		t.Errorf("支付方式 = %q，想要订单上那个 wechat_papay", block.PaymentMethod)
	}
	if len(charges.paymentAsked) != 1 || charges.paymentAsked[0] != block.PaymentNo {
		t.Fatalf("拿出去查支付的单号 = %v，想要订单上那个 %q", charges.paymentAsked, block.PaymentNo)
	}
	if block.ProviderTransactionID != charges.payment.ProviderTransactionID {
		t.Errorf("渠道流水 = %q，想要 %q——**这一格就是绕这一跳的原因**（出了争议时运营拿它去微信查）",
			block.ProviderTransactionID, charges.payment.ProviderTransactionID)
	}
	if block.PaidAt != "2026-09-01T02:00:00Z" {
		t.Errorf("支付时间 = %q", block.PaidAt)
	}
}
