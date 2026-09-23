package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
)

// 代扣那一行的仓储，**只有真库能验的那几条**：库级幂等键、状态机的 WHERE 子句、两条唯一索引。
//
// 服务层的用例（service/agreement_charge_test.go）用的是假仓储，它验的是「七种已有状态各自碰不
// 碰渠道」那张分流表——那是编排的判据。而这一份验的是分流**底下**那几件事：同一期是不是真的只
// 有一行、重试是不是真的复用同一个商户单号、一条结果通知是不是真的只结算一次、已结算的结果会
// 不会被另一条通知翻过来。写错任何一条，编译期与服务层的假仓储都不会红——线上多扣用户一期钱。
//
// 与其它集成用例同一个约定：只用一个调用方给的库 URL，不建库、不删库、不跑迁移，夹具一律用
// 随机 uuid。**payment_state_transitions 与 message_outbox 跑完不清理**——前者有 append-only
// 触发器（DELETE 直接抛），后者的行按聚合隔离；两者每次都是新的聚合 id，不会让断言串台。

// chargeFixture 是一份「活着、可扣款」的协议，外加为它开一期扣款的能力。
type chargeFixture struct {
	t    *testing.T
	repo *PostgresRepository
	pool *pgxpool.Pool

	AgreementID string
	AgreementNo string
	UserID      string
	// Provider 取**真实的**渠道码（catalog 里那条渠道的 Provider，见 wechatpay.Name）：通知
	// 那条路上要用它与协议上的渠道对一遍（见 notificationChannelMatches），编一个不存在的值
	// 会让「渠道对得上」那条判据验的是一段现实里不会出现的形状。
	Provider string
}

// newChargeFixture 造一份 active 的协议。
//
// 形状照着 repository.CreateAgreement 那一步写进去的那一行来（见 agreement.go:97）——列不同的话
// 验的就不是真实的行。这里**不走那个函数**：它要一枚幂等键、要写 outbox 与调用流水，而这一份
// 用例要的是「协议已经建好、可以扣款了」这个起点。
func newChargeFixture(t *testing.T) *chargeFixture {
	t.Helper()
	pool := paymentIntegrationPool(t)

	fixture := &chargeFixture{
		t:           t,
		repo:        NewPostgresRepository(pool, nil),
		pool:        pool,
		AgreementID: uuid.NewString(),
		AgreementNo: "AGR-CHG-" + uuid.NewString(),
		UserID:      uuid.NewString(),
		Provider:    catalog.ChannelCodeWeChatPay,
	}
	_, err := pool.Exec(context.Background(), `INSERT INTO payment_agreements
		(id, agreement_no, user_id, provider, payment_method, contract_no, subject, plan_code,
		 max_charge_amount, status)
		VALUES ($1,$2,$3,$4,'',$5,'会员自动续费','PLAN-MONTHLY',0,'active')`,
		fixture.AgreementID, fixture.AgreementNo, fixture.UserID, fixture.Provider,
		"CTR-"+uuid.NewString())
	if err != nil {
		t.Fatalf("插一份协议失败：%v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 扣款行先删（外键 RESTRICT），再删协议。状态流水与 outbox 留着。
		_, _ = pool.Exec(ctx, `DELETE FROM payment_agreement_charges WHERE agreement_id=$1`, fixture.AgreementID)
		_, _ = pool.Exec(ctx, `DELETE FROM payment_agreements WHERE id=$1`, fixture.AgreementID)
	})
	return fixture
}

// createCharge 取或建一期扣款。
func (f *chargeFixture) createCharge(period, outTradeNo string) (*model.PaymentAgreementCharge, bool) {
	f.t.Helper()
	charge, created, err := f.repo.CreateOrFindCharge(context.Background(), CreateOrFindChargeParams{
		AgreementID: f.AgreementID,
		AgreementNo: f.AgreementNo,
		BizPeriod:   period,
		Amount:      990,
		OutTradeNo:  outTradeNo,
		RequestID:   uuid.NewString(),
	})
	if err != nil {
		f.t.Fatalf("取或建 %s 这一期失败：%v", period, err)
	}
	return charge, created
}

// notification 落一条通知记录（扣款的回调会先把它插进来，再把 id 交给结算那一步）。
func (f *chargeFixture) notification() string {
	f.t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(), `INSERT INTO payment_notifications
		(provider, notification_id, event_type, body, body_sha256)
		VALUES ($1,$2,'agreement_charge',$3,$4) RETURNING id`,
		f.Provider, "N-"+uuid.NewString(), []byte("<xml/>"), uuid.NewString()).Scan(&id)
	if err != nil {
		f.t.Fatalf("插一条通知记录失败：%v", err)
	}
	return id
}

// notificationStatus 读回一条通知的状态。它是「被拒的那些通知有没有被标成 processed」的唯一判据
// ——结算那一步是把「改业务行」与「标通知」放在同一个事务里的。
func (f *chargeFixture) notificationStatus(id string) string {
	f.t.Helper()
	var status string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM payment_notifications WHERE id=$1`, id).Scan(&status); err != nil {
		f.t.Fatalf("读回通知状态失败：%v", err)
	}
	return status
}

// chargeEvents 读回这一类事件里**属于这一份协议**的那些。
//
// dev 库是共享的，message_outbox 里还有别人的行，所以按载荷里的 agreementId 过滤，不按条数断言
// ——与其它集成用例同一条规矩。
func (f *chargeFixture) chargeEvents(eventType string) []dto.AgreementChargeEventPayload {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT convert_from(payload,'UTF8')
		FROM message_outbox
		WHERE event_type = $1 AND convert_from(payload,'UTF8') LIKE '%' || $2 || '%'`,
		eventType, f.AgreementID)
	if err != nil {
		f.t.Fatalf("读 outbox 失败：%v", err)
	}
	defer rows.Close()

	var matched []dto.AgreementChargeEventPayload
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatalf("扫 outbox 失败：%v", err)
		}
		var event dto.AgreementChargeEventPayload
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			f.t.Fatalf("事件体解不开（event_type=%s）：%v", eventType, err)
		}
		matched = append(matched, event)
	}
	return matched
}

// ============================================================
// 一期只有一行
// ============================================================

// TestIntegrationCreateOrFindChargeIsOneRowPerPeriod 钉住 `UNIQUE (agreement_id, biz_period)`
// 真的在挡第二次，以及**重试时现算的那个商户单号会被丢掉**。
//
// 这是这一整张表最要紧的一条：两个 worker 副本同时扫到同一份到期订阅时，进程内的锁帮不上忙
// （老系统正是栽在这里），能挡住第二笔扣款的只有这条唯一键。而单号那一条更细——新开一行会带一个
// 新单号，渠道那边就是**第二笔订单**，两笔单号不同、渠道无法去重，用户被扣两次（见 payment_agreement_charges.out_trade_no 的列注释）。
func TestIntegrationCreateOrFindChargeIsOneRowPerPeriod(t *testing.T) {
	f := newChargeFixture(t)

	const period = "20261001"
	first, created := f.createCharge(period, "CHG-"+uuid.NewString())
	if !created {
		t.Fatal("第一次取或建应当是新建成的一行")
	}
	if first.Status != model.ChargeStatusPending || first.AttemptCount != 0 {
		t.Errorf("新行应当是 pending 且没试过：status=%q attempt_count=%d", first.Status, first.AttemptCount)
	}
	if first.ChargedAt != nil || first.NextRetryAt != nil {
		t.Errorf("新行不该有扣成时间或退避时间：charged_at=%v next_retry_at=%v", first.ChargedAt, first.NextRetryAt)
	}

	// 同一个期次再来一次，带着**另一个**现算的单号：回来的必须是同一行，且单号还是原来那个。
	second, created := f.createCharge(period, "CHG-"+uuid.NewString())
	if created {
		t.Error("同一期第二次取或建却说是新建的——两个 worker 会各自发起一笔扣款")
	}
	if second.ID != first.ID {
		t.Fatalf("同一期取回了两行：%s / %s", first.ID, second.ID)
	}
	if second.OutTradeNo != first.OutTradeNo {
		t.Errorf("重试换了商户单号：%q → %q（等于在渠道那边开了第二笔订单，用户会被扣两次）",
			first.OutTradeNo, second.OutTradeNo)
	}

	// 换个期次是**另一行**，它有自己的单号。只验「同一期不会重复建」的话，把 biz_period 写死在
	// 语句里（或者漏掉这一列）也能过——而那样一来下个月就再也扣不动了。
	next, created := f.createCharge("20261101", "CHG-"+uuid.NewString())
	if !created || next.ID == first.ID {
		t.Errorf("换一个期次应当新建成另一行（created=%v id=%s/%s）", created, next.ID, first.ID)
	}
	if next.OutTradeNo == first.OutTradeNo {
		t.Errorf("两期共用了同一个商户单号：%q", next.OutTradeNo)
	}

	// 库里就是两行。
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM payment_agreement_charges WHERE agreement_id=$1`, f.AgreementID).Scan(&count); err != nil {
		t.Fatalf("数行数失败：%v", err)
	}
	if count != 2 {
		t.Errorf("这一份协议下有 %d 行扣款，想要 2 行（两个期次）", count)
	}
}

// ============================================================
// 记一次尝试
// ============================================================

// TestIntegrationMarkChargeAttempt 钉住「一次尝试」落在那一行上的样子：计数在锁内自增、受理不发
// 事件、失败才发、退避按已试次数算、空流水号不覆盖已有的号。
//
// 「受理不发事件」值得单独说：事件是给消费方推进续费的凭据，收到一条「受理了」就去续一个月，
// 正是老系统那个 bug（拿到 transaction_id 就记账）的升级版。
func TestIntegrationMarkChargeAttempt(t *testing.T) {
	f := newChargeFixture(t)
	charge, _ := f.createCharge("20261001", "CHG-"+uuid.NewString())

	// 第一次尝试：渠道受理了。
	accepted, err := f.repo.MarkChargeAttempt(context.Background(), ChargeAttemptParams{
		ChargeID:              charge.ID,
		Status:                model.ChargeStatusCharging,
		ProviderTransactionID: "WXTXN-ACCEPTED",
		RequestID:             uuid.NewString(),
		TraceID:               uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("记一次受理失败：%v", err)
	}
	if accepted.AttemptCount != 1 || accepted.Status != model.ChargeStatusCharging {
		t.Errorf("受理之后：attempt_count=%d status=%q，想要 1 / charging", accepted.AttemptCount, accepted.Status)
	}
	if accepted.ProviderTransactionID != "WXTXN-ACCEPTED" {
		t.Errorf("渠道流水号没记下来：%q", accepted.ProviderTransactionID)
	}
	if events := f.chargeEvents(dto.EventAgreementChargeSucceeded); len(events) != 0 {
		t.Errorf("受理就发了成功事件：%+v（受理不等于扣到钱）", events)
	}
	if events := f.chargeEvents(dto.EventAgreementChargeFailed); len(events) != 0 {
		t.Errorf("受理却发了失败事件：%+v", events)
	}

	// 第二次尝试：渠道当场拒了，且**没给**流水号——已有的那个号要留着（一个已受理的尝试后来又
	// 失败了，那个号仍然是人工去渠道查这一笔时唯一的线索）。
	nextRetryAt := model.ChargeNextRetryAt(time.Now(), 1)
	rejected, err := f.repo.MarkChargeAttempt(context.Background(), ChargeAttemptParams{
		ChargeID:       charge.ID,
		Status:         model.ChargeStatusFailed,
		FailureCode:    "BALANCE_NOT_ENOUGH",
		FailureMessage: "余额不足",
		NextRetryAt:    nextRetryAt,
		RequestID:      uuid.NewString(),
		TraceID:        uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("记一次失败失败：%v", err)
	}
	if rejected.AttemptCount != 2 {
		t.Errorf("attempt_count = %d，想要 2（封顶判据就是拿它算的）", rejected.AttemptCount)
	}
	if rejected.ProviderTransactionID != "WXTXN-ACCEPTED" {
		t.Errorf("空流水号把已有的那个冲掉了：%q", rejected.ProviderTransactionID)
	}
	if rejected.FailureCode != "BALANCE_NOT_ENOUGH" || rejected.FailureMessage != "余额不足" {
		t.Errorf("失败原因没落库：%q / %q", rejected.FailureCode, rejected.FailureMessage)
	}
	if rejected.NextRetryAt == nil {
		t.Error("退避时间没落库——这一期会立刻被下一轮扫描再试一次")
	}

	failed := f.chargeEvents(dto.EventAgreementChargeFailed)
	if len(failed) != 1 {
		t.Fatalf("失败事件 = %d 条，想要 1 条", len(failed))
	}
	if failed[0].BizPeriod != charge.BizPeriod || failed[0].Status != model.ChargeStatusFailed {
		t.Errorf("事件体说的不是这一期这一次：%+v", failed[0])
	}
	if failed[0].UserID != f.UserID || failed[0].AgreementID != f.AgreementID {
		t.Errorf("事件体定位不到签约人与协议：%+v", failed[0])
	}
}

// ============================================================
// 一条结果通知
// ============================================================

// settleParams 是一条「本期的金额、本协议、这条通知」的正常入参，各条用例在它上面改一处来造反例。
func (f *chargeFixture) settleParams(charge *model.PaymentAgreementCharge, target string) ChargeNotificationParams {
	f.t.Helper()
	return ChargeNotificationParams{
		NotificationID:        f.notification(),
		Provider:              f.Provider,
		OutTradeNo:            charge.OutTradeNo,
		ProviderTransactionID: "WXTXN-" + uuid.NewString(),
		Amount:                charge.Amount,
		Target:                target,
		Reason:                "charge result notification",
		TraceID:               uuid.NewString(),
	}
}

// TestIntegrationSettleChargeNotificationSucceeds 钉住成功那条：只结算一次、通知被标 processed、
// 账号流水与渠道流水都落上、发且只发一条成功事件。
//
// 「只结算一次」是**第二道**防线：第一道是 UNIQUE (provider, notification_id)，但它只挡得住一模
// 一样的报文；渠道对同一次扣款推两份内容略有差别（时间戳不同）的报文时，两条都会落库，而在这里
// 被「只有 pending/charging 能推进」挡住第二次。
func TestIntegrationSettleChargeNotificationSucceeds(t *testing.T) {
	f := newChargeFixture(t)
	charge, _ := f.createCharge("20261001", "CHG-"+uuid.NewString())

	// 先走一次「渠道受理了」——真实链路里这一行到通知时就是 charging（受理 ≠ 扣到钱）。
	if _, err := f.repo.MarkChargeAttempt(context.Background(), ChargeAttemptParams{
		ChargeID:              charge.ID,
		Status:                model.ChargeStatusCharging,
		ProviderTransactionID: "WXTXN-PENDING",
		RequestID:             uuid.NewString(),
	}); err != nil {
		t.Fatalf("记一次受理失败：%v", err)
	}

	params := f.settleParams(charge, model.ChargeStatusSucceeded)
	settlement, err := f.repo.SettleChargeNotification(context.Background(), params)
	if err != nil {
		t.Fatalf("结算失败：%v", err)
	}
	if !settlement.Changed {
		t.Fatal("Changed = false：这一期没有被推进")
	}
	settled := settlement.Charge
	if settled.Status != model.ChargeStatusSucceeded || settled.ChargedAt == nil {
		t.Errorf("结算之后：status=%q charged_at=%v", settled.Status, settled.ChargedAt)
	}
	if settled.ProviderTransactionID != params.ProviderTransactionID {
		t.Errorf("渠道流水号 = %q，想要通知里那个 %q", settled.ProviderTransactionID, params.ProviderTransactionID)
	}
	if settled.AttemptCount != 1 {
		t.Errorf("attempt_count = %d，想要 1（一条结果通知不是一次新的尝试）", settled.AttemptCount)
	}
	if settled.PaymentNo != "" {
		t.Errorf("代扣不建 payments 行，payment_no 却写成了 %q", settled.PaymentNo)
	}
	if status := f.notificationStatus(params.NotificationID); status != model.NotificationProcessed {
		t.Errorf("通知状态 = %q，想要 processed", status)
	}

	events := f.chargeEvents(dto.EventAgreementChargeSucceeded)
	if len(events) != 1 {
		t.Fatalf("成功事件 = %d 条，想要 1 条", len(events))
	}
	if events[0].BizPeriod != charge.BizPeriod || events[0].ProviderTransactionID != params.ProviderTransactionID {
		t.Errorf("事件体说的不是这一期这一笔：%+v", events[0])
	}

	// 渠道把**同一笔**又推了一遍（报文略有差别 → 新的一条通知记录，同一个商户单号）。
	replay := f.settleParams(charge, model.ChargeStatusSucceeded)
	again, err := f.repo.SettleChargeNotification(context.Background(), replay)
	if err != nil {
		t.Fatalf("重投结算失败：%v", err)
	}
	if again.Changed {
		t.Error("重投又推进了一次——这一期会被算成两笔（membership 那边就是多送一个月）")
	}
	if again.Charge.Status != model.ChargeStatusSucceeded {
		t.Errorf("重投把状态改成了 %q", again.Charge.Status)
	}
	if status := f.notificationStatus(replay.NotificationID); status != model.NotificationProcessed {
		t.Errorf("重投的那条通知状态 = %q，想要 processed（安静地收下，不重推）", status)
	}
	if events := f.chargeEvents(dto.EventAgreementChargeSucceeded); len(events) != 1 {
		t.Errorf("重投之后成功事件 = %d 条，想要仍然 1 条", len(events))
	}

	// 已结算的一期**不许被另一条通知翻过来**——哪怕那条通知说这次扣款失败了。这是本表最不能
	// 发生的事：钱已经记进来了，翻成 failed 会让 membership 那边把已续的期当成没续。
	flip := f.settleParams(charge, model.ChargeStatusFailed)
	flip.FailureCode = "BALANCE_NOT_ENOUGH"
	flipped, err := f.repo.SettleChargeNotification(context.Background(), flip)
	if err != nil {
		t.Fatalf("投一条相反的通知失败：%v", err)
	}
	if flipped.Changed || flipped.Charge.Status != model.ChargeStatusSucceeded {
		t.Errorf("已结算的一期被翻了：changed=%v status=%q", flipped.Changed, flipped.Charge.Status)
	}
	if events := f.chargeEvents(dto.EventAgreementChargeFailed); len(events) != 0 {
		t.Errorf("已成功的一期发了失败事件：%+v", events)
	}
}

// TestIntegrationSettleChargeNotificationRefusesWhatItCannotTrust 钉住入账前的三道闸：找不到
// 那一期、渠道对不上、金额对不上。三条都必须**一个字节都没改**，且通知不能被标成 processed。
//
// 金额那一条是老系统完全没有的闸：它把通知里的钱数直接当成这一期的账。金额对不上的含义是「这两
// 条报文说的不是同一笔」（渠道串了单，或有人拿着一条别的单的通知打到了这个回调上），两种都不该
// 把这一期结掉。
func TestIntegrationSettleChargeNotificationRefusesWhatItCannotTrust(t *testing.T) {
	f := newChargeFixture(t)
	ctx := context.Background()
	charge, _ := f.createCharge("20261001", "CHG-"+uuid.NewString())

	// 1. 商户单号不认识：报文里没有别的字段能定位到一行，只能拒（老系统收到的续费通知里带的
	//    正是另一个号，而那条判据缺失的后果是永久重推）。
	unknown := f.settleParams(charge, model.ChargeStatusSucceeded)
	unknown.OutTradeNo = "CHG-UNKNOWN-" + uuid.NewString()
	if _, err := f.repo.SettleChargeNotification(ctx, unknown); !errors.Is(err, ErrChargeNotFound) {
		t.Errorf("认不出的商户单号回的是 %v，想要 ErrChargeNotFound", err)
	}
	if status := f.notificationStatus(unknown.NotificationID); status != model.NotificationReceived {
		t.Errorf("被拒的通知被标成了 %q，想要仍然是 received（留给人工来看）", status)
	}

	// 2. 渠道对不上：这一份协议的扣款只可能来自它签约的那条渠道。
	wrongChannel := f.settleParams(charge, model.ChargeStatusSucceeded)
	wrongChannel.Provider = "unionpay"
	if _, err := f.repo.SettleChargeNotification(ctx, wrongChannel); !errors.Is(err, ErrPaymentChannelMismatch) {
		t.Errorf("渠道对不上时回的是 %v，想要 ErrPaymentChannelMismatch", err)
	}

	// 3. 金额对不上：不是「以通知为准」，是拒绝入账。
	wrongAmount := f.settleParams(charge, model.ChargeStatusSucceeded)
	wrongAmount.Amount = charge.Amount + 1
	if _, err := f.repo.SettleChargeNotification(ctx, wrongAmount); !errors.Is(err, ErrChargeAmountMismatch) {
		t.Errorf("金额对不上时回的是 %v，想要 ErrChargeAmountMismatch", err)
	}

	// 三道闸都没让这一期往前走。
	after, err := f.repo.FindChargeByTradeNo(ctx, charge.OutTradeNo)
	if err != nil {
		t.Fatalf("读回这一期失败：%v", err)
	}
	if after.Status != model.ChargeStatusPending || after.ChargedAt != nil || after.AttemptCount != 0 {
		t.Errorf("被拒的通知改了业务行：status=%q charged_at=%v attempt_count=%d",
			after.Status, after.ChargedAt, after.AttemptCount)
	}
	if events := f.chargeEvents(dto.EventAgreementChargeSucceeded); len(events) != 0 {
		t.Errorf("被拒的通知发了成功事件：%+v", events)
	}
}

// TestIntegrationChargeProviderTransactionIsUniquePerRow 钉住 payment_agreement_charges_transaction_unique：**同一笔渠道
// 流水不许挂在两期上**。
//
// 它一旦发生，含义非常具体：同一笔钱被记成了两期。比漏记严重得多——漏记只是没扣，重复记是账面
// 上多收了用户一期的钱。仓储**不翻**这个错（见 mapChargeError）：那是要人来看的事故，原样报出去
// 比翻成一句业务错误更能让人停下。
func TestIntegrationChargeProviderTransactionIsUniquePerRow(t *testing.T) {
	f := newChargeFixture(t)
	ctx := context.Background()

	first, _ := f.createCharge("20261001", "CHG-"+uuid.NewString())
	second, _ := f.createCharge("20261101", "CHG-"+uuid.NewString())

	const transactionID = "WXTXN-SAME-FOR-BOTH"
	firstParams := f.settleParams(first, model.ChargeStatusSucceeded)
	firstParams.ProviderTransactionID = transactionID
	if _, err := f.repo.SettleChargeNotification(ctx, firstParams); err != nil {
		t.Fatalf("结算第一期失败：%v", err)
	}

	secondParams := f.settleParams(second, model.ChargeStatusSucceeded)
	secondParams.ProviderTransactionID = transactionID
	_, err := f.repo.SettleChargeNotification(ctx, secondParams)
	if err == nil {
		t.Fatal("同一笔渠道流水被两期同时记下了——账面上这一笔钱被算成了两期")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("回的是 %v，想要一条唯一约束冲突（23505）", err)
	}
	if status := f.notificationStatus(secondParams.NotificationID); status != model.NotificationReceived {
		t.Errorf("第二期的通知被标成了 %q——那一行其实一个字节都没改", status)
	}
}

// ============================================================
// 按协议读回全部期次
// ============================================================

// TestIntegrationListChargesByAgreementNo 钉住后台「续费明细」那一块读的两件事：**只回这一份
// 协议的**期次，且**按期次升序**。
//
// 为什么这两条值得用真库验：两者都是 SQL 里的子句，写错了在服务层完全看不出来——假仓储只是把
// 调用原样转发，单测里那个切片是测试自己排好的。
//
//   - 漏掉 WHERE 就是**串台**：后台打开一份订阅，看到的是库里所有人的扣款记录，而这里面的金额、
//     失败原因、渠道流水号都是别人的。这是这一页唯一可能泄露数据的写法。
//   - 漏掉 ORDER BY（或者按 created_at 排）在今天看不出区别，因为建行顺序通常就是期次顺序；
//     而补扣、跨月补录、人工插一期都会让它显出原形——那一列在界面上的名字是「扣款时间」，顺序错了
//     就是一张自相矛盾的账。
//
// 夹具刻意**不按时间顺序建**（先 11 月、再 10 月、再 12 月），这样「按建行顺序返回」的实现会红。
func TestIntegrationListChargesByAgreementNo(t *testing.T) {
	f := newChargeFixture(t)
	ctx := context.Background()

	// 11 月：渠道受理了，还在路上（charging）。
	inFlight, _ := f.createCharge("20261101", "CHG-"+uuid.NewString())
	if _, err := f.repo.MarkChargeAttempt(ctx, ChargeAttemptParams{
		ChargeID:              inFlight.ID,
		Status:                model.ChargeStatusCharging,
		ProviderTransactionID: "WXTXN-IN-FLIGHT",
		RequestID:             uuid.NewString(),
		TraceID:               uuid.NewString(),
	}); err != nil {
		t.Fatalf("记一次受理失败：%v", err)
	}

	// 10 月：扣成了，有渠道流水号。
	succeeded, _ := f.createCharge("20261001", "CHG-"+uuid.NewString())
	params := f.settleParams(succeeded, model.ChargeStatusSucceeded)
	if _, err := f.repo.SettleChargeNotification(ctx, params); err != nil {
		t.Fatalf("结算 10 月那一期失败：%v", err)
	}

	// 12 月：被拒了，带着失败原因与一个排定的重试。
	failed, _ := f.createCharge("20261201", "CHG-"+uuid.NewString())
	if _, err := f.repo.MarkChargeAttempt(ctx, ChargeAttemptParams{
		ChargeID:       failed.ID,
		Status:         model.ChargeStatusFailed,
		FailureCode:    "BALANCE_NOT_ENOUGH",
		FailureMessage: "余额不足",
		NextRetryAt:    model.ChargeNextRetryAt(time.Now(), 1),
		RequestID:      uuid.NewString(),
		TraceID:        uuid.NewString(),
	}); err != nil {
		t.Fatalf("记一次失败失败：%v", err)
	}

	// 另一份协议也有一期**同一个期次**：它是这一条用例真正的判据——只按 biz_period 过滤、
	// 忘了按协议过滤的话，下面那个断言会看到它。
	other := newChargeFixture(t)
	other.createCharge("20261001", "CHG-"+uuid.NewString())

	charges, err := f.repo.ListChargesByAgreementNo(ctx, f.AgreementNo)
	if err != nil {
		t.Fatalf("按期次读回失败：%v", err)
	}
	if len(charges) != 3 {
		t.Fatalf("读回 %d 期，想要 3 期（多出来的多半是别的协议的）：%+v", len(charges), charges)
	}

	wantPeriods := []string{"20261001", "20261101", "20261201"}
	for i, want := range wantPeriods {
		if charges[i].BizPeriod != want {
			t.Errorf("第 %d 期是 %q，想要 %q——期次没有升序，或者串到了别的协议",
				i, charges[i].BizPeriod, want)
		}
		if charges[i].AgreementNo != f.AgreementNo {
			t.Errorf("第 %d 期属于协议 %q，不是 %q", i, charges[i].AgreementNo, f.AgreementNo)
		}
	}

	// 三期的状态与各自的痕迹都真的读出来了。只比期次的话，把 SELECT 里的状态列写死也能过。
	if got := charges[0]; got.Status != model.ChargeStatusSucceeded ||
		got.ProviderTransactionID != params.ProviderTransactionID || got.ChargedAt == nil {
		t.Errorf("10 月那期读成了 %+v，想要 succeeded + 渠道流水号 %q + 扣成时间",
			got, params.ProviderTransactionID)
	}
	if got := charges[1]; got.Status != model.ChargeStatusCharging ||
		got.ProviderTransactionID != "WXTXN-IN-FLIGHT" || got.ChargedAt != nil {
		t.Errorf("11 月那期读成了 %+v，想要 charging + 已受理的流水号", got)
	}
	if got := charges[2]; got.Status != model.ChargeStatusFailed ||
		got.FailureCode != "BALANCE_NOT_ENOUGH" || got.NextRetryAt == nil {
		t.Errorf("12 月那期读成了 %+v，想要 failed + 失败原因 + 退避时间", got)
	}

	// 期次升序是这台机器上读这一页的**唯一**顺序保证，读第二遍必须一样。
	again, err := f.repo.ListChargesByAgreementNo(ctx, f.AgreementNo)
	if err != nil {
		t.Fatalf("第二遍读回失败：%v", err)
	}
	if len(again) != len(charges) {
		t.Fatalf("两遍读回的期数不同：%d / %d", len(charges), len(again))
	}
	for i := range again {
		if again[i].ID != charges[i].ID {
			t.Errorf("第 %d 期的顺序两遍不一致：%s / %s", i, charges[i].ID, again[i].ID)
		}
	}

	// 一份**没有期次**的协议回空切片，不是 nil（调用方要的是 `[]`，不是 `null`）、也不是 404——
	// 那一格由服务层查协议本身来区分（见 service.ListAgreementCharges）。
	empty := newChargeFixture(t)
	none, err := f.repo.ListChargesByAgreementNo(ctx, empty.AgreementNo)
	if err != nil {
		t.Fatalf("读一份没有期次的协议失败：%v", err)
	}
	if none == nil {
		t.Error("没有期次时回的是 nil——调用方拿到的是 null，不是空数组")
	}
	if len(none) != 0 {
		t.Errorf("没有期次的协议读回了 %d 期：%+v", len(none), none)
	}
}
