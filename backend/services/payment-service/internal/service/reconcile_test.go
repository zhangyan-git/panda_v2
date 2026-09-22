package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组测试覆盖主动查单那条路（见 reconcile.go）：一笔停在 pending 的支付单被问了一次渠道
// 之后，我们对它做什么、不做什么。
//
// 这里最要紧的不是「成功的那条走通了」，而是**其余每一种回答都不许把钱判死**：查单说还没结、
// 出网就没通、金额对不上、这一族的渠道压根没有查单接口——这些情形下支付单必须原样停在 pending。
// 那几条断言是这一整个任务存在的意义的反面：写得松一点，它就会变成一台「把用户已经付过的钱
// 标成失败」的机器。

// pendingPayment 是一张「发起之后一直没结论」的支付单：主动查单这条路上唯一的输入形状。
//
// provider 与 payment_method 都必须是真的（`ums` 与目录里的一条 code），否则 resolveMethod
// 会解不出渠道、用例验的就变成别的分支了。
func pendingPayment() model.Payment {
	return model.Payment{
		ID: "payment-1", PaymentNo: "PAY20260914120000000001", OrderNo: testOrderNo,
		UserID: testUserID, Amount: 1980,
		Provider:      catalog.ChannelCodeUMS,
		PaymentMethod: testMethodCode,
		Status:        model.PaymentPending,
		RequestID:     testRequest,
	}
}

// stubQuerier 是一个**会查单**的假适配器：它在 stubProvider（发起与验签那条路）之上补一个
// Query。
//
// 用嵌入而不是往 stubProvider 上加个字段：那一族「没有查单接口」的渠道正是要验的一种情形
// （见 TestReconcileSkipsAProviderWithoutQuery），而它是靠**类型断言失败**表达的——stubProvider
// 一旦自己实现了 Query，那条用例就永远为真、什么也验不到了。
type stubQuerier struct {
	*stubProvider
	result     provider.CreateResult
	err        error
	queryCalls []provider.QueryRequest
}

func (s *stubQuerier) Query(_ context.Context, req provider.QueryRequest) (provider.CreateResult, error) {
	s.queryCalls = append(s.queryCalls, req)
	return s.result, s.err
}

// newReconcileFixture 装一套「目录里那条 code 指向 stubQuerier」的业务层，外加一批待查的
// 支付单。
//
// settle 给了一个非 nil 的默认回答：真仓储在 err 为 nil 时从不返回 nil（见 settleInTx 的
// 每一条返回），调用方也照着这一点解引用它。
func newReconcileFixture(t *testing.T, pending []model.Payment, adapter provider.Provider) (*PaymentService, *fakeRepository) {
	t.Helper()
	repo := &fakeRepository{
		stalePending: pending,
		settle:       &repository.PaymentSettlement{Payment: &pending[0]},
	}
	svc := newServiceWith(t, repo, adapter, testMethodCode, nil,
		func(*catalog.Channel, string) string { return "secret" })
	return svc, repo
}

// settled 数这一轮真正把支付单推到了终态的笔数——也就是「做了事」的那部分。
//
// 它在这里写成一个小函数而不是给 ReconcileStats 加个方法：那个类型是给 worker 记账与打日志用的，
// 「Succeeded + Failed」是这几条用例的判据，不是它自己要提供的读数。
func settled(stats ReconcileStats) int { return stats.Succeeded + stats.Failed }

// reconciled 跑一轮并断言它没报错。
func reconciled(t *testing.T, svc *PaymentService) ReconcileStats {
	t.Helper()
	stats, err := svc.ReconcilePendingPayments(context.Background(), time.Now().Add(-5*time.Minute), 20)
	if err != nil {
		t.Fatalf("ReconcilePendingPayments: %v", err)
	}
	return stats
}

func TestReconcileSettlesASucceededPayment(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
		Result:                provider.ResultSuccess,
		ProviderTransactionID: "TXN-9",
		ResponseSummary:       map[string]any{"totalAmount": payment.Amount},
	}}
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)

	stats := reconciled(t, svc)

	if stats.Scanned != 1 || stats.Succeeded != 1 || stats.Failed != 0 {
		t.Fatalf("计数 = %+v，期望问 1 笔、成 1 笔", stats)
	}

	// 问的是这一笔，带上了目录里解出来的那条方式与解析出来的凭据。查单要签名，凭据少不了
	// ——少了它适配器会拒，而在这一层看不出来。
	if len(querier.queryCalls) != 1 {
		t.Fatalf("问了 %d 次渠道，期望 1 次", len(querier.queryCalls))
	}
	asked := querier.queryCalls[0]
	if asked.PaymentNo != payment.PaymentNo || asked.OrderNo != payment.OrderNo {
		t.Errorf("问的是 %s / %s，期望 %s / %s", asked.PaymentNo, asked.OrderNo, payment.PaymentNo, payment.OrderNo)
	}
	if asked.Method.Code != testMethodCode {
		t.Errorf("问单用的支付方式 = %q，期望 %q", asked.Method.Code, testMethodCode)
	}
	if asked.Secrets.Get(stubSecretSlot) != "secret" {
		t.Errorf("查单没带上解析出来的凭据：%v", asked.Secrets.Get(stubSecretSlot))
	}

	// 落下去的是**查单得到的结论**，而且没有回调记录可标。
	if len(repo.settleCalls) != 1 {
		t.Fatalf("结算被调了 %d 次，期望 1 次", len(repo.settleCalls))
	}
	settle := repo.settleCalls[0]
	if settle.NotificationID != "" {
		t.Errorf("NotificationID = %q，期望空——这一条结论不是回调带来的", settle.NotificationID)
	}
	if !settle.Succeeded {
		t.Error("结算成的是失败，期望成功")
	}
	if settle.Amount != payment.Amount {
		t.Errorf("结算金额 = %d，期望 %d", settle.Amount, payment.Amount)
	}
	if settle.Provider != payment.Provider {
		t.Errorf("结算渠道 = %q，期望 %q", settle.Provider, payment.Provider)
	}
	if settle.ProviderTransactionID != "TXN-9" {
		t.Errorf("渠道交易号 = %q，期望 TXN-9", settle.ProviderTransactionID)
	}
	if settle.FailureCode != "" {
		t.Errorf("成功那一次带上了失败码 %q", settle.FailureCode)
	}

	// 查单本身也要在渠道调用流水里留一行。这一笔问过几次、每次对面怎么答的，事后只有这里
	// 查得回来。
	if len(repo.providerCall) != 1 {
		t.Fatalf("渠道调用流水写了 %d 行，期望 1 行", len(repo.providerCall))
	}
	call := repo.providerCall[0]
	if call.Operation != model.CallOperationQuery {
		t.Errorf("流水 operation = %q，期望 %q", call.Operation, model.CallOperationQuery)
	}
	if call.Result != string(provider.ResultSuccess) || call.PaymentNo != payment.PaymentNo {
		t.Errorf("流水 = %+v，期望成功的、挂在 %s 上", call, payment.PaymentNo)
	}
}

func TestReconcileSettlesAProviderReportedFailure(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
		Result:         provider.ResultFailed,
		FailureCode:    "query_reported_failure",
		FailureMessage: "provider reported TRADE_CLOSED",
	}}
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)

	stats := reconciled(t, svc)

	// 明确失败才落 failed（见 ums 的 interpretQuery：只有 TRADE_CLOSED 那一组词算数）。
	if stats.Failed != 1 || stats.Succeeded != 0 {
		t.Fatalf("计数 = %+v，期望失败 1 笔", stats)
	}
	settle := repo.settleCalls[0]
	if settle.Succeeded {
		t.Error("结算成的是成功，期望失败")
	}
	if settle.FailureCode != "query_reported_failure" {
		t.Errorf("失败码 = %q，期望把渠道那个结论带下去", settle.FailureCode)
	}
}

// TestReconcileLeavesAnUnresolvedPaymentAlone 是这一组里最要紧的一条。
//
// 「查不到」与「没付钱」是两件事：渠道说这笔还没结（WAIT_BUYER_PAY）、或者对面回了一句我们
// 读不懂的东西，都只是「我们还是不知道」。这时候把支付单标成 failed，等于把一笔可能已经收了
// 的钱从账上抹掉——而用户手上那笔扣款是真的。
func TestReconcileLeavesAnUnresolvedPaymentAlone(t *testing.T) {
	for _, result := range []provider.Result{provider.ResultUnknown, provider.ResultTimeout} {
		t.Run(string(result), func(t *testing.T) {
			payment := pendingPayment()
			querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
				Result: result, FailureCode: "query_inconclusive",
			}}
			svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)

			stats := reconciled(t, svc)

			if stats.Inconclusive != 1 || settled(stats) != 0 {
				t.Fatalf("计数 = %+v，期望「问到了、没结论」", stats)
			}
			if len(repo.settleCalls) != 0 {
				t.Fatalf("结算被调了 %d 次，期望一次都不调", len(repo.settleCalls))
			}
		})
	}
}

// TestReconcileRefusesAnAmountThatDoesNotMatchThePayment 钉住金额那一道闸。
//
// 渠道回的金额与我们记的应付对不上，说明这份应答说的**不是这一笔**（老系统同样处置，见
// RecoverStuckCoffeeOrder:2021）。这时候宁可什么都不做：下一轮再问一次，代价是一分钟；落错
// 的代价是一笔金额不对的账，而且它看起来是成功的。
func TestReconcileRefusesAnAmountThatDoesNotMatchThePayment(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
		Result: provider.ResultSuccess,
		// 渠道说成了，但金额是另一个数（比如那是同一台设备上另一笔单）。
		ResponseSummary: map[string]any{"totalAmount": payment.Amount + 100},
	}}
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)

	stats := reconciled(t, svc)

	if stats.Inconclusive != 1 || stats.Succeeded != 0 {
		t.Fatalf("计数 = %+v，期望「仍然不明」", stats)
	}
	if len(repo.settleCalls) != 0 {
		t.Fatalf("金额对不上还是结算了 %d 次", len(repo.settleCalls))
	}
}

// TestReconcileAcceptsAMissingAmount 是上一条的反面：渠道没回金额时**不因此拒绝**。
//
// 判据与回调那条路逐字一致（见 repository.SettlePayment 里「两边都非空才比」那一段）：
// 要求查单应答必须带金额，等于要求渠道给一个它本来就可能不给的字段，而那会让一整族渠道
// 一笔都结不了。
func TestReconcileAcceptsAMissingAmount(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
		Result:          provider.ResultSuccess,
		ResponseSummary: map[string]any{"providerStatus": "TRADE_SUCCESS"},
	}}
	svc, _ := newReconcileFixture(t, []model.Payment{payment}, querier)

	if stats := reconciled(t, svc); stats.Succeeded != 1 {
		t.Fatalf("计数 = %+v，期望渠道没回金额时照样结算", stats)
	}
}

// TestReconcileLeavesThePaymentAloneWhenTheQueryDoesNotGoOut：一次出网就没通的查单，
// 不比没查更知道什么。
func TestReconcileLeavesThePaymentAloneWhenTheQueryDoesNotGoOut(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, err: errors.New("dial tcp: timeout")}
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)

	stats := reconciled(t, svc)

	if stats.Inconclusive != 1 || settled(stats) != 0 {
		t.Fatalf("计数 = %+v，期望「没问到」", stats)
	}
	if len(repo.settleCalls) != 0 {
		t.Fatalf("查单没发出去还是结算了 %d 次", len(repo.settleCalls))
	}
	// 失败也要留一行流水：这一笔问过、而且没问到，是排查时第一个要看的东西。
	if len(repo.providerCall) != 1 {
		t.Fatalf("渠道调用流水写了 %d 行，期望 1 行", len(repo.providerCall))
	}
}

// TestReconcileSkipsAProviderWithoutQuery：这一族的适配器没有查单接口不是故障，是它的形状
// （见 provider.Querier）。那一笔原样留在 pending，与引入这条任务之前的行为一致。
func TestReconcileSkipsAProviderWithoutQuery(t *testing.T) {
	payment := pendingPayment()
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, &stubProvider{})

	stats := reconciled(t, svc)

	if stats.Skipped != 1 || settled(stats) != 0 {
		t.Fatalf("计数 = %+v，期望「跳过 1 笔」", stats)
	}
	if len(repo.settleCalls) != 0 || len(repo.providerCall) != 0 {
		t.Error("没有查单接口的渠道上不该产生任何结算或调用流水")
	}
}

// TestReconcileDoesNotCountAnAlreadySettledPayment：状态早就到了（并发下另一个副本、或者那条
// 迟到的回调先落的手）。查单本身没做错什么，但它确实什么都没推进——不该记进「这一轮收了几笔」。
func TestReconcileDoesNotCountAnAlreadySettledPayment(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
		Result: provider.ResultSuccess,
	}}
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)
	already := &repository.PaymentSettlement{Payment: &payment, AlreadySettled: true}
	repo.settle = already

	stats := reconciled(t, svc)

	if stats.Succeeded != 0 || stats.Failed != 0 {
		t.Fatalf("计数 = %+v，期望「早就结过了」不计进推进", stats)
	}
}

// TestReconcileSurvivesASettleThatBlowsUp：结算失败（最典型的是这一笔已经被超时关单收走了，
// 而渠道说钱收了——那是真事故）既不能把这台机器打停，也不能被静默吞掉。
func TestReconcileSurvivesASettleThatBlowsUp(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
		Result: provider.ResultSuccess,
	}}
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)
	repo.settleErr = errors.New("payment is expired")

	stats := reconciled(t, svc)

	if stats.Succeeded != 0 || stats.Skipped != 1 {
		t.Fatalf("计数 = %+v，期望计进「没落成」", stats)
	}
}

// TestReconcileAsksForTheWindowAndBatchItWasGiven 钉住调用方给的那两个参数**原样**传给了仓储。
//
// 「多久没动过才算该问一句、一次最多问几笔」是 worker 那两个常量定的，仓储只负责按它筛。中间
// 任何一层自己改一个默认值，症状都是「查单比配置的慢」或者「一次问得比预期多」——两件都不会
// 报错的事。
func TestReconcileAsksForTheWindowAndBatchItWasGiven(t *testing.T) {
	payment := pendingPayment()
	querier := &stubQuerier{stubProvider: &stubProvider{}, result: provider.CreateResult{
		Result: provider.ResultUnknown,
	}}
	svc, repo := newReconcileFixture(t, []model.Payment{payment}, querier)

	window := time.Date(2026, 9, 14, 11, 55, 0, 0, time.UTC)
	if _, err := svc.ReconcilePendingPayments(context.Background(), window, 7); err != nil {
		t.Fatalf("ReconcilePendingPayments: %v", err)
	}

	if len(repo.staleQueries) != 1 {
		t.Fatalf("扫描被调了 %d 次，期望 1 次", len(repo.staleQueries))
	}
	if got := repo.staleQueries[0]; !got.staleBefore.Equal(window) || got.limit != 7 {
		t.Fatalf("扫描实参 = %+v，期望窗口 %v、批量 7", got, window)
	}
}
