package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组测试覆盖发起支付里**不碰渠道网络**的那部分：形状校验、分派前的拒绝、以及三种
// 渠道结论（成功 / 拒绝 / 不确定）各自对支付单做什么。它们都用假仓储，因此跑得快、也不
// 需要 PG——这一层里最容易写错的恰恰是这些判断（金额该不该建单、结果不明时该不该标失败），
// 而不是 SQL 本身。
//
// 渠道网络那一段用 stubProvider 顶替：它直接返回一个预设的 provider.CreateResult，
// 就像适配器从渠道那里拿到的一样。真实渠道的协议由 provider/manual 的测试守。

const (
	testUserID   = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	testMethodID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	testChannel  = "c1a1e6b0-9f6e-4a8f-8f4f-0c2b8a1d5e77"
	testOrderID  = "5c2f2b5a-3d1c-4a6e-9b0f-8e7d6c5b4a39"
	testOrderNo  = "ORD20260914000001"
	testRequest  = "req-1"
)

// fakeRepository 是一个够用的假仓储：它只记下「被调用了哪些方法、参数是什么」，
// 并按预设返回。没有并发保护——这些测试都是单 goroutine 的。
type fakeRepository struct {
	method        *repository.PaymentMethodWithChannel
	methodErr     error
	methodLookups int
	// notification / notificationErr 编排 InsertNotification 的回答，供回调那几条用例
	// 模拟「这条通知已经在库里了」。
	notification    *repository.NotificationRecord
	notificationErr error
	channel         *repository.ChannelRecord
	channelErr      error

	beginPayment *model.Payment
	beginSnap    []byte
	beginHit     bool
	beginErr     error

	pendingCalls []repository.MarkPaymentPendingParams
	failedCalls  []repository.MarkPaymentFailedParams
	providerCall []repository.ProviderCallParams

	settle    *repository.PaymentSettlement
	settleErr error

	accountSettles []repository.SettleAccountPaymentParams
	accountErr     error
	// accountErrOn 按 paymentID 编排 SettleAccountPayment 的失败：命中就返回那一个错误。
	// 用来在同一批补偿里造出「一条坏、一条好」，验证坏的不挡住好的。
	accountErrOn map[string]error

	// —— 账户出资的扣减留痕与补偿（见 createAccountPayment、expire.go）——
	deductions   []accountDeduction
	deductionErr error
	// overdue 是 FindOverdueAccountFundedPayments 的回答：一批「豆已经扣了、到点还没结算」
	// 的支付单。
	overdue    []model.Payment
	overdueErr error
	// paymentByNo 编排 FindPaymentByNo 的回答。零值保持原来的「查不到」。
	paymentByNo *model.Payment
	// failedErr 编排 MarkPaymentFailed 的回答。零值保持「写成功了」。
	failedErr error
}

// accountDeduction 是一次 RecordAccountDeduction 调用的实参。
//
// fundedAt 也记下来：结算用的成交时间必须**等于**扣豆那一刻，而不是结算那一刻（见
// createAccountPayment），只记 paymentID 与 entryID 的话这条断言就没法写。
type accountDeduction struct {
	paymentID string
	entryID   string
	fundedAt  time.Time
}

func (f *fakeRepository) FindPaymentMethod(context.Context, string) (*repository.PaymentMethodWithChannel, error) {
	f.methodLookups++
	if f.methodErr != nil {
		return nil, f.methodErr
	}
	return f.method, nil
}

func (f *fakeRepository) FindChannelByCode(context.Context, string) (*repository.ChannelRecord, error) {
	if f.channelErr != nil {
		return nil, f.channelErr
	}
	return f.channel, nil
}

func (f *fakeRepository) FindPaymentByNo(context.Context, string) (*model.Payment, error) {
	if f.paymentByNo != nil {
		return f.paymentByNo, nil
	}
	return nil, repository.ErrPaymentNotFound
}

func (f *fakeRepository) BeginPayment(_ context.Context, p repository.BeginPaymentParams) (*model.Payment, []byte, bool, error) {
	if f.beginErr != nil {
		return nil, nil, false, f.beginErr
	}
	if f.beginHit {
		return nil, f.beginSnap, true, nil
	}
	payment := f.beginPayment
	if payment == nil {
		payment = &model.Payment{
			ID: "payment-1", PaymentNo: p.PaymentNo, OrderNo: p.OrderNo, UserID: p.UserID,
			Amount: p.Amount, FundingType: p.FundingType, Status: model.PaymentCreated,
			RequestID: p.RequestID,
		}
	}
	return payment, nil, false, nil
}

func (f *fakeRepository) MarkPaymentPending(_ context.Context, p repository.MarkPaymentPendingParams) (*model.Payment, error) {
	f.pendingCalls = append(f.pendingCalls, p)
	return &model.Payment{ID: p.PaymentID, PaymentNo: "PAY-FAKE", Status: model.PaymentPending}, nil
}

func (f *fakeRepository) MarkPaymentFailed(_ context.Context, p repository.MarkPaymentFailedParams) (*model.Payment, error) {
	f.failedCalls = append(f.failedCalls, p)
	if f.failedErr != nil {
		return nil, f.failedErr
	}
	return &model.Payment{ID: p.PaymentID, Status: model.PaymentFailed}, nil
}

func (f *fakeRepository) RecordProviderCall(_ context.Context, p repository.ProviderCallParams) error {
	f.providerCall = append(f.providerCall, p)
	return nil
}

func (f *fakeRepository) InsertNotification(context.Context, repository.NotificationParams) (*repository.NotificationRecord, error) {
	// 零值保持「新到的一条」：建单那几条用例看不到这个开关。
	if f.notificationErr != nil {
		return nil, f.notificationErr
	}
	if f.notification != nil {
		return f.notification, nil
	}
	return &repository.NotificationRecord{ID: "n-1", Inserted: true}, nil
}

func (f *fakeRepository) MarkNotification(context.Context, string, string, string) error { return nil }

func (f *fakeRepository) SettlePayment(context.Context, repository.SettleNotificationParams) (*repository.PaymentSettlement, error) {
	if f.settleErr != nil {
		return nil, f.settleErr
	}
	return f.settle, nil
}

func (f *fakeRepository) ExpireOverduePayments(context.Context, int) (int, error) { return 0, nil }

func (f *fakeRepository) SettleAccountPayment(_ context.Context, p repository.SettleAccountPaymentParams) (*model.Payment, error) {
	if err := f.accountErrOn[p.PaymentID]; err != nil {
		return nil, err
	}
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	f.accountSettles = append(f.accountSettles, p)
	return &model.Payment{ID: p.PaymentID, Status: model.PaymentSucceeded}, nil
}

func (f *fakeRepository) RecordAccountDeduction(_ context.Context, paymentID, accountEntryID string, fundedAt time.Time) error {
	if f.deductionErr != nil {
		return f.deductionErr
	}
	f.deductions = append(f.deductions, accountDeduction{paymentID: paymentID, entryID: accountEntryID, fundedAt: fundedAt})
	return nil
}

func (f *fakeRepository) FindOverdueAccountFundedPayments(context.Context, int) ([]model.Payment, error) {
	if f.overdueErr != nil {
		return nil, f.overdueErr
	}
	return f.overdue, nil
}

// stubProvider 是一个可编排的适配器：Create 原样返回预设结果，Verify 永不通过。
type stubProvider struct {
	result  provider.CreateResult
	err     error
	calls   []provider.CreateRequest
	ackTrue provider.Ack
	// verifyOK 打开后 Verify 返回 notification/verifyErr 而不是默认的验签失败。
	verifyOK     bool
	notification provider.Notification
	verifyErr    error
}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Create(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	s.calls = append(s.calls, req)
	return s.result, s.err
}

func (s *stubProvider) Verify(context.Context, provider.NotificationRequest) (provider.Notification, error) {
	// 零值保持「验签不过」：只有回调那几条用例会把这个开关打开。
	if !s.verifyOK {
		return provider.Notification{}, provider.ErrSignatureMismatch
	}
	return s.notification, s.verifyErr
}

func (s *stubProvider) Ack(accepted bool) provider.Ack {
	if accepted {
		return s.ackTrue
	}
	return provider.Ack{Status: 400}
}

// stubLedger 是可编排的假账本：Deduct 原样返回预设结果，并记下请求，让「扣的是不是这张
// 订单、这个金额、这个用户」变成可断言的。
type stubLedger struct {
	result client.DeductResult
	err    error
	calls  []client.DeductRequest
}

func (s *stubLedger) Deduct(_ context.Context, req client.DeductRequest) (client.DeductResult, error) {
	s.calls = append(s.calls, req)
	return s.result, s.err
}

// newTestService 组装一个「支付方式指向 stub 适配器」的业务层，不带账户域。
func newTestService(t *testing.T, repo *fakeRepository, stub *stubProvider, action string) *PaymentService {
	t.Helper()
	return newTestServiceWith(t, repo, stub, action, nil)
}

// newBeanService 组装账户出资那条路的业务层：支付方式 action=account（没有渠道行），
// 账本是传入的那个假账本。
func newBeanService(t *testing.T, repo *fakeRepository, ledger BeanLedger) *PaymentService {
	t.Helper()
	return newTestServiceWith(t, repo, &stubProvider{}, model.ActionAccount, ledger)
}

func newTestServiceWith(t *testing.T, repo *fakeRepository, stub *stubProvider, action string, beans BeanLedger) *PaymentService {
	t.Helper()
	channel := &model.PaymentChannel{
		ID: testChannel, Code: "stub_dev", Provider: "stub", Mode: "sandbox",
		Status: model.ChannelEnabled, SecretRef: "TEST_SECRET",
	}
	var channelID *string
	if action != model.ActionAccount {
		channelID = &channel.ID
	}
	repo.method = &repository.PaymentMethodWithChannel{
		Method: model.PaymentMethod{
			ID: testMethodID, Code: "stub_pay", Action: action,
			FundingType: model.FundingWechat, Status: model.MethodEnabled,
			ChannelID: channelID,
		},
		Channel:       channel,
		MethodParams:  map[string]string{},
		ChannelConfig: map[string]string{},
	}
	return New(repo, provider.NewRegistry(stub), func(string) string { return "secret" }, beans, Options{
		NotifyBaseURL: "https://pay.example.test",
		Now:           func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) },
		NewPaymentNo:  func(time.Time) string { return "PAY20260914120000000001" },
	})
}

func validCreateRequest() CreateRequest {
	return CreateRequest{
		OrderID: testOrderID, OrderNo: testOrderNo, UserID: testUserID, Amount: 1980,
		PaymentMethodID: testMethodID, RequestID: testRequest,
		WalletOpenID: "openid-1", Subject: "拿铁",
	}
}

// TestCreatePaymentValidation 校验是形状层面的：一条都不能走到仓储。
//
// 「没走到仓储」是断言的一部分，不是顺带的：一条建不了单的请求在库上留下任何痕迹都是错的。
func TestCreatePaymentValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateRequest)
		want   error
	}{
		{"缺订单 ID", func(r *CreateRequest) { r.OrderID = "" }, ErrOrderIDRequired},
		{"订单 ID 不是 uuid", func(r *CreateRequest) { r.OrderID = "o-1" }, ErrOrderIDInvalid},
		{"缺订单号", func(r *CreateRequest) { r.OrderNo = " " }, ErrOrderNoRequired},
		{"缺用户", func(r *CreateRequest) { r.UserID = "" }, ErrUserIDRequired},
		{"用户不是 uuid", func(r *CreateRequest) { r.UserID = "u-1" }, ErrUserIDInvalid},
		{"金额为 0", func(r *CreateRequest) { r.Amount = 0 }, ErrAmountNotPositive},
		{"金额为负", func(r *CreateRequest) { r.Amount = -1 }, ErrAmountNotPositive},
		{"缺支付方式", func(r *CreateRequest) { r.PaymentMethodID = " " }, ErrMethodIDRequired},
		{"支付方式不是 uuid", func(r *CreateRequest) { r.PaymentMethodID = "m-1" }, ErrMethodIDInvalid},
		{"缺幂等号", func(r *CreateRequest) { r.RequestID = "" }, ErrRequestIDRequired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{}
			svc := newTestService(t, repo, &stubProvider{}, model.ActionNativePay)
			in := validCreateRequest()
			tc.mutate(&in)
			if _, err := svc.CreatePayment(context.Background(), in); !errors.Is(err, tc.want) {
				t.Fatalf("CreatePayment error = %v, want %v", err, tc.want)
			}
			if repo.methodLookups != 0 {
				t.Fatal("invalid input reached the payment method lookup")
			}
		})
	}
}

// TestCreateAccountPaymentDeductsAndSettles 账户出资的成功路径：扣豆 → 一次事务结算。
//
// 断言的三件事分别对应这条路上最容易写错的三处：扣的是**订单 ID**（不是支付单号，
// 那是幂等键的来源）、出资行的账变 ID 被原样带进结算、以及返回的是 succeeded 而不是
// pending（没有第三方要等）。
func TestCreateAccountPaymentDeductsAndSettles(t *testing.T) {
	repo := &fakeRepository{}
	ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
	svc := newBeanService(t, repo, ledger)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment error = %v", err)
	}
	if result.Status != model.PaymentSucceeded {
		t.Fatalf("status = %q, want %q", result.Status, model.PaymentSucceeded)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("Deduct called %d times, want 1", len(ledger.calls))
	}
	call := ledger.calls[0]
	if call.OrderID != testOrderID {
		t.Fatalf("Deduct orderID = %q, want %q", call.OrderID, testOrderID)
	}
	if call.Amount != 1980 || call.UserID != testUserID {
		t.Fatalf("Deduct got user %q amount %d, want %q / 1980", call.UserID, call.Amount, testUserID)
	}
	if len(repo.accountSettles) != 1 {
		t.Fatalf("SettleAccountPayment called %d times, want 1", len(repo.accountSettles))
	}
	if got := repo.accountSettles[0].AccountEntryID; got != "entry-1" {
		t.Fatalf("settled account entry = %q, want entry-1", got)
	}
	if len(repo.providerCall) != 0 {
		t.Fatal("an account payment recorded a provider call; there is no provider")
	}
	if len(repo.pendingCalls) != 0 {
		t.Fatal("an account payment went through MarkPaymentPending; it never waits on a third party")
	}
}

// TestCreateAccountPaymentRecordsTheDeductionBeforeSettling 守「扣豆成功先留痕」。
//
// 这条留痕要覆盖的是**结算失败**那个窗口：豆在账户域是独立提交的，结算一旦失败，
// payment_fundings 那一行会随事务回滚，本地就再没有任何东西指向账户域那笔账变。所以断言
// 分两半，缺一不可：成功的路上留痕存在，且成交时间与结算用的是同一个值（两处记的时间不
// 一样，对账时就得猜哪一个是钱真正走掉的时刻）；失败的路上留痕**依然在**——那才是它存在
// 的全部理由，只测成功那条等于什么都没守住。
func TestCreateAccountPaymentRecordsTheDeductionBeforeSettling(t *testing.T) {
	t.Run("结算成功时两边的时间一致", func(t *testing.T) {
		repo := &fakeRepository{}
		ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
		svc := newBeanService(t, repo, ledger)

		if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err != nil {
			t.Fatalf("CreatePayment error = %v", err)
		}
		if len(repo.deductions) != 1 {
			t.Fatalf("RecordAccountDeduction called %d times, want 1", len(repo.deductions))
		}
		recorded := repo.deductions[0]
		if recorded.entryID != "entry-1" || recorded.paymentID != "payment-1" {
			t.Fatalf("recorded %+v, want entry-1 on payment-1", recorded)
		}
		if len(repo.accountSettles) != 1 {
			t.Fatalf("SettleAccountPayment called %d times, want 1", len(repo.accountSettles))
		}
		if got := repo.accountSettles[0].PaidAt; !got.Equal(recorded.fundedAt) {
			t.Fatalf("结算用的成交时间 = %v，留痕记的是 %v，两者必须相等", got, recorded.fundedAt)
		}
	})

	t.Run("结算失败时留痕仍然落下", func(t *testing.T) {
		repo := &fakeRepository{accountErr: errors.New("the ledger write failed")}
		ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
		svc := newBeanService(t, repo, ledger)

		if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
			t.Fatal("CreatePayment error = nil, want the settle failure")
		}
		// 这一条就是这次修复本身：豆已经扣了，结算没成，但本地必须留下指向那笔账变的线索。
		if len(repo.deductions) != 1 {
			t.Fatalf("结算失败时留痕丢了（RecordAccountDeduction 调用 %d 次）——豆扣走了，本地却什么都没留下",
				len(repo.deductions))
		}
	})
}

// TestCreateAccountPaymentSettlesEvenIfTheDeductionCannotBeRecorded 留痕写不下去不该挡住结算。
//
// 结算那个事务自己会把 account_entry_id 写进 payment_fundings，那才是权威的出资留痕；
// 这一步要覆盖的只是结算失败的那个窗口。为了它把一次本来能成的收款推回去，是把轻重搞反了。
func TestCreateAccountPaymentSettlesEvenIfTheDeductionCannotBeRecorded(t *testing.T) {
	repo := &fakeRepository{deductionErr: errors.New("the ledger write failed")}
	ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
	svc := newBeanService(t, repo, ledger)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment error = %v, want the payment to go through anyway", err)
	}
	if result.Status != model.PaymentSucceeded {
		t.Fatalf("status = %q, want %q", result.Status, model.PaymentSucceeded)
	}
	if len(repo.accountSettles) != 1 {
		t.Fatalf("SettleAccountPayment called %d times, want 1", len(repo.accountSettles))
	}
}

// TestCreateAccountPaymentInsufficientBeansIsADecline 豆不够是**结论**，不是故障。
//
// 它落成 status='failed' 的结果（客户端可以换一种方式重试），并且**不能**碰结算——
// 把一张没扣到钱的支付单推成 succeeded 是这条路线上最严重的一种错。
func TestCreateAccountPaymentInsufficientBeansIsADecline(t *testing.T) {
	repo := &fakeRepository{}
	ledger := &stubLedger{err: fmt.Errorf("%w: user %s", ErrInsufficientCoffeeBeans, testUserID)}
	svc := newBeanService(t, repo, ledger)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment error = %v, want a failed result instead", err)
	}
	if result.Status != model.PaymentFailed {
		t.Fatalf("status = %q, want %q", result.Status, model.PaymentFailed)
	}
	if result.FailureCode != failureCodeInsufficientBeans {
		t.Fatalf("failureCode = %q, want %q", result.FailureCode, failureCodeInsufficientBeans)
	}
	if len(repo.accountSettles) != 0 {
		t.Fatal("a declined account payment was settled anyway")
	}
	if len(repo.failedCalls) != 1 {
		t.Fatalf("MarkPaymentFailed called %d times, want 1", len(repo.failedCalls))
	}
}

// TestCreateAccountPaymentAccountUnreachableLeavesPaymentCreated 账户域不可达时**得不出结论**。
//
// 支付单必须停在 created（由超时关单收走），调用方拿到一个普通错误——**不是**一次
// status='failed' 的结论。标成 failed 会让用户以为没付成，而豆可能已经扣了。
func TestCreateAccountPaymentAccountUnreachableLeavesPaymentCreated(t *testing.T) {
	repo := &fakeRepository{}
	ledger := &stubLedger{err: errors.New("account service is unreachable")}
	svc := newBeanService(t, repo, ledger)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("CreatePayment error = nil, want a plain error")
	}
	if len(repo.failedCalls) != 0 || len(repo.accountSettles) != 0 {
		t.Fatal("an unreachable account service produced a payment outcome")
	}
	if len(repo.pendingCalls) != 0 {
		t.Fatal("an unreachable account service advanced the payment")
	}
}

// TestCreateAccountPaymentWithoutLedgerFails 没接账户域的部署选了豆支付时明确失败，
// 而不是跳过扣豆直接结算——那等于白送一单。
func TestCreateAccountPaymentWithoutLedgerFails(t *testing.T) {
	repo := &fakeRepository{}
	svc := newBeanService(t, repo, nil)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("CreatePayment error = nil, want a failure when the ledger is not configured")
	}
	if len(repo.accountSettles) != 0 {
		t.Fatal("a payment was settled without any ledger to deduct from")
	}
}

// TestCreatePaymentRejectsUnknownAction 库里读到一个代码不认识的 action 时明确报错。
//
// 数据库的 CHECK 挡得住写坏的数据，但挡不住「代码回滚到不认这个 action 的版本」——
// 那时静默走 default 会把一次配置错误变成一次看不懂的失败。
func TestCreatePaymentRejectsUnknownAction(t *testing.T) {
	repo := &fakeRepository{}
	svc := newTestService(t, repo, &stubProvider{}, "some_future_action")
	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); !errors.Is(err, ErrUnauthorizedAction) {
		t.Fatalf("CreatePayment error = %v, want ErrUnauthorizedAction", err)
	}
}

// TestCreatePaymentUnregisteredProviderIsExplicit 渠道行指向一个没注册的 provider 时
// 是一次**明确的失败**，不是静默降级。
func TestCreatePaymentUnregisteredProviderIsExplicit(t *testing.T) {
	repo := &fakeRepository{}
	svc := newTestService(t, repo, &stubProvider{}, model.ActionNativePay)
	// 渠道行说自己是 "wechat"，而注册表里只有 "stub"。
	repo.method.Channel.Provider = "wechat"
	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("CreatePayment error = %v, want ErrProviderNotConfigured", err)
	}
}

// TestCreatePaymentSuccess 成功那一段：推进到 pending、落一条出资行、写幂等快照。
func TestCreatePaymentSuccess(t *testing.T) {
	stub := &stubProvider{result: provider.CreateResult{
		Result:                provider.ResultSuccess,
		ProviderTransactionID: "STUB-1",
		PayParams:             map[string]string{"payUrl": "https://pay.example.test/x"},
	}}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, model.ActionH5)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if result.Status != model.PaymentPending || result.Action != model.ActionH5 {
		t.Fatalf("result = %+v, want status pending with action h5", result)
	}
	if result.ExpiresAtUnix == 0 {
		t.Error("ExpiresAtUnix is 0; 客户端拿不到超时就没法倒计时")
	}
	if len(repo.pendingCalls) != 1 {
		t.Fatalf("MarkPaymentPending called %d times, want 1", len(repo.pendingCalls))
	}
	pending := repo.pendingCalls[0]
	if len(pending.FundingLines) != 1 || pending.FundingLines[0].Amount != 1980 {
		t.Fatalf("funding lines = %+v, want one line of 1980", pending.FundingLines)
	}
	// 快照必须与回给客户端的响应是同一份字节，否则第二次点支付拿到的参数会不一样。
	snapshot, err := json.Marshal(pending.IdempotencyResponse)
	if err != nil {
		t.Fatalf("marshal idempotency snapshot: %v", err)
	}
	var fromSnapshot CreateResult
	if err := json.Unmarshal(snapshot, &fromSnapshot); err != nil {
		t.Fatalf("unmarshal idempotency snapshot: %v", err)
	}
	if fromSnapshot.PaymentNo != result.PaymentNo || fromSnapshot.Status != result.Status {
		t.Errorf("snapshot = %+v, want the same result the caller got (%+v)", fromSnapshot, result)
	}
	// 渠道调用流水成败都要有。
	if len(repo.providerCall) != 1 || repo.providerCall[0].Result != string(provider.ResultSuccess) {
		t.Fatalf("provider calls = %+v, want one success record", repo.providerCall)
	}
	if got := stub.calls[0].NotifyURL; got != "https://pay.example.test/v1/payments/callback/stub_dev" {
		t.Errorf("NotifyURL = %q", got)
	}
	// walletOpenId 由服务端写入，调用方不能从 attach 里覆盖它。
	if got := stub.calls[0].Attach["walletOpenId"]; got != "openid-1" {
		t.Errorf("attach walletOpenId = %q, want openid-1", got)
	}
}

// TestCreatePaymentDeclinedIsAResultNotAnError 渠道明确拒绝返回的是**结果**，不是错误。
//
// 这是与 gRPC 契约对齐的那一条：failed = 发起即失败，客户端可以换方式重试。返回 error 会
// 让 order-service 把一次正常的业务拒绝当成服务端故障去重试。
func TestCreatePaymentDeclinedIsAResultNotAnError(t *testing.T) {
	stub := &stubProvider{result: provider.CreateResult{
		Result: provider.ResultFailed, FailureCode: "RISK_REJECTED", FailureMessage: "风控拒绝",
	}}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, model.ActionH5)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("a declined payment must not be an error, got %v", err)
	}
	if result.Status != model.PaymentFailed || result.FailureCode != "RISK_REJECTED" {
		t.Fatalf("result = %+v, want status failed with the provider's code", result)
	}
	if len(repo.failedCalls) != 1 || len(repo.pendingCalls) != 0 {
		t.Fatalf("failed=%d pending=%d, want exactly one failed write and no pending write",
			len(repo.failedCalls), len(repo.pendingCalls))
	}
}

// TestCreatePaymentDeclinedWithoutCodeGetsOne 适配器没说为什么拒的，也要留一个非空的码。
//
// 空的 failure_code 在后台列表里看起来像「没失败」，而这一单确实失败了。
func TestCreatePaymentDeclinedWithoutCodeGetsOne(t *testing.T) {
	stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultFailed}}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, model.ActionH5)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if result.FailureCode == "" {
		t.Fatal("a declined payment was recorded without a failure code")
	}
}

// TestCreatePaymentUncertainResultLeavesPaymentInCreated 结果不明时**什么都不改**。
//
// 这是整个发起支付里最反直觉的一条，也是最重要的：超时或结果未知时渠道侧可能已经有一张
// 能付的预支付单。把本地单标成 failed，就会出现「用户真把那笔钱付了，而我们这边是一张
// 已失败的单」。所以支付单停在 created，由超时关单扫描收走。
func TestCreatePaymentUncertainResultLeavesPaymentInCreated(t *testing.T) {
	for _, result := range []provider.Result{provider.ResultTimeout, provider.ResultUnknown, ""} {
		t.Run(string(result)+"", func(t *testing.T) {
			stub := &stubProvider{result: provider.CreateResult{Result: result}}
			repo := &fakeRepository{}
			svc := newTestService(t, repo, stub, model.ActionH5)

			_, err := svc.CreatePayment(context.Background(), validCreateRequest())
			if !errors.Is(err, ErrProviderResultUncertain) {
				t.Fatalf("error = %v, want ErrProviderResultUncertain", err)
			}
			if len(repo.failedCalls) != 0 || len(repo.pendingCalls) != 0 {
				t.Fatalf("failed=%d pending=%d, want the payment left untouched in created",
					len(repo.failedCalls), len(repo.pendingCalls))
			}
			// 流水仍然要记：它是事后判断「到底发生了什么」的唯一依据。
			if len(repo.providerCall) != 1 {
				t.Fatalf("provider calls = %d, want 1", len(repo.providerCall))
			}
		})
	}
}

// TestCreatePaymentProviderCallFailureStaysInCreated 调用根本没发出去时同样停在 created。
//
// 与「结果不明」走同一条收尾，但错误不同：这是我们自己的问题（密钥没配、参数拼不出来），
// 不是一个渠道结论。
func TestCreatePaymentProviderCallFailureStaysInCreated(t *testing.T) {
	stub := &stubProvider{err: errors.New("secret is missing")}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, model.ActionH5)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("a provider call that never went out must surface an error")
	}
	if len(repo.failedCalls) != 0 || len(repo.pendingCalls) != 0 {
		t.Fatalf("failed=%d pending=%d, want the payment left untouched in created",
			len(repo.failedCalls), len(repo.pendingCalls))
	}
}

// TestCreatePaymentReplaysRecordedSnapshot 同一个 request_id 再调时原样回放上次的结论。
//
// 成功与失败的回放走同一条路：客户端第二次点支付拿到的必须是同一个答复，而不是「上次被拒、
// 这次又去问了一遍渠道」。
func TestCreatePaymentReplaysRecordedSnapshot(t *testing.T) {
	previous := CreateResult{
		PaymentNo: "PAY20260914120000000001", Status: model.PaymentFailed,
		FailureCode: "RISK_REJECTED", FailureMessage: "风控拒绝",
	}
	snapshot, err := json.Marshal(previous)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}
	repo := &fakeRepository{beginHit: true, beginSnap: snapshot}
	svc := newTestService(t, repo, stub, model.ActionH5)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	// 用 DeepEqual 而不是 ==：CreateResult 里有 PayParams 这个 map，结构体不可比较。
	if !reflect.DeepEqual(*result, previous) {
		t.Fatalf("replayed result = %+v, want %+v", *result, previous)
	}
	// 最重要的一条：回放**不去碰渠道**。否则「重试」就变成真的又发起了一笔支付。
	if len(stub.calls) != 0 {
		t.Fatalf("the provider was called %d times during a replay, want 0", len(stub.calls))
	}
	if len(repo.pendingCalls) != 0 || len(repo.failedCalls) != 0 {
		t.Error("a replay must not write any payment state")
	}
}

// TestCreatePaymentEmptySnapshotIsAnError 幂等行在但快照是空的：报错让人来查。
//
// 只可能是一行被标成 succeeded 却没写 response 的脏数据。在这里猜一个支付参数给客户端
// 是最坏的选择——客户端会拿一份凭空造出来的参数去调起支付。
func TestCreatePaymentEmptySnapshotIsAnError(t *testing.T) {
	repo := &fakeRepository{beginHit: true, beginSnap: nil}
	svc := newTestService(t, repo, &stubProvider{}, model.ActionH5)
	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("an idempotency row without a recorded response must be an error")
	}
}

// TestCreateRequestHashIgnoresPresentationFields 幂等哈希只认业务字段。
//
// 判据是「改了它，还算不算同一笔支付」：subject 与 attach 是展示与渠道附加数据，改了它们
// 不该让一次重试变成「同一个 key 换了请求体」的冲突；而金额变了**必须**冲突。
func TestCreateRequestHashIgnoresPresentationFields(t *testing.T) {
	base := validCreateRequest()

	presentational := base
	presentational.Subject = "换了描述"
	presentational.Attach = map[string]string{"deviceNo": "D-1"}
	presentational.WalletOpenID = "openid-2"
	presentational.TraceID = "trace-2"
	if createRequestHash(base) != createRequestHash(presentational) {
		t.Error("subject / attach / walletOpenId / traceId must not change the idempotency hash")
	}

	business := base
	business.Amount = 1981
	if createRequestHash(base) == createRequestHash(business) {
		t.Error("a different amount must produce a different idempotency hash")
	}
}
