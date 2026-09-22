package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// 连续包月订阅的后台读路径与「取消」。
//
// 这一组用例有一个共同的前提：**今天没有任何代码能创建一条订阅**（能创建的只有小程序端的
// 签约，而那条链路依赖微信直连）。所以这里全是直接 INSERT 造数据——它不是「绕过服务层偷懒」，
// 而是照着「将来小程序签约写进来的那一行」的形状造，这正是这些读路径要吃进去的东西。

// seedMembership 造出一条会员，返回它用的套餐。三条订阅用例都要先有一个 membership_id
// （membership_subscriptions 上有 ON DELETE RESTRICT 的外键）。
func (f *membershipFixture) seedMembership(t *testing.T) *model.Plan {
	t.Helper()
	plan := f.createPlan(autoPlanRequest())
	f.activate(plan)
	f.mustPay(uuid.NewString(), plan, f.clock)
	return plan
}

// mustSubscribe 直接插一条订阅并返回它的 id。
//
// 两个时间戳（suspended_at / cancel_at）由状态推出来，因为库上有两条 CHECK 钉着「是什么状态
// 就必须有哪个时刻」——不按状态给值的话，插 suspended 那一行会撞 23514。
//
// **协议那两个值（agreement_id / contract_code）也一起写**：真实的签约在**发起**那一刻就把它们
// 落库了（见 repository.CreateSubscription，pending_sign 的行上就有），不是等签成才写。少了它们
// 造出来的是一行现实里不存在的订阅——代扣发不出去（没有商户协议号），取消也走不到渠道那一步，
// 于是这两条路上的缺陷在这份用例里全都看不见。
func (f *membershipFixture) mustSubscribe(plan *model.Plan, status string, nextChargeAt *time.Time, createdAt time.Time) string {
	f.t.Helper()

	now := f.clock
	var suspendedAt, cancelAt *time.Time
	switch status {
	case model.SubscriptionStatusSuspended:
		suspendedAt = &now
	case model.SubscriptionStatusCancelled:
		cancelAt = &now
	}

	var id string
	err := f.pool.QueryRow(context.Background(), `INSERT INTO membership_subscriptions
		(membership_id, user_id, plan_id, status, agreement_id, contract_code, price_cents,
		 wechat_plan_id, period, period_count, next_charge_at, suspended_at, cancel_at,
		 created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
		RETURNING id`,
		f.membership().ID, f.user, plan.ID, status,
		uuid.NewString(), "AGR-"+uuid.NewString()[:8], plan.PriceCents, plan.WechatPlanID,
		plan.Period, plan.PeriodCount, nextChargeAt, suspendedAt, cancelAt, createdAt).Scan(&id)
	if err != nil {
		f.t.Fatalf("插订阅（%s）失败：%v", status, err)
	}
	return id
}

// listSubscriptions 是本用例那位用户的订阅列表，失败即终止。
func (f *membershipFixture) listSubscriptions(query dto.SubscriptionQuery) ([]*dto.SubscriptionResponse, int) {
	f.t.Helper()
	query.UserID = f.user
	query.PageSize = 20

	rows, total, err := f.svc.ListSubscriptions(context.Background(), query)
	if err != nil {
		f.t.Fatalf("订阅列表失败：%v", err)
	}
	return rows, total
}

func (f *membershipFixture) mustStats() dto.SubscriptionStats {
	f.t.Helper()
	stats, err := f.svc.SubscriptionStats(context.Background())
	if err != nil {
		f.t.Fatalf("订阅统计失败：%v", err)
	}
	return stats
}

// TestIntegrationListSubscriptions 钉住列表的筛选与排序。
func TestIntegrationListSubscriptions(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)

	nextCharge := f.clock.Add(24 * time.Hour)
	cancelledID := f.mustSubscribe(plan, model.SubscriptionStatusCancelled, nil, f.clock.Add(-2*time.Hour))
	f.mustSubscribe(plan, model.SubscriptionStatusActive, &nextCharge, f.clock.Add(-time.Hour))

	// 不带 status：不筛，但仍要排除 pending_sign（这条用例里没有那条，所以是 2）。
	rows, total := f.listSubscriptions(dto.SubscriptionQuery{})
	if total != 2 || len(rows) != 2 {
		t.Fatalf("默认列表应当有 2 条，实际 total=%d len=%d", total, len(rows))
	}
	// 排序是 created_at DESC：**刚插的 active 在前**（它比 cancelled 晚一小时）。
	if rows[0].Status != model.SubscriptionStatusActive {
		t.Errorf("排序应当是 created_at DESC，第一条却是 %s", rows[0].Status)
	}
	// 套餐名是 join 出来的，页面上「会员等级」那一列就是它。
	if rows[0].PlanName != plan.Name {
		t.Errorf("套餐名应当是 %q，实际 %q", plan.Name, rows[0].PlanName)
	}
	// 三个来源 id 都为空 → 签约场景落到默认那一档；首月支付也只有那一档才有值。
	if rows[0].SignScene != dto.SubscriptionSceneMemberCenter {
		t.Errorf("签约场景应当是 %s，实际 %s", dto.SubscriptionSceneMemberCenter, rows[0].SignScene)
	}

	// 按状态筛：只剩那一条，且 id 对得上（只看条数的话，筛错成「筛了别的状态」也看不出来）。
	rows, total = f.listSubscriptions(dto.SubscriptionQuery{Status: model.SubscriptionStatusCancelled})
	if total != 1 || len(rows) != 1 || rows[0].ID != cancelledID {
		t.Fatalf("按 cancelled 筛应当只剩 %s，实际 total=%d", cancelledID, total)
	}

	// 一个不在这五个码里的状态：**报错而不是回空列表**（见 normalizeSubscriptionQuery）。
	if _, _, err := f.svc.ListSubscriptions(context.Background(), dto.SubscriptionQuery{
		UserID: f.user, Status: "paused", PageSize: 20,
	}); !errors.Is(err, ErrSubscriptionStatusInvalid) {
		t.Errorf("pending_pay 这类不存在的状态应当回 ErrSubscriptionStatusInvalid，实际 %v", err)
	}
}

// TestIntegrationSubscriptionsHidePendingSign 钉住默认筛选那一条：**不带 status 是不等于
// pending_sign，而不是不过滤**。一屏「已下单、等签约结果」的单子对运营没有任何用处。
func TestIntegrationSubscriptionsHidePendingSign(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)

	pendingID := f.mustSubscribe(plan, model.SubscriptionStatusPendingSign, nil, f.clock)

	rows, total := f.listSubscriptions(dto.SubscriptionQuery{})
	if total != 0 || len(rows) != 0 {
		t.Fatalf("默认列表不该出现 pending_sign，实际 total=%d", total)
	}

	rows, total = f.listSubscriptions(dto.SubscriptionQuery{Status: model.SubscriptionStatusPendingSign})
	if total != 1 || rows[0].ID != pendingID {
		t.Fatalf("显式筛 pending_sign 时应当看得到它，实际 total=%d", total)
	}
}

// TestIntegrationSubscriptionStats 钉住两张统计卡的口径。
//
// 统计是**全库**的，而这是 dev 库、里面可能还有别的订阅——所以断言的是**增量**（插一条之后
// 这个数长没长），不是绝对值。绝对值断言在这张表上只会在别人插了数据之后莫名红掉。
func TestIntegrationSubscriptionStats(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)

	before := f.mustStats()

	// 一条 active 且已到点：两个数都该 +1。
	due := f.clock.Add(-time.Minute)
	nextCharge := due
	f.mustSubscribe(plan, model.SubscriptionStatusActive, &nextCharge, f.clock)

	after := f.mustStats()
	if after.ActiveCount != before.ActiveCount+1 {
		t.Errorf("有效订阅应当 +1：before=%d after=%d", before.ActiveCount, after.ActiveCount)
	}
	if after.DueCount != before.DueCount+1 {
		t.Errorf("待续费应当 +1：before=%d after=%d", before.DueCount, after.DueCount)
	}
}

// TestIntegrationSubscriptionStatsIgnoresSuspended 钉住另一头：**suspended 不算有效订阅**，
// 也不算待续费。它在等下一轮重试，混进「现在有多少人在正常续费」会让那个数虚高。
func TestIntegrationSubscriptionStatsIgnoresSuspended(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)

	before := f.mustStats()
	// suspended 的 next_charge_at 故意造到过去：它**不该**被算进待续费，而这一条正是「SQL 里
	// 那两条判据有没有写全」唯一能区分出来的地方（只判时间不判状态的话它会被数进去）。
	past := f.clock.Add(-time.Hour)
	f.mustSubscribe(plan, model.SubscriptionStatusSuspended, &past, f.clock)

	after := f.mustStats()
	if after.ActiveCount != before.ActiveCount {
		t.Errorf("suspended 不该算进有效订阅：before=%d after=%d", before.ActiveCount, after.ActiveCount)
	}
	if after.DueCount != before.DueCount {
		t.Errorf("suspended 不该算进待续费：before=%d after=%d", before.DueCount, after.DueCount)
	}
}

// TestIntegrationCancelSubscription 钉住取消。
//
// 三件事：**状态与 cancel_at 必须同时落库**（库上有 CHECK (status <> 'cancelled' OR
// cancel_at IS NOT NULL)，分两次写的话第一次必然撞上它）；**先让渠道把协议解掉，成功了才改
// 本地**（顺序反过来的表现是「用户以为关了、下个月照扣」，见 service.terminateForSubscription）；
// 以及哪几档状态能取消——active 与 suspended 可以，已经结束的不行。
func TestIntegrationCancelSubscription(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)

	nextCharge := f.clock.Add(24 * time.Hour)
	id := f.mustSubscribe(plan, model.SubscriptionStatusActive, &nextCharge, f.clock)

	agreements := f.chargeGateway()
	contractCode := f.subscription(id).ContractCode
	const cancelReason = "用户电话要求取消"

	actor := f.actor()
	got, err := f.svc.CancelSubscription(context.Background(), id,
		dto.CancelSubscriptionRequest{Reason: cancelReason}, actor)
	if err != nil {
		t.Fatalf("取消失败：%v", err)
	}
	if got.Status != model.SubscriptionStatusCancelled {
		t.Errorf("状态应当是 cancelled，实际 %s", got.Status)
	}
	if got.CancelAt == nil {
		t.Error("cancel_at 应当有值（库上那条 CHECK 就是这么要求的）")
	}
	if got.CancelledBy != actor.AdminID {
		t.Errorf("解约发起人应当是 %s，实际 %q", actor.AdminID, got.CancelledBy)
	}

	// 库里再确认一遍：上面那个是 UPDATE ... RETURNING 回出来的，与库里的行同源，但「取消
	// 之后这条订阅在库里长什么样」是这张页面的全部意义所在，值得直接看一眼。
	var status, reason string
	var cancelAt *time.Time
	var cancelledBy *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, cancel_at, cancelled_by::text, cancel_reason
		 FROM membership_subscriptions WHERE id = $1`, id).
		Scan(&status, &cancelAt, &cancelledBy, &reason); err != nil {
		t.Fatalf("读回订阅失败：%v", err)
	}
	if status != model.SubscriptionStatusCancelled || cancelAt == nil {
		t.Errorf("库里的状态是 %s、cancel_at 是 %v，两者都该有值", status, cancelAt)
	}
	if cancelledBy == nil || *cancelledBy != actor.AdminID {
		t.Errorf("库里的解约发起人不该是 %v", cancelledBy)
	}
	if reason != cancelReason {
		t.Errorf("原因没落库：%q", reason)
	}

	// 渠道那一份也解掉了，而且用的是**同一个协议号与同一句原因**：本地那句原因在后台详情里直接
	// 显示，两句不一样的话，客服看到的与微信那边记的对不上。
	if len(agreements.terminates) != 1 {
		t.Fatalf("解约调用 = %d 次，想要 1 次（不先解约就改本地，下个月照样会扣）", len(agreements.terminates))
	}
	terminated := agreements.terminates[0]
	if terminated.AgreementNo != contractCode {
		t.Errorf("交给渠道的协议号 = %q，想要订阅上那个 %q", terminated.AgreementNo, contractCode)
	}
	if terminated.Reason != cancelReason {
		t.Errorf("交给渠道的原因 = %q，想要与本地同一句 %q", terminated.Reason, cancelReason)
	}

	// 空原因：不给默认值，明说「请填写取消原因」。它排在解约之前，所以渠道一次都不该被问。
	if _, err := f.svc.CancelSubscription(context.Background(), id,
		dto.CancelSubscriptionRequest{}, f.actor()); !errors.Is(err, ErrCancelReasonRequired) {
		t.Errorf("空原因应当回 ErrCancelReasonRequired，实际 %v", err)
	}
	if len(agreements.terminates) != 1 {
		t.Errorf("原因都没填就解了一次约：%d 次", len(agreements.terminates))
	}

	// 已经是 cancelled 了：再取消一次没有任何意义，也**不该再解一次约**。
	if _, err := f.svc.CancelSubscription(context.Background(), id,
		dto.CancelSubscriptionRequest{Reason: "再来一次"}, f.actor()); !errors.Is(err, ErrSubscriptionNotCancellable) {
		t.Errorf("重复取消应当回 ErrSubscriptionNotCancellable，实际 %v", err)
	}
	if len(agreements.terminates) != 1 {
		t.Errorf("已经终态了还去解了一次约：%d 次", len(agreements.terminates))
	}

	// suspended **可以取消**：连续失败到停扣之后，这一行除了「等下一次扣款成功自己回来」没有
	// 别的可能，而想彻底不续的用户与想收尾的运营手上都需要一个按钮（见 repository.CancelSubscription）。
	// 但它同样要先解掉渠道那份协议——停扣只是「这一期先不扣了」，协议还在微信上挂着。
	suspendedID := f.mustSubscribe(plan, model.SubscriptionStatusSuspended, &nextCharge, f.clock)
	suspendedCode := f.subscription(suspendedID).ContractCode
	cancelled, err := f.svc.CancelSubscription(context.Background(), suspendedID,
		dto.CancelSubscriptionRequest{Reason: "掐掉它"}, f.actor())
	if err != nil {
		t.Fatalf("取消一条停扣的订阅失败：%v", err)
	}
	if cancelled.Status != model.SubscriptionStatusCancelled {
		t.Errorf("停扣的订阅取消后状态 = %q，想要 cancelled", cancelled.Status)
	}
	if last := agreements.terminates[len(agreements.terminates)-1]; last.AgreementNo != suspendedCode {
		t.Errorf("最后一次解的是 %q，想要停扣那条的 %q", last.AgreementNo, suspendedCode)
	}
}

// TestIntegrationGetSubscriptionNotFound 钉住「查不到」这条路：一个不存在的 id 与一个压根
// 不是 uuid 的 id 都回 ErrSubscriptionNotFound（→404），而不是 500。
func TestIntegrationGetSubscriptionNotFound(t *testing.T) {
	f := newMembershipFixture(t)

	for _, id := range []string{uuid.NewString(), "not-a-uuid", ""} {
		if _, err := f.svc.GetSubscription(context.Background(), id); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Errorf("id=%q 应当回 ErrSubscriptionNotFound，实际 %v", id, err)
		}
		if _, err := f.svc.CancelSubscription(context.Background(), id,
			dto.CancelSubscriptionRequest{Reason: "取消"}, f.actor()); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Errorf("取消时 id=%q 应当回 ErrSubscriptionNotFound，实际 %v", id, err)
		}
	}
}
