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

// 这一组用例守的是账户出资的补偿路径：**豆已经扣了、单还没结**的那些单。
//
// 它们是那两次写库（扣豆 / 结算）之间出的岔子留下的，钱已经动了。所以两条断言的方向是相反的：
// 能结的必须结掉（否则用户付了钱、订单却永远停在待支付），结不了**并且**这个结论已经确定的
// 必须落成一个说得清的终态（否则它每分钟被重扫一次、日志里反复出现同一行，而没有人知道该
// 去还哪一笔豆）。

// overdueFundedPayment 造一张「豆已经扣了、到点还没结」的单。
//
// 成交时间 (11:00) 与测试里那个可注入的 now (12:00) **刻意不同**：结算必须用扣豆那一刻，
// 拿处理请求的那一刻顶上，在一条走了几小时才被扫到的单上会差出几个小时。
func overdueFundedPayment(paymentID string) model.Payment {
	entryID := "entry-" + paymentID
	fundedAt := time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
	return model.Payment{
		ID: paymentID, PaymentNo: "PAY-" + paymentID, OrderNo: testOrderNo,
		UserID: testUserID, Amount: 1980,
		Status: model.PaymentCreated, PaymentMethod: catalog.CodeCoffeeBean,
		RequestID:      "req-" + paymentID,
		AccountEntryID: &entryID, AccountFundedAt: &fundedAt,
	}
}

// TestSettleOverdueAccountPaymentsSettlesWithTheDeductionTime 能结的必须结掉，而且记的是扣豆
// 那一刻的成交时间、带着那笔账变 ID。
//
// 账变 ID 是这条路上唯一能把一次结算指回账户域那笔扣减的东西，漏掉它等于把「钱从哪儿扣的」
// 又丢了一次——而这条补偿路径存在的全部理由就是别再丢它。
func TestSettleOverdueAccountPaymentsSettlesWithTheDeductionTime(t *testing.T) {
	payment := overdueFundedPayment("payment-overdue-1")
	repo := &fakeRepository{overdue: []model.Payment{payment}}
	svc := newBeanService(t, repo, &stubLedger{})

	settled, err := svc.SettleOverdueAccountPayments(context.Background(), 10)
	if err != nil {
		t.Fatalf("SettleOverdueAccountPayments error = %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1", settled)
	}
	if len(repo.accountSettles) != 1 {
		t.Fatalf("SettleAccountPayment called %d times, want 1", len(repo.accountSettles))
	}
	got := repo.accountSettles[0]
	if got.PaymentID != payment.ID {
		t.Fatalf("settled payment = %q, want %q", got.PaymentID, payment.ID)
	}
	if got.AccountEntryID != *payment.AccountEntryID {
		t.Fatalf("settled account entry = %q, want %q", got.AccountEntryID, *payment.AccountEntryID)
	}
	if !got.PaidAt.Equal(*payment.AccountFundedAt) {
		t.Fatalf("成交时间 = %v, want 扣豆那一刻 %v", got.PaidAt, *payment.AccountFundedAt)
	}
	// 幂等回放的快照必须是 succeeded：同一个 request_id 的调用方重试时，拿到的答复
	// 得与他第一次发起成功时拿到的一样。
	snapshot, ok := got.IdempotencyResponse.(*CreateResult)
	if !ok {
		t.Fatalf("idempotency response = %T, want *CreateResult", got.IdempotencyResponse)
	}
	if snapshot.Status != model.PaymentSucceeded || snapshot.Action != string(provider.ActionAccount) {
		t.Fatalf("replayed snapshot = %+v, want a succeeded account result", snapshot)
	}
	if got.RequestID != payment.RequestID {
		t.Fatalf("settled request id = %q, want %q", got.RequestID, payment.RequestID)
	}
	if len(repo.failedCalls) != 0 {
		t.Fatal("a payment that settled was also marked failed")
	}
}

// TestSettleOverdueAccountPaymentsConcludesAnUnsettleablePayment 结不了、且结论确定的，落成
// 一个说得清的终态，而不是留在原地每分钟重扫一次。
//
// 最常见的那种：同一张订单的另一张支付单先成了，payments_one_succeeded_per_order 把这一张
// 挡在外面。豆是在账户域独立扣走的，本地还不了（支付域对账户域只有扣减一个动作），所以能做
// 的只有把「哪一笔豆要还」写清楚、并且让它别再被反复选中。
func TestSettleOverdueAccountPaymentsConcludesAnUnsettleablePayment(t *testing.T) {
	payment := overdueFundedPayment("payment-overdue-1")
	repo := &fakeRepository{
		overdue:    []model.Payment{payment},
		accountErr: repository.ErrPaymentNotPending,
		// 读回来的当前状态还是 created：确认它确实没成，不是并发下已经被别处结算了。
		paymentByNo: &model.Payment{ID: payment.ID, Status: model.PaymentCreated},
	}
	svc := newBeanService(t, repo, &stubLedger{})

	settled, err := svc.SettleOverdueAccountPayments(context.Background(), 10)
	if err != nil {
		t.Fatalf("SettleOverdueAccountPayments error = %v, want nil", err)
	}
	if settled != 0 {
		t.Fatalf("settled = %d, want 0（它没结成功）", settled)
	}
	if len(repo.failedCalls) != 1 {
		t.Fatalf("MarkPaymentFailed called %d times, want 1", len(repo.failedCalls))
	}
	failed := repo.failedCalls[0]
	if failed.FailureCode != failureCodeAccountFundedNotSettled {
		t.Fatalf("failureCode = %q, want %q", failed.FailureCode, failureCodeAccountFundedNotSettled)
	}
	if failed.PaymentID != payment.ID {
		t.Fatalf("marked payment = %q, want %q", failed.PaymentID, payment.ID)
	}
}

// TestSettleOverdueAccountPaymentsLeavesAnAlreadySettledPaymentAlone 并发下另一条路刚把它结算
// 掉了，这时**什么都不能碰**——把一张已经成功的单标成 failed 是把一次真实的收款抹掉。
func TestSettleOverdueAccountPaymentsLeavesAnAlreadySettledPaymentAlone(t *testing.T) {
	payment := overdueFundedPayment("payment-overdue-1")
	repo := &fakeRepository{
		overdue:     []model.Payment{payment},
		accountErr:  repository.ErrPaymentNotPending,
		paymentByNo: &model.Payment{ID: payment.ID, Status: model.PaymentSucceeded},
	}
	svc := newBeanService(t, repo, &stubLedger{})

	if _, err := svc.SettleOverdueAccountPayments(context.Background(), 10); err != nil {
		t.Fatalf("SettleOverdueAccountPayments error = %v", err)
	}
	if len(repo.failedCalls) != 0 {
		t.Fatal("一张已经成功的单被标成了 failed")
	}
}

// TestSettleOverdueAccountPaymentsKeepsGoingAfterOneFailure 一条坏的不能挡住同一批里其它好的。
//
// 一批里混着「另一张支付单已经收过钱了」的单是常态，让它在队列头部把整批卡住，等于后面所有
// 本该结算的单都跟着停在待支付。
func TestSettleOverdueAccountPaymentsKeepsGoingAfterOneFailure(t *testing.T) {
	broken := overdueFundedPayment("payment-broken")
	healthy := overdueFundedPayment("payment-healthy")
	repo := &fakeRepository{
		overdue:      []model.Payment{broken, healthy},
		accountErrOn: map[string]error{broken.ID: errors.New("the database is unreachable")},
	}
	svc := newBeanService(t, repo, &stubLedger{})

	settled, err := svc.SettleOverdueAccountPayments(context.Background(), 10)
	if err != nil {
		t.Fatalf("SettleOverdueAccountPayments error = %v, want nil", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1（只有 healthy 那条能结）", settled)
	}
	if len(repo.accountSettles) != 1 || repo.accountSettles[0].PaymentID != healthy.ID {
		t.Fatalf("结算的是 %+v, want 只有 %s", repo.accountSettles, healthy.ID)
	}
	// 真故障（库不可达）**不是**结论：留给下一轮，不许标 failed。
	if len(repo.failedCalls) != 0 {
		t.Fatal("一次库故障被当成了一次业务结论")
	}
}

// TestSettleOverdueAccountPaymentsReportsFinderFailures 选批失败要报出去——worker 靠这个错误
// 记日志；吞掉它，积压就变成了一件从外部完全看不见的事。
func TestSettleOverdueAccountPaymentsReportsFinderFailures(t *testing.T) {
	repo := &fakeRepository{overdueErr: errors.New("the database is unreachable")}
	svc := newBeanService(t, repo, &stubLedger{})

	if _, err := svc.SettleOverdueAccountPayments(context.Background(), 10); err == nil {
		t.Fatal("SettleOverdueAccountPayments error = nil, want the finder failure")
	}
}
