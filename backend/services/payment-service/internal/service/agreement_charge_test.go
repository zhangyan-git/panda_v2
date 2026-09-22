package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 本文件是 fakeRepository 上代扣那三个方法的实现（字段在 create_test.go 的 struct 里，
// 与别的假实现一样按路分组），以及下游的用例。
//
// # 用例守的是什么
//
//  1. **分流表**（TestChargeAgreementNeverTouchesTheChannelOnAPeriodThatHasAVerdict）：一行
//     已经有过结论的扣款，无论被问多少次都不许再碰渠道。这一条错了就是重复扣款。
//  2. **受理 ≠ 扣到钱**（TestChargeAgreementStopsAtChargingUntilTheNotification）：渠道说
//     「收下了」只推到 charging。老系统正是把这一步当成了扣款成功。
//  3. **结果不明按「可能已经扣了」处理**（TestChargeAgreementWaitsForTheNotification...）：
//     留在 charging 等通知，绝不记失败——记失败会让随后到达的那条通知被丢掉，钱扣了账上
//     却什么都没有。

// CreateOrFindCharge 与真仓储那条唯一键同形：同一期已经有行就回那一行、**并且不采纳这次
// 现算的 out_trade_no**。「重试复用同一个号」那条规矩在真库上是 ON CONFLICT DO NOTHING 的
// 结果，在这里必须逐字复现——否则一条「每次重试都换号」的实现会在假仓储上验不出来，而那正是
// 用户被扣两次的那条路（见迁移 013 的文件头）。
func (f *fakeRepository) CreateOrFindCharge(_ context.Context, p repository.CreateOrFindChargeParams) (*model.PaymentAgreementCharge, bool, error) {
	f.createChargeCalls = append(f.createChargeCalls, p)
	if f.createChargeErr != nil {
		return nil, false, f.createChargeErr
	}
	if f.charge != nil && f.charge.AgreementID == p.AgreementID && f.charge.BizPeriod == p.BizPeriod {
		return f.charge, false, nil
	}
	f.charge = &model.PaymentAgreementCharge{
		ID: "charge-1", AgreementID: p.AgreementID, AgreementNo: p.AgreementNo,
		BizPeriod: p.BizPeriod, Amount: p.Amount, OutTradeNo: p.OutTradeNo,
		Status: model.ChargeStatusPending,
	}
	return f.charge, true, nil
}

// MarkChargeAttempt 就地改那一期，与真仓储的可观察结果一致：attempt_count 加一、状态推进、
// 号与失败原因落在行上，失败时排下一次重试。判定本身（哪一次算确定失败）不在这里重写——
// 那是 service 那张分流表该管的，这一层要验的是「service 让它记了什么」。
func (f *fakeRepository) MarkChargeAttempt(_ context.Context, p repository.ChargeAttemptParams) (*model.PaymentAgreementCharge, error) {
	f.chargeAttemptCalls = append(f.chargeAttemptCalls, p)
	if f.chargeAttemptErr != nil {
		return nil, f.chargeAttemptErr
	}
	if f.charge == nil {
		return nil, repository.ErrChargeNotFound
	}
	f.charge.Status = p.Status
	f.charge.AttemptCount++
	if p.ProviderTransactionID != "" {
		f.charge.ProviderTransactionID = p.ProviderTransactionID
	}
	f.charge.FailureCode = p.FailureCode
	f.charge.FailureMessage = p.FailureMessage
	f.charge.NextRetryAt = p.NextRetryAt
	if f.charge.Status == model.ChargeStatusFailed {
		f.charge.FailureCode = p.FailureCode
	}
	return f.charge, nil
}

// SettleChargeNotification 与协议那两条通知同一个形状：记下实参（service 把报文里的结果翻成
// 了哪一个目标状态，只有在这里看得见），按开关回答「改了没有」。
func (f *fakeRepository) SettleChargeNotification(_ context.Context, p repository.ChargeNotificationParams) (*repository.ChargeSettlement, error) {
	f.chargeNotificationSettles = append(f.chargeNotificationSettles, p)
	if f.chargeNotificationErr != nil {
		return nil, f.chargeNotificationErr
	}
	if f.charge == nil {
		return nil, repository.ErrChargeNotFound
	}
	if !f.chargeNotificationChanged {
		return &repository.ChargeSettlement{Charge: f.charge}, nil
	}
	f.charge.Status = p.Target
	if p.ProviderTransactionID != "" {
		f.charge.ProviderTransactionID = p.ProviderTransactionID
	}
	f.charge.FailureCode = p.FailureCode
	f.charge.FailureMessage = p.FailureMessage
	if p.Target == model.ChargeStatusSucceeded {
		chargedAt := time.Now()
		f.charge.ChargedAt = &chargedAt
	}
	return &repository.ChargeSettlement{Charge: f.charge, Changed: true}, nil
}

// —— 用例 ——

// testBizPeriod 是这一期的期次。真值是 membership-service 从订阅的 next_charge_at 派生的
// （Asia/Shanghai 的 20060102），这里只要一个**两次调用之间算出来一样**的串。
const testBizPeriod = "20261001"

// testChargeNo 是这一期在渠道那边的商户单号。按真生成的形状写（CHG + 时间戳 + 毫秒 + 随机），
// 因为「重试绝不能换号」那条断言要看的正是这一行上的值有没有被重算掉。
const testChargeNo = "CHG202609141200000012345678"

// testChargeNow 是业务层里钉死的那个 now（见 newServiceWithCatalog 的 Options.Now）。退避算
// 出来是哪一刻只能按它推——用 time.Now() 的话断言会随着跑测试的时刻漂。
var testChargeNow = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

// activeAgreementOnFile 是一份**生效中**的协议：代扣的前置条件（协议不是 active 时，扣款
// 请求发出去必然被渠道拒，所以在入口就断）。
func activeAgreementOnFile() *model.PaymentAgreement {
	return &model.PaymentAgreement{
		ID: "agreement-1", AgreementNo: "AGR20260901120000000001",
		UserID: testUserID, Provider: "wechat_pay", PaymentMethod: "wechat_papay",
		PlanCode: "monthly_19_9", Subject: "连续包月", MaxChargeAmount: 1990,
		// 渠道侧的协议号：扣款认的是它，不是我们自己的 agreement_no。
		ContractNo: "1234567890",
		Status:     model.AgreementStatusActive,
	}
}

// chargeOnFile 是库上那一期扣款的样子。
func chargeOnFile(status string) *model.PaymentAgreementCharge {
	return &model.PaymentAgreementCharge{
		ID: "charge-1", AgreementID: "agreement-1", AgreementNo: "AGR20260901120000000001",
		BizPeriod: testBizPeriod, Amount: 1990, OutTradeNo: testChargeNo, Status: status,
	}
}

// validChargeRequest 是一次形状正确的代扣请求。
func validChargeRequest() ChargeAgreementRequest {
	return ChargeAgreementRequest{
		AgreementNo: "AGR20260901120000000001",
		BizPeriod:   testBizPeriod,
		Amount:      1990,
		Subject:     "会员自动续费",
		RequestID:   testRequest,
	}
}

// TestChargeAgreementNeverTouchesTheChannelOnAPeriodThatHasAVerdict 是这张分流表本身。
//
// 六种「已经有结论」的状态各一条，逐条断言**没碰渠道、没记尝试、没写流水，而且原样回现状**。
// 这一条错了的后果是重复扣款：succeeded 再发一次就是扣第二遍，charging 再发一次会让渠道开出
// 第二笔单（同号幂等在渠道那边成立，而我们这边会因为收到两条通知而算不清这一期到底怎样）。
//
// 「failed」拆成两条而不是一条：退避中与试满是**两个不同的判据**（next_retry_at 有没有到、
// attempt_count 有没有满），写成一条的话，改坏其中一个另一个照样是绿的。
func TestChargeAgreementNeverTouchesTheChannelOnAPeriodThatHasAVerdict(t *testing.T) {
	retryDue := testChargeNow.Add(24 * time.Hour)
	cases := []struct {
		name        string
		status      string
		attempts    int
		nextRetryAt *time.Time
	}{
		{"这一期已经扣到了", model.ChargeStatusSucceeded, 1, nil},
		{"上一次的受理还在路上", model.ChargeStatusCharging, 1, nil},
		{"协议在扣款途中解约了", model.ChargeStatusCancelled, 1, nil},
		{"业务方说过这一期不扣", model.ChargeStatusSkipped, 0, nil},
		{"失败了、还在退避中", model.ChargeStatusFailed, 1, &retryDue},
		{"失败了、已经试满", model.ChargeStatusFailed, model.ChargeMaxAttempts, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			charge := chargeOnFile(tc.status)
			charge.AttemptCount = tc.attempts
			charge.NextRetryAt = tc.nextRetryAt
			repo := &fakeRepository{agreement: activeAgreementOnFile(), charge: charge}
			keeper := &stubAgreementKeeper{}
			svc := agreementKeeperService(t, repo, keeper)

			result, err := svc.ChargeAgreement(context.Background(), validChargeRequest())
			if err != nil {
				t.Fatalf("ChargeAgreement: %v", err)
			}

			if len(keeper.chargeCalls) != 0 {
				t.Fatalf("这一期已经有结论却碰了渠道：%+v", keeper.chargeCalls)
			}
			if len(repo.chargeAttemptCalls) != 0 {
				t.Errorf("没碰渠道却记了一次尝试：%+v", repo.chargeAttemptCalls)
			}
			if len(repo.providerCall) != 0 {
				t.Errorf("没碰渠道却写了渠道调用流水：%+v", repo.providerCall)
			}
			if result.Status != tc.status {
				t.Errorf("回的状态 = %q，想要这一行本来的 %q", result.Status, tc.status)
			}
			if result.ChargeNo != testChargeNo {
				t.Errorf("回的单号 = %q，想要这一行上存着的 %q（重算一个等于在渠道侧开出第二笔）",
					result.ChargeNo, testChargeNo)
			}
		})
	}
}

// TestChargeAgreementRetriesOnceTheBackoffHasElapsed 是上一张表的另一面：**该碰的时候要碰**。
//
// 退避判据写反的话上面六条全绿，而这一期会永远停在 failed 上——用户看到的是一期没扣成之后
// 再也不会被扣（会员断在下一期，而系统里没有任何东西在动）。
//
// 第二种情形是 next_retry_at 为空且**没试满**：那是「从来没有排过」（列是 NULL），而 NULL 与
// 「排不上」在库上是同一个值，只能靠 attempt_count 区分——所以它是一条独立的判据。
func TestChargeAgreementRetriesOnceTheBackoffHasElapsed(t *testing.T) {
	due := testChargeNow.Add(-time.Minute)
	cases := []struct {
		name        string
		nextRetryAt *time.Time
	}{
		{"退避期已经过了", &due},
		{"从来没有排过下一次", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			charge := chargeOnFile(model.ChargeStatusFailed)
			charge.AttemptCount = 1
			charge.NextRetryAt = tc.nextRetryAt
			repo := &fakeRepository{agreement: activeAgreementOnFile(), charge: charge}
			keeper := &stubAgreementKeeper{charged: provider.AgreementChargeResult{
				ProviderTransactionID: "4200001234202609140000000001", Result: provider.ResultSuccess,
			}}
			svc := agreementKeeperService(t, repo, keeper)

			result, err := svc.ChargeAgreement(context.Background(), validChargeRequest())
			if err != nil {
				t.Fatalf("ChargeAgreement: %v", err)
			}

			if len(keeper.chargeCalls) != 1 {
				t.Fatalf("扣款调用 = %d 次，想要 1 次（退避到期就该再试）", len(keeper.chargeCalls))
			}
			// 重试**复用那一行上的号**：换一个就是在渠道侧开出第二笔订单，而两个单号不同，
			// 渠道那边无法去重。
			if keeper.chargeCalls[0].OutTradeNo != testChargeNo {
				t.Errorf("重试用的单号 = %q，想要这一行上存着的 %q",
					keeper.chargeCalls[0].OutTradeNo, testChargeNo)
			}
			if len(repo.chargeAttemptCalls) != 1 || repo.chargeAttemptCalls[0].Status != model.ChargeStatusCharging {
				t.Errorf("尝试记录 = %+v，想要一次把这一期推回 charging 的", repo.chargeAttemptCalls)
			}
			if result.Status != model.ChargeStatusCharging {
				t.Errorf("状态 = %q，想要 %q", result.Status, model.ChargeStatusCharging)
			}
		})
	}
}

// TestChargeAgreementStopsAtChargingUntilTheNotification 守这一刀最要紧的一句话：
// **受理不是结论**。
//
// 渠道回 result_code=SUCCESS 只说明它收下了这个请求，钱到没到由 notify_url 那条通知说
// （见 TestChargeNotificationSettlesBothOutcomes）。所以这里断言的是：落下去的状态是
// charging，而且**没有下一次重试的时间**——退避是给「确定没扣到」用的，给一次受理中的扣款
// 排下一次，就等于让同一期在钱可能已经动了的情况下再被扣一次。
func TestChargeAgreementStopsAtChargingUntilTheNotification(t *testing.T) {
	charge := chargeOnFile(model.ChargeStatusPending)
	// 金额认的是**这一行**（第一期定下来的），不是这次调用给的：契约上写的是 9.9 还是 19.9
	// 由那一行决定，调用方改不了。
	charge.Amount = 990
	repo := &fakeRepository{agreement: activeAgreementOnFile(), charge: charge}
	keeper := &stubAgreementKeeper{charged: provider.AgreementChargeResult{
		ProviderTransactionID: "4200001234202609140000000001", Result: provider.ResultSuccess,
	}}
	svc := agreementKeeperService(t, repo, keeper)

	result, err := svc.ChargeAgreement(context.Background(), validChargeRequest())
	if err != nil {
		t.Fatalf("ChargeAgreement: %v", err)
	}

	if len(keeper.chargeCalls) != 1 {
		t.Fatalf("扣款调用 = %d 次，想要 1 次（第一次落到 pending 那一支）", len(keeper.chargeCalls))
	}
	asked := keeper.chargeCalls[0]
	if asked.ProviderContractID != "1234567890" {
		t.Errorf("交给渠道的协议号 = %q，想要渠道侧那个（扣款认的是它，不是我们自己的 agreement_no）",
			asked.ProviderContractID)
	}
	if asked.OutTradeNo != testChargeNo {
		t.Errorf("交给渠道的单号 = %q，想要这一行上存着的 %q", asked.OutTradeNo, testChargeNo)
	}
	if asked.Amount != 990 {
		t.Errorf("交给渠道的金额 = %d，想要那一行上的 990（不是这次请求给的 1990）", asked.Amount)
	}
	if strings.TrimSpace(asked.Subject) == "" {
		t.Error("账单上那句话是空的：微信对 body 必填，空着会被拒")
	}
	if !strings.HasSuffix(asked.NotifyURL, "/v1/payments/agreement-charge-notify/"+catalog.ChannelCodeWeChatPay) {
		t.Errorf("notify_url = %q，想要**扣款结果**那条路（拼成协议通知那条路的话，结果永远回不来）", asked.NotifyURL)
	}
	if asked.Secrets.Get(stubSecretSlot) == "" {
		t.Error("交给适配器的凭据是空的：扣款要挂双向证书，取不到它连不上")
	}

	if len(repo.chargeAttemptCalls) != 1 {
		t.Fatalf("尝试记录 = %d 次，想要 1 次", len(repo.chargeAttemptCalls))
	}
	attempt := repo.chargeAttemptCalls[0]
	if attempt.Status != model.ChargeStatusCharging {
		t.Errorf("落下去的状态 = %q，想要 %q（受理不等于扣到钱）", attempt.Status, model.ChargeStatusCharging)
	}
	if attempt.NextRetryAt != nil {
		t.Errorf("受理中却排了下一次重试（%v）：钱可能已经动了，再发一次就是重复扣款", attempt.NextRetryAt)
	}
	if attempt.ProviderTransactionID != "4200001234202609140000000001" {
		t.Errorf("渠道流水号 = %q，想要受理时那个（用户在微信账单里看到的就是它）", attempt.ProviderTransactionID)
	}
	if result.Status != model.ChargeStatusCharging {
		t.Errorf("回给调用方的状态 = %q，想要 %q", result.Status, model.ChargeStatusCharging)
	}

	// 出网调用先留痕、再管状态：这一次询问已经真的发生过了。
	if len(repo.providerCall) != 1 {
		t.Fatalf("渠道调用流水 = %d 条，想要 1 条", len(repo.providerCall))
	}
	call := repo.providerCall[0]
	if call.Operation != model.CallOperationAgreementCharge {
		t.Errorf("流水上的操作 = %q，想要 %q", call.Operation, model.CallOperationAgreementCharge)
	}
	if call.RequestSummary["bizPeriod"] != testBizPeriod || call.RequestSummary["outTradeNo"] != testChargeNo {
		t.Errorf("流水摘要 = %v，想要能定位到这一期的那几项", call.RequestSummary)
	}
}

// TestChargeAgreementSchedulesTheNextTryWhenTheChannelRefuses 是「确定没扣到」那一条路。
//
// 三种次数各一条，因为退避按**这一次之后**的次数算（now + 24h × n），而满三次就不再排期。
// 算错一次的后果是重试节奏与订阅那边的「连续失败三次就停扣」对不上：一边还在重试，另一边
// 已经置了 suspended。
func TestChargeAgreementSchedulesTheNextTryWhenTheChannelRefuses(t *testing.T) {
	cases := []struct {
		name        string
		attempts    int
		wantNextTry *time.Time
	}{
		{"第一次被拒", 0, ptrTime(testChargeNow.Add(24 * time.Hour))},
		{"第二次被拒", 1, ptrTime(testChargeNow.Add(48 * time.Hour))},
		{"第三次被拒：试满，不再排期", 2, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			charge := chargeOnFile(model.ChargeStatusPending)
			charge.AttemptCount = tc.attempts
			repo := &fakeRepository{agreement: activeAgreementOnFile(), charge: charge}
			keeper := &stubAgreementKeeper{charged: provider.AgreementChargeResult{
				Result: provider.ResultFailed, FailureCode: "NOTENOUGH", FailureMessage: "余额不足",
			}}
			svc := agreementKeeperService(t, repo, keeper)

			result, err := svc.ChargeAgreement(context.Background(), validChargeRequest())
			if err != nil {
				t.Fatalf("ChargeAgreement: %v", err)
			}

			if len(repo.chargeAttemptCalls) != 1 {
				t.Fatalf("尝试记录 = %d 次，想要 1 次", len(repo.chargeAttemptCalls))
			}
			attempt := repo.chargeAttemptCalls[0]
			if attempt.Status != model.ChargeStatusFailed {
				t.Errorf("落下去的状态 = %q，想要 %q", attempt.Status, model.ChargeStatusFailed)
			}
			if attempt.FailureCode != "NOTENOUGH" {
				t.Errorf("失败码 = %q，想要渠道原话（它进流水，是排查时唯一能对上的东西）", attempt.FailureCode)
			}
			switch {
			case tc.wantNextTry == nil && attempt.NextRetryAt != nil:
				t.Errorf("试满了却还排了下一次：%v", attempt.NextRetryAt)
			case tc.wantNextTry != nil && attempt.NextRetryAt == nil:
				t.Errorf("还该重试却没有排期")
			case tc.wantNextTry != nil && !attempt.NextRetryAt.Equal(*tc.wantNextTry):
				t.Errorf("下一次重试 = %v，想要 %v", attempt.NextRetryAt, *tc.wantNextTry)
			}
			if result.Status != model.ChargeStatusFailed {
				t.Errorf("回给调用方的状态 = %q，想要 %q", result.Status, model.ChargeStatusFailed)
			}
		})
	}
}

// TestChargeAgreementWaitsForTheNotificationWhenTheResultIsUnclear 守「结果不明」那一条。
//
// 请求发出去没等到应答时，钱**可能已经动了**，所以这一期必须留在 charging 上等通知。记成失败
// 的后果不是少赚一期，而是那条随后到达的通知会撞上「只有 pending/charging 能推进」被丢掉
// ——钱扣了、账上什么都没有，而用户那边自动续费已经「失败」过一次了。
//
// 唯一的例外是适配器明确标了 NOT_SENT（一个字节都没发出去）：那一次确实没发生，可以重试。
func TestChargeAgreementWaitsForTheNotificationWhenTheResultIsUnclear(t *testing.T) {
	cases := []struct {
		name       string
		charged    provider.AgreementChargeResult
		chargeErr  error
		wantStatus string
	}{
		{
			name:       "渠道超时：连发出去没有都不确定",
			charged:    provider.AgreementChargeResult{Result: provider.ResultUnknown},
			chargeErr:  context.DeadlineExceeded,
			wantStatus: model.ChargeStatusCharging,
		},
		{
			name: "报文读不懂",
			charged: provider.AgreementChargeResult{
				Result: provider.ResultUnknown, FailureCode: "SIGNERROR",
			},
			wantStatus: model.ChargeStatusCharging,
		},
		{
			// NOT_SENT 是适配器给的那句「确定没发出去」——它有明确的来源（httpx 的 NotSent），
			// 不是猜的。只有这一种结果不明可以重试。
			name: "一个字节都没发出去",
			charged: provider.AgreementChargeResult{
				Result: provider.ResultUnknown, FailureCode: "NOT_SENT",
			},
			wantStatus: model.ChargeStatusFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{
				agreement: activeAgreementOnFile(),
				charge:    chargeOnFile(model.ChargeStatusPending),
			}
			keeper := &stubAgreementKeeper{charged: tc.charged, chargeErr: tc.chargeErr}
			svc := agreementKeeperService(t, repo, keeper)

			if _, err := svc.ChargeAgreement(context.Background(), validChargeRequest()); err != nil {
				t.Fatalf("ChargeAgreement: %v", err)
			}

			if len(repo.chargeAttemptCalls) != 1 {
				t.Fatalf("尝试记录 = %d 次，想要 1 次", len(repo.chargeAttemptCalls))
			}
			attempt := repo.chargeAttemptCalls[0]
			if attempt.Status != tc.wantStatus {
				t.Errorf("落下去的状态 = %q，想要 %q", attempt.Status, tc.wantStatus)
			}
			// 「为什么没问出结论」不进那一行（它已经回到 charging，失败原因描述的是上一次），
			// 而是留在渠道调用流水上：那一次询问真的发生过，这是唯一的线索。
			if len(repo.providerCall) != 1 {
				t.Fatalf("渠道调用流水 = %d 条，想要 1 条", len(repo.providerCall))
			}
			call := repo.providerCall[0]
			if call.Result != string(provider.ResultUnknown) {
				t.Errorf("流水上的结果 = %q，想要 %q（我们没问出结论，不是渠道拒了）",
					call.Result, provider.ResultUnknown)
			}
			if call.ProviderCode == "" && call.RequestSummary["error"] == nil {
				t.Error("没问出结论却没留下线索：流水上既没有渠道原话，也没有我们这边的错误")
			}
		})
	}
}

// TestChargeAgreementRefusesBeforeTouchingAnything 是入口那几道闸。
//
// 「没碰渠道、也没建行」是断言的一部分：一条不合法的扣款请求在库上留下任何痕迹都是错的
// ——建了行就是给这一期占了一个幂等键，而那一期从来没有真的发生过。
func TestChargeAgreementRefusesBeforeTouchingAnything(t *testing.T) {
	cases := []struct {
		name      string
		agreement *model.PaymentAgreement
		mutate    func(*ChargeAgreementRequest)
		wantErr   error
	}{
		{
			name: "没给协议号", agreement: activeAgreementOnFile(),
			mutate:  func(r *ChargeAgreementRequest) { r.AgreementNo = "" },
			wantErr: ErrAgreementNoRequired,
		},
		{
			// 期次不是备注，是幂等键（库上 UNIQUE (agreement_id, biz_period)）：缺了它没有任何
			// 东西挡得住同一期被扣两次。
			name: "没给期次", agreement: activeAgreementOnFile(),
			mutate:  func(r *ChargeAgreementRequest) { r.BizPeriod = "  " },
			wantErr: ErrBizPeriodRequired,
		},
		{
			name: "金额不是正数", agreement: activeAgreementOnFile(),
			mutate:  func(r *ChargeAgreementRequest) { r.Amount = 0 },
			wantErr: ErrAmountNotPositive,
		},
		{
			name: "这份协议不在库里", agreement: nil,
			wantErr: repository.ErrAgreementNotFound,
		},
		{
			// 还没生效（用户在微信那边还没点确认）：扣款请求发出去必然被渠道拒，而那条失败流水
			// 会让「连续失败」的计数凭空涨一格，用户什么都没干。
			name: "协议还没生效", agreement: func() *model.PaymentAgreement {
				agreement := activeAgreementOnFile()
				agreement.Status = model.AgreementStatusPending
				return agreement
			}(),
			wantErr: ErrAgreementNotChargable,
		},
		{
			name: "协议已经解约", agreement: func() *model.PaymentAgreement {
				agreement := activeAgreementOnFile()
				agreement.Status = model.AgreementStatusTerminated
				return agreement
			}(),
			wantErr: ErrAgreementNotChargable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{agreement: tc.agreement, charge: chargeOnFile(model.ChargeStatusPending)}
			keeper := &stubAgreementKeeper{}
			svc := agreementKeeperService(t, repo, keeper)

			request := validChargeRequest()
			if tc.mutate != nil {
				tc.mutate(&request)
			}
			if _, err := svc.ChargeAgreement(context.Background(), request); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，想要 %v", err, tc.wantErr)
			}
			if len(keeper.chargeCalls) != 0 {
				t.Error("被拦下的扣款不该去问渠道")
			}
			if len(repo.createChargeCalls) != 0 {
				t.Error("被拦下的扣款不该建行（建了就是给这一期占了一个幂等键）")
			}
			if len(repo.providerCall) != 0 {
				t.Error("被拦下的扣款不该写渠道调用流水")
			}
		})
	}
}

// TestChargeAgreementRefusesAnUnknownStatus：词表外的状态是**我们自己的编码错误**。
//
// 它必须报错而不是当成「不用扣」：后者的后果是这一期安静地永远不被扣，而会员到期日不会
// 自己往后走——用户看到的是「自动续费开着、会员却断了」。
func TestChargeAgreementRefusesAnUnknownStatus(t *testing.T) {
	repo := &fakeRepository{
		agreement: activeAgreementOnFile(),
		charge:    chargeOnFile("changing"),
	}
	keeper := &stubAgreementKeeper{}
	svc := agreementKeeperService(t, repo, keeper)

	_, err := svc.ChargeAgreement(context.Background(), validChargeRequest())
	if err == nil {
		t.Fatal("词表外的状态却成功了：这一期会安静地永远不被扣")
	}
	if !strings.Contains(err.Error(), "changing") {
		t.Errorf("错误串 = %v，想要带上那个不认识的取值（那是唯一的线索）", err)
	}
	if len(keeper.chargeCalls) != 0 {
		t.Error("不认识的状态却去问了渠道")
	}
}

// TestChargeAgreementRecordsTheCallBeforeItWritesTheStatus 守次序：**出网调用先留痕**。
//
// 即便随后那一步（把结果写回这一行）失败了，这一次询问也已经真的发生过了——钱可能动了。
// 没有这条流水，账上就只剩下一个停在 pending 的行和一条库故障日志。
func TestChargeAgreementRecordsTheCallBeforeItWritesTheStatus(t *testing.T) {
	repo := &fakeRepository{
		agreement: activeAgreementOnFile(),
		charge:    chargeOnFile(model.ChargeStatusPending),
		// 受理之后那一次落库失败。
		chargeAttemptErr: errors.New("db is down"),
	}
	keeper := &stubAgreementKeeper{charged: provider.AgreementChargeResult{
		ProviderTransactionID: "4200001234202609140000000001", Result: provider.ResultSuccess,
	}}
	svc := agreementKeeperService(t, repo, keeper)

	if _, err := svc.ChargeAgreement(context.Background(), validChargeRequest()); err == nil {
		t.Fatal("落库失败却回了成功：调用方会以为这一期已经受理了")
	}
	if len(repo.providerCall) != 1 {
		t.Fatalf("渠道调用流水 = %d 条，想要 1 条（那一次询问真的发生过了）", len(repo.providerCall))
	}
	call := repo.providerCall[0]
	if call.Operation != model.CallOperationAgreementCharge || call.ProviderCode != "" {
		t.Errorf("流水 = %+v，想要一条挂在这一期上的扣款记录", call)
	}
	if call.RequestSummary["outTradeNo"] != testChargeNo {
		t.Errorf("流水摘要 = %v，想要能定位到这一期的单号", call.RequestSummary)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// ——— 解约 ———
//
// 解约这条路上只有一句话要紧：**先撤渠道，再改本地**。反过来的话，一次网络抖动会留下「本地
// 已解约、微信那边还挂着」，而微信下个月照扣（用户在小程序里看到的是「自动续费：已关闭」）。

// TestTerminateAgreementAsksTheChannelThenSettlesLocally 是成功那一条。
func TestTerminateAgreementAsksTheChannelThenSettlesLocally(t *testing.T) {
	repo := &fakeRepository{agreement: activeAgreementOnFile(), agreementSettleChanged: true}
	keeper := &stubAgreementKeeper{terminated: provider.AgreementCallResult{Result: provider.ResultSuccess}}
	svc := agreementKeeperService(t, repo, keeper)

	result, err := svc.TerminateAgreement(context.Background(), TerminateAgreementRequest{
		AgreementNo: "AGR20260901120000000001", Reason: "用户关闭自动续费", RequestID: testRequest,
	})
	if err != nil {
		t.Fatalf("TerminateAgreement: %v", err)
	}

	if len(keeper.terminateCalls) != 1 {
		t.Fatalf("解约调用 = %d 次，想要 1 次", len(keeper.terminateCalls))
	}
	asked := keeper.terminateCalls[0]
	if asked.ProviderContractID != "1234567890" {
		t.Errorf("交给渠道的协议号 = %q，想要渠道侧那个（微信的 deletecontract 认的是 contract_id）",
			asked.ProviderContractID)
	}
	if strings.TrimSpace(asked.Reason) == "" {
		t.Error("解约原因是空的：那是这件事在渠道账单上留下的唯一一句人话")
	}

	if len(repo.agreementSettles) != 1 {
		t.Fatalf("本地结算 = %d 次，想要 1 次（渠道撤了才轮到本地）", len(repo.agreementSettles))
	}
	settled := repo.agreementSettles[0]
	if settled.Target != model.AgreementStatusTerminated {
		t.Errorf("本地目标状态 = %q，想要 %q", settled.Target, model.AgreementStatusTerminated)
	}
	// 本地这句与发给渠道的那句必须**是同一句**：两处各写一句的话，用户看到的与渠道账单上的
	// 迟早对不上。
	if settled.Reason != asked.Reason {
		t.Errorf("本地留痕的原因 = %q，发给渠道的 = %q，两者必须是同一句", settled.Reason, asked.Reason)
	}
	if result.Status != model.AgreementStatusTerminated || result.FailureCode != "" {
		t.Errorf("结果 = %+v，想要一句干净的「已解约」", result)
	}
	if len(repo.providerCall) != 1 || repo.providerCall[0].Operation != model.CallOperationAgreementTerminate {
		t.Errorf("渠道调用流水 = %+v，想要一条解约记录", repo.providerCall)
	}
}

// TestTerminateAgreementDoesNotChangeAnythingWhenTheChannelRefuses 守「渠道明确拒绝」。
//
// 「这份合同已经不存在了」是一句**结论**不是错误：重试不会有别的结果，所以它走 FailureCode
// 回给调用方，而本地**一个字节都不改**。membership-service 那边据此什么都不做、把话原样报给
// 用户（见计划 §二.4）——它要是自作主张收掉本地状态，用户就会以为关掉了，而渠道那边还挂着。
func TestTerminateAgreementDoesNotChangeAnythingWhenTheChannelRefuses(t *testing.T) {
	repo := &fakeRepository{agreement: activeAgreementOnFile(), agreementSettleChanged: true}
	keeper := &stubAgreementKeeper{terminated: provider.AgreementCallResult{
		Result: provider.ResultFailed, FailureCode: "CONTRACTNOTEXIST", FailureMessage: "合同不存在",
	}}
	svc := agreementKeeperService(t, repo, keeper)

	result, err := svc.TerminateAgreement(context.Background(), TerminateAgreementRequest{
		AgreementNo: "AGR20260901120000000001", Reason: "用户关闭自动续费",
	})
	if err != nil {
		t.Fatalf("渠道明确拒绝**不该**是 error（它是一句结论）：%v", err)
	}

	if result.FailureCode != "CONTRACTNOTEXIST" {
		t.Errorf("失败码 = %q，想要渠道原话（调用方要拿它决定怎么跟用户说）", result.FailureCode)
	}
	if result.Status != model.AgreementStatusActive {
		t.Errorf("回的状态 = %q，想要本地**原样**的那个", result.Status)
	}
	if len(repo.agreementSettles) != 0 {
		t.Fatalf("渠道没同意却改了本地：%+v", repo.agreementSettles)
	}
}

// TestTerminateAgreementRefusesToGuessWhenTheAnswerIsUnclear：超时、报文读不懂时**什么都不改**
// 并报错。
//
// 把一次解约超时当成「已解约」是本服务里最贵的一种误判——用户以为关了，微信下个月还在扣。
func TestTerminateAgreementRefusesToGuessWhenTheAnswerIsUnclear(t *testing.T) {
	repo := &fakeRepository{agreement: activeAgreementOnFile(), agreementSettleChanged: true}
	keeper := &stubAgreementKeeper{
		terminated:   provider.AgreementCallResult{Result: provider.ResultUnknown},
		terminateErr: context.DeadlineExceeded,
	}
	svc := agreementKeeperService(t, repo, keeper)

	result, err := svc.TerminateAgreement(context.Background(), TerminateAgreementRequest{
		AgreementNo: "AGR20260901120000000001", Reason: "用户关闭自动续费",
	})
	if !errors.Is(err, ErrProviderResultUncertain) {
		t.Fatalf("err = %v，想要 ErrProviderResultUncertain", err)
	}
	if result != nil {
		t.Errorf("结果 = %+v，想要 nil（什么都没问出来）", result)
	}
	if len(repo.agreementSettles) != 0 {
		t.Fatalf("结果不明却改了本地：%+v", repo.agreementSettles)
	}
}

// TestTerminateAgreementSkipsTheChannelWhenThereIsNothingToCancel：两种「渠道那边不用问」的情形。
//
//   - 本地已经是终态：渠道上一刀已经撤过了，再问一次只会拿到「合同不存在」，那会变成一句
//     回给用户的失败，描述的却是我们已经知道的现状。
//   - 协议上没记渠道侧的号：它还停在 pending（用户从来没在微信那边点过确认）。V2 在 Create
//     时不做渠道调用，所以渠道那边**不存在**这份合同，去解约只会拿到一句噪音。
func TestTerminateAgreementSkipsTheChannelWhenThereIsNothingToCancel(t *testing.T) {
	cases := []struct {
		name        string
		agreement   *model.PaymentAgreement
		wantSettles int
	}{
		{
			name: "本地已经是终态",
			agreement: func() *model.PaymentAgreement {
				agreement := activeAgreementOnFile()
				agreement.Status = model.AgreementStatusTerminated
				return agreement
			}(),
			wantSettles: 0,
		},
		{
			name: "渠道侧还没有凭证",
			agreement: func() *model.PaymentAgreement {
				agreement := activeAgreementOnFile()
				agreement.Status = model.AgreementStatusPending
				agreement.ContractNo = ""
				return agreement
			}(),
			wantSettles: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{agreement: tc.agreement, agreementSettleChanged: true}
			keeper := &stubAgreementKeeper{}
			svc := agreementKeeperService(t, repo, keeper)

			result, err := svc.TerminateAgreement(context.Background(), TerminateAgreementRequest{
				AgreementNo: "AGR20260901120000000001", Reason: "用户关闭自动续费",
			})
			if err != nil {
				t.Fatalf("TerminateAgreement: %v", err)
			}
			if len(keeper.terminateCalls) != 0 {
				t.Fatalf("没有可撤的东西却去问了渠道：%+v", keeper.terminateCalls)
			}
			if len(repo.agreementSettles) != tc.wantSettles {
				t.Fatalf("本地结算 = %d 次，想要 %d 次", len(repo.agreementSettles), tc.wantSettles)
			}
			if result.Status != model.AgreementStatusTerminated {
				t.Errorf("状态 = %q，想要 %q", result.Status, model.AgreementStatusTerminated)
			}
		})
	}
}

// TestTerminateAgreementNeedsAnAgreementNumber 是入口那道闸：连库都不该查。
func TestTerminateAgreementNeedsAnAgreementNumber(t *testing.T) {
	repo := &fakeRepository{agreement: activeAgreementOnFile()}
	keeper := &stubAgreementKeeper{}
	svc := agreementKeeperService(t, repo, keeper)

	if _, err := svc.TerminateAgreement(context.Background(), TerminateAgreementRequest{}); !errors.Is(err, ErrAgreementNoRequired) {
		t.Fatalf("err = %v，想要 ErrAgreementNoRequired", err)
	}
	if len(keeper.terminateCalls) != 0 {
		t.Error("没给协议号却去问了渠道")
	}
}

// ——— 扣款结果通知 ———
//
// 报文形状与验签在 wechatpay 包里验（见 notify_test.go 的 TestParseChargeNotify*）；这里用假
// 适配器，验的是 service 拿到的结论怎么处置，以及处置错的时候**有没有碰那一期**。

// stubAgreementChargeNotifier 是一个额外认扣款结果通知的假适配器，用内嵌而不是另起一个完整
// 实现（同 stubAgreementNotifier）。
type stubAgreementChargeNotifier struct {
	stubAgreementKeeper
	notification provider.AgreementChargeNotification
	verifyErr    error
	calls        []provider.NotificationRequest
}

func (s *stubAgreementChargeNotifier) VerifyAgreementChargeNotification(_ context.Context, req provider.NotificationRequest) (provider.AgreementChargeNotification, error) {
	s.calls = append(s.calls, req)
	return s.notification, s.verifyErr
}

// chargeNotifier 组装一个「微信那条渠道指向假适配器」的业务层（同 agreementNotifier）。
func chargeNotifier(t *testing.T, repo *fakeRepository, notifier *stubAgreementChargeNotifier) *PaymentService {
	t.Helper()
	notifier.name = catalog.ChannelCodeWeChatPay
	return newServiceWithCatalog(t, repo, testCatalog(), notifier, catalog.CodeWechatPapay, nil,
		func(*catalog.Channel, string) string { return "secret" })
}

// TestChargeNotificationSettlesBothOutcomes 钉住「渠道的结论翻成本地状态与事件」这一段。
//
// 两个结论各一条：说扣到了就推 succeeded（并出 charge_succeeded，下游据此续会员），说没扣到
// 就推 failed（出 charge_failed，下游据此累计连续失败）。翻错了的后果是会员在没扣到钱的
// 情况下被续，或者钱扣了会员却断掉。
func TestChargeNotificationSettlesBothOutcomes(t *testing.T) {
	body := []byte("<xml>a charge result notification</xml>")
	cases := []struct {
		name          string
		notification  provider.AgreementChargeNotification
		wantTarget    string
		wantEventType string
	}{
		{
			name: "渠道说扣到了",
			notification: provider.AgreementChargeNotification{
				OutTradeNo: testChargeNo, ProviderTransactionID: "4200001234202609140000000001",
				ProviderContractID: "1234567890", Amount: 1990, Result: provider.ResultSuccess,
			},
			wantTarget: model.ChargeStatusSucceeded, wantEventType: dto.EventAgreementChargeSucceeded,
		},
		{
			name: "渠道说没扣到",
			notification: provider.AgreementChargeNotification{
				OutTradeNo: testChargeNo, ProviderContractID: "1234567890", Amount: 1990,
				Result: provider.ResultFailed, FailureCode: "NOTENOUGH", FailureMessage: "余额不足",
			},
			wantTarget: model.ChargeStatusFailed, wantEventType: dto.EventAgreementChargeFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{
				charge:                    chargeOnFile(model.ChargeStatusCharging),
				chargeNotificationChanged: true,
			}
			notifier := &stubAgreementChargeNotifier{notification: tc.notification}
			svc := chargeNotifier(t, repo, notifier)

			result, err := svc.HandleAgreementChargeNotification(context.Background(), ChargeNotificationRequest{
				ChannelCode: catalog.ChannelCodeWeChatPay, Body: body,
			})
			if err != nil {
				t.Fatalf("HandleAgreementChargeNotification: %v", err)
			}

			// 通知号是**报文自身的摘要**（微信的扣款通知没有通知号），这里独立算一遍而不是调
			// 被测的那个函数——否则「算法改了」这件事永远测不出来。
			if len(repo.notificationCalls) != 1 {
				t.Fatalf("通知落库 = %d 次，想要 1 次", len(repo.notificationCalls))
			}
			inserted := repo.notificationCalls[0]
			wantID := fmt.Sprintf("%x", sha256.Sum256(body))
			if inserted.NotificationID != wantID {
				t.Errorf("通知号 = %q，想要报文自身的 sha256", inserted.NotificationID)
			}
			if inserted.EventType != tc.wantEventType {
				t.Errorf("事件类型 = %q，想要 %q（它是路由键，三段式，payment.* 通配符匹配不到它）",
					inserted.EventType, tc.wantEventType)
			}
			// 代扣不建 payments 行（payments.order_no 是订单号，而代扣没有订单）：留空而不是
			// 编一个号，编的那个号会让对账的人去找一张不存在的支付单。
			if inserted.PaymentNo != "" {
				t.Errorf("支付单号 = %q，想要空（代扣不建 payments 行）", inserted.PaymentNo)
			}

			if len(repo.chargeNotificationSettles) != 1 {
				t.Fatalf("结算 = %d 次，想要 1 次", len(repo.chargeNotificationSettles))
			}
			settled := repo.chargeNotificationSettles[0]
			if settled.Target != tc.wantTarget {
				t.Errorf("目标状态 = %q，想要 %q", settled.Target, tc.wantTarget)
			}
			// 报文里的金额**原样送下去**：对账在仓储的锁内做（那里才看得到这一行当前的金额），
			// service 这一层不许替它判、更不许改成本地那个数。
			if settled.Amount != 1990 {
				t.Errorf("送下去的金额 = %d，想要报文里的那个（对账是仓储在锁内做的）", settled.Amount)
			}
			if settled.FailureCode != tc.notification.FailureCode {
				t.Errorf("失败码 = %q，想要报文里的那个", settled.FailureCode)
			}
			if result.EventType != tc.wantEventType || !result.Settled {
				t.Errorf("结果 = %+v，想要一条真的推进了这一期的 %q", result, tc.wantEventType)
			}
		})
	}
}

// TestChargeNotificationRefusesAMismatchedAmount 守「金额对不上就拒绝入账」。
//
// 判据在仓储的锁内（见 repository.ErrChargeAmountMismatch），这一层能验的是**处置**：错误
// 照原样报出去、那一行**没被推进**，而且这条通知被标成 failed 留痕。吞掉它换成「以通知为准」
// 等于让别人拿一条别的单的通知来改我们这一期的账。
func TestChargeNotificationRefusesAMismatchedAmount(t *testing.T) {
	repo := &fakeRepository{
		charge:                chargeOnFile(model.ChargeStatusCharging),
		chargeNotificationErr: repository.ErrChargeAmountMismatch,
	}
	notifier := &stubAgreementChargeNotifier{notification: provider.AgreementChargeNotification{
		OutTradeNo: testChargeNo, Amount: 1, Result: provider.ResultSuccess,
	}}
	svc := chargeNotifier(t, repo, notifier)

	_, err := svc.HandleAgreementChargeNotification(context.Background(), ChargeNotificationRequest{
		ChannelCode: catalog.ChannelCodeWeChatPay, Body: []byte("<xml>charge result</xml>"),
	})
	if !errors.Is(err, repository.ErrChargeAmountMismatch) {
		t.Fatalf("err = %v，想要 ErrChargeAmountMismatch 原样透出来", err)
	}
	// 那一行停在原地：这一次拒绝没有动它。
	if repo.charge.Status != model.ChargeStatusCharging {
		t.Errorf("状态 = %q，想要原样停在 %q", repo.charge.Status, model.ChargeStatusCharging)
	}
	// 留痕走**独立连接**（业务事务已经回滚了）：那条 failed 行是「我们拒过这条通知」的唯一证据。
	if len(repo.markedNotifications) != 1 || repo.markedNotifications[0].status != model.NotificationFailed {
		t.Errorf("通知留痕 = %+v，想要一条标成 %q 的", repo.markedNotifications, model.NotificationFailed)
	}
}

// TestChargeNotificationSettlesOnceWhenTheSameBodyIsRedelivered 守重投。
//
// 微信在没收到成功应答时会按自己的计划重投同一个报文。第二次必须**只回答、不再结算**：那一期
// 已经推进过了，再推一次就是同一笔钱记两次账。
func TestChargeNotificationSettlesOnceWhenTheSameBodyIsRedelivered(t *testing.T) {
	body := []byte("<xml>charge result</xml>")
	repo := &fakeRepository{
		charge:                    chargeOnFile(model.ChargeStatusCharging),
		chargeNotificationChanged: true,
	}
	notifier := &stubAgreementChargeNotifier{notification: provider.AgreementChargeNotification{
		OutTradeNo: testChargeNo, Amount: 1990, Result: provider.ResultSuccess,
	}}
	svc := chargeNotifier(t, repo, notifier)

	first, err := svc.HandleAgreementChargeNotification(context.Background(), ChargeNotificationRequest{
		ChannelCode: catalog.ChannelCodeWeChatPay, Body: body,
	})
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	if !first.Settled || first.Duplicate {
		t.Fatalf("第一次的结果 = %+v，想要一次真的结算", first)
	}

	// 第二次：那一行已经在了（UNIQUE (provider, notification_id) 挡住），且上一次处理完了。
	repo.notification = &repository.NotificationRecord{
		ID: "n-1", ExistingStatus: model.NotificationProcessed, ExistingAgeSeconds: 1,
	}
	second, err := svc.HandleAgreementChargeNotification(context.Background(), ChargeNotificationRequest{
		ChannelCode: catalog.ChannelCodeWeChatPay, Body: body,
	})
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("第二次的结果 = %+v，想要一条「重投、已处理过」", second)
	}
	if second.Settled {
		t.Error("重投却结算了：同一笔钱会被记两次")
	}
	if len(repo.chargeNotificationSettles) != 1 {
		t.Errorf("结算 = %d 次，想要 1 次（重投只回答、不再结算）", len(repo.chargeNotificationSettles))
	}
}
