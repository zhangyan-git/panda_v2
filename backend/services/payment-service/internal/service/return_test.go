package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组用例守的是结果页回跳那条路，而它只有两条判据值得钉：
//
//  1. **它不许写库**。回跳不携带任何新的收款事实（钱到没到早已由回调或查单决定过），
//     而它又是任何人都能打的一个无认证入口——写一次库就是一次可被诱导的副作用。
//  2. **它必须把查询串原样交下去**。验签盖的是渠道拼出来的那串参数，少一个字段、或者把
//     参数塞进 Body，适配器那边就是「验不过」，而用户看到的是一个「链接无效」的页面。

// readOnlyRepository 让**任何写方法**在被调用时立刻让测试失败。
//
// 它内嵌 service.Repository（于是四条读方法走的是那个假仓储），然后把十二个写方法逐个覆盖
// 成 t.Fatalf。**不内嵌的话**「回跳一行都不写」这句话只是一句注释；内嵌之后它是可执行的，
// 而且失败信息直接点名是哪个写方法。
//
// ⚠️ 给 Repository 加写方法时**必须在这里补一个**。少了那一个不会编译失败（内嵌的接口会
// 静默继承），于是那条新方法会走假仓储、写成功，而这条断言一声不吭——这是这个类型唯一的
// 失效方式，所以写在这里。
type readOnlyRepository struct {
	Repository
	t *testing.T
}

func (r readOnlyRepository) refuse(method string) {
	r.t.Helper()
	r.t.Fatalf("回跳路径调了 %s：这条路上不允许有任何写入（见 service.HandleReturn）", method)
}

func (r readOnlyRepository) BeginPayment(context.Context, repository.BeginPaymentParams) (*model.Payment, []byte, bool, error) {
	r.refuse("BeginPayment")
	return nil, nil, false, nil
}

func (r readOnlyRepository) AbandonPaymentAttempt(context.Context, string, string) error {
	r.refuse("AbandonPaymentAttempt")
	return nil
}

func (r readOnlyRepository) MarkPaymentPending(context.Context, repository.MarkPaymentPendingParams) (*model.Payment, error) {
	r.refuse("MarkPaymentPending")
	return nil, nil
}

func (r readOnlyRepository) MarkPaymentFailed(context.Context, repository.MarkPaymentFailedParams) (*model.Payment, error) {
	r.refuse("MarkPaymentFailed")
	return nil, nil
}

func (r readOnlyRepository) SettleAccountPayment(context.Context, repository.SettleAccountPaymentParams) (*model.Payment, error) {
	r.refuse("SettleAccountPayment")
	return nil, nil
}

func (r readOnlyRepository) RecordAccountDeduction(context.Context, string, string, time.Time) error {
	r.refuse("RecordAccountDeduction")
	return nil
}

func (r readOnlyRepository) RecordProviderCall(context.Context, repository.ProviderCallParams) error {
	r.refuse("RecordProviderCall")
	return nil
}

func (r readOnlyRepository) InsertNotification(context.Context, repository.NotificationParams) (*repository.NotificationRecord, error) {
	r.refuse("InsertNotification")
	return nil, nil
}

func (r readOnlyRepository) MarkNotification(context.Context, string, string, string) error {
	r.refuse("MarkNotification")
	return nil
}

func (r readOnlyRepository) SettlePayment(context.Context, repository.SettleNotificationParams) (*repository.PaymentSettlement, error) {
	r.refuse("SettlePayment")
	return nil, nil
}

func (r readOnlyRepository) ExpireOverduePayments(context.Context, int) (int, error) {
	r.refuse("ExpireOverduePayments")
	return 0, nil
}

func (r readOnlyRepository) ListStalePendingPayments(context.Context, time.Time, int) ([]model.Payment, error) {
	r.refuse("ListStalePendingPayments")
	return nil, nil
}

func (r readOnlyRepository) FindOverdueAccountFundedPayments(context.Context, int) ([]model.Payment, error) {
	r.refuse("FindOverdueAccountFundedPayments")
	return nil, nil
}

// newReturnService 组装回跳那条路的业务层。
//
// returnPageURL 直接进 Options（而不是先建好再改私有字段）：`{merOrderId}` 的替换规则、
// 以及「没配就是空串」这件事都是**装配契约**的一部分，用例要盖的就是它。
func newReturnService(t *testing.T, repo Repository, stub *stubProvider, returnPageURL string) *PaymentService {
	t.Helper()
	return New(repo, testCatalog(), provider.NewRegistry(stub),
		func(*catalog.Channel, string) string { return "comm-secret" }, nil, Options{
			NotifyBaseURL: "https://pay.example.test",
			ReturnPageURL: returnPageURL,
			Now:           func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) },
			NewPaymentNo:  func(time.Time) string { return "PAY20260920120000000001" },
		})
}

// returnRequest 造一条回跳请求。渠道码必须是**目录里真有的那一条**：回跳的第一步就是拿它
// 去目录里查渠道（见 HandleReturn），查不到会回 ErrChannelNotFound，而那与用例要验的东西
// 无关。路径也照真的那样拼。
func returnRequest() ReturnRequest {
	return ReturnRequest{
		ChannelCode: catalog.ChannelCodeUMS,
		Query:       url.Values{"merOrderId": {"PAY20260920120000000001"}, "signature": {"deadbeef"}},
		Headers:     http.Header{},
		RequestPath: "/v1/payments/return/" + catalog.ChannelCodeUMS,
	}
}

// TestHandleReturnVerifiesAndBuildsTheResultPage 是这条路的主干：验签过了就把用户送去结果页，
// 而结果页地址里的单号来自**验签之后**的报文。
func TestHandleReturnVerifiesAndBuildsTheResultPage(t *testing.T) {
	stub := &stubProvider{
		verifyOK:     true,
		notification: provider.Notification{EventType: "return", PaymentNo: "PAY20260920120000000001"},
	}
	repo := readOnlyRepository{Repository: &fakeRepository{}, t: t}
	svc := newReturnService(t, repo, stub, "https://h5.example.test/pay/result?no={merOrderId}")

	result, err := svc.HandleReturn(context.Background(), returnRequest())
	if err != nil {
		t.Fatalf("HandleReturn: %v", err)
	}
	if result.PaymentNo != "PAY20260920120000000001" {
		t.Fatalf("PaymentNo = %q", result.PaymentNo)
	}
	want := "https://h5.example.test/pay/result?no=PAY20260920120000000001"
	if result.RedirectURL != want {
		t.Fatalf("RedirectURL = %q, want %q", result.RedirectURL, want)
	}

	// 交给适配器的那一份必须是 GET + Query + 空 Body。写成 Body 也「能跑」（适配器可以自己
	// 去拼），但那会让验签对不上时错误串指着一个根本不存在的报文——在这一层钉死。
	if len(stub.verifyCalls) != 1 {
		t.Fatalf("Verify 调了 %d 次，期望 1 次", len(stub.verifyCalls))
	}
	sent := stub.verifyCalls[0]
	if sent.HTTPMethod != http.MethodGet {
		t.Errorf("HTTPMethod = %q, want GET", sent.HTTPMethod)
	}
	if len(sent.Body) != 0 {
		t.Errorf("Body = %q，回跳的参数只能走 Query", sent.Body)
	}
	if got := sent.Query.Get("signature"); got != "deadbeef" {
		t.Errorf("Query 没有原样传下去：signature = %q", got)
	}
	if sent.RequestPath != "/v1/payments/return/"+catalog.ChannelCodeUMS {
		t.Errorf("RequestPath = %q", sent.RequestPath)
	}
	// 凭据走的是既有那条路（按适配器声明的槽逐个解析），中途不能变成空串——空了适配器会拒签，
	// 而那看起来像「渠道签名错了」。取的是假适配器默认声明的那一格。
	if got := sent.Secrets.Get(stubSecretSlot); got != "comm-secret" {
		t.Errorf("凭据没有交下去：%q", got)
	}
}

// TestHandleReturnWithoutAResultPageStillVerifies 没配结果页时**照样验签**，只是不跳转。
//
// 这条与上一条是一对：少了它，一个「验签那步被短路掉、直接回 200」的实现也能过上面那条
// （因为上面的结果页恰好总是配着的）。
func TestHandleReturnWithoutAResultPageStillVerifies(t *testing.T) {
	stub := &stubProvider{
		verifyOK:     true,
		notification: provider.Notification{EventType: "return", PaymentNo: "PAY20260920120000000001"},
	}
	repo := readOnlyRepository{Repository: &fakeRepository{}, t: t}
	svc := newReturnService(t, repo, stub, "")

	result, err := svc.HandleReturn(context.Background(), returnRequest())
	if err != nil {
		t.Fatalf("HandleReturn: %v", err)
	}
	if result.RedirectURL != "" {
		t.Fatalf("没配结果页时 RedirectURL 应当为空，got %q", result.RedirectURL)
	}
	if len(stub.verifyCalls) != 1 {
		t.Fatalf("没配结果页时也必须验签，Verify 调了 %d 次", len(stub.verifyCalls))
	}
}

// TestHandleReturnTemplatesWithoutAPlaceholder 模板里没有占位符时原样返回。
//
// 运营配一个静态结果页是合法的（「支付已完成」这种页面本来就不需要单号），而它的表现与
// 「占位符写错了」（`{merOrderid}`）在这个函数里完全一样——都是原样返回。差别只在于页面上
// 会显示一串花括号。这不算错，所以这里钉住「不做任何额外处理」。
func TestHandleReturnTemplatesWithoutAPlaceholder(t *testing.T) {
	stub := &stubProvider{
		verifyOK:     true,
		notification: provider.Notification{EventType: "return", PaymentNo: "PAY-1"},
	}
	repo := readOnlyRepository{Repository: &fakeRepository{}, t: t}
	svc := newReturnService(t, repo, stub, "https://h5.example.test/pay/done")

	result, err := svc.HandleReturn(context.Background(), returnRequest())
	if err != nil {
		t.Fatalf("HandleReturn: %v", err)
	}
	if result.RedirectURL != "https://h5.example.test/pay/done" {
		t.Fatalf("RedirectURL = %q", result.RedirectURL)
	}
}

// TestHandleReturnRefusesAnythingThatDoesNotVerify 验签不过就拒，而且**什么都不写**。
//
// 与回调那条路刻意不同：回调拒收时落一行 payment_notifications(status=failed)，因为它会
// 重投、运维要看得到。回跳不会重投（用户看一次就走了），落库的收益只剩「有人戳这个地址时
// 留个痕」，而那件事 access log 已经在做。所以这里的断言是 readOnlyRepository——它会在
// 任何一次写调用上直接让测试失败。
func TestHandleReturnRefusesAnythingThatDoesNotVerify(t *testing.T) {
	// 零值 stubProvider 的 Verify 就是「签名不对」。
	stub := &stubProvider{}
	repo := readOnlyRepository{Repository: &fakeRepository{}, t: t}
	svc := newReturnService(t, repo, stub, "https://h5.example.test/pay/result?no={merOrderId}")

	_, err := svc.HandleReturn(context.Background(), returnRequest())
	if err == nil {
		t.Fatal("验签不过的回跳必须返回错误")
	}
	if !errors.Is(err, provider.ErrSignatureMismatch) {
		t.Fatalf("错误 = %v，期望 ErrSignatureMismatch", err)
	}
}

// TestHandleReturnRefusesASettlementEvent 适配器若把回跳归一化成了结算事件，必须拒。
//
// 这条今天不可达（ums 的 verifyReturn 只产出 "return"），它守的是**下一个协议族**：接回跳时
// 若顺手复用了回调那条归一化路径，返回的就是 succeeded / failed。那时我们要拿它去给用户
// 显示结果页——在一个连我们自己都说不清语义的输入上放行。
func TestHandleReturnRefusesASettlementEvent(t *testing.T) {
	stub := &stubProvider{
		verifyOK:     true,
		notification: provider.Notification{EventType: provider.EventSucceeded, PaymentNo: "PAY-1"},
	}
	repo := readOnlyRepository{Repository: &fakeRepository{}, t: t}
	svc := newReturnService(t, repo, stub, "https://h5.example.test/pay/result?no={merOrderId}")

	_, err := svc.HandleReturn(context.Background(), returnRequest())
	if err == nil {
		t.Fatal("被归一化成结算事件的回跳必须拒收")
	}
	if !errors.Is(err, provider.ErrInvalidNotification) {
		t.Fatalf("错误 = %v，期望 ErrInvalidNotification", err)
	}
}

// TestHandleReturnValidation 形状不对的请求在**碰仓储之前**就被挡住。
//
// 假仓储的 channel 是 nil，所以「没挡住」的表现是空指针 panic 而不是断言失败——这已经足够
// 说明它越过了那道检查。
func TestHandleReturnValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ReturnRequest)
		want   error
	}{
		{"缺渠道码", func(r *ReturnRequest) { r.ChannelCode = " " }, ErrChannelCodeRequired},
		{"查询串是空的", func(r *ReturnRequest) { r.Query = url.Values{} }, ErrReturnEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newReturnService(t, &fakeRepository{}, &stubProvider{}, "")
			req := returnRequest()
			tc.mutate(&req)
			if _, err := svc.HandleReturn(context.Background(), req); err != tc.want {
				t.Fatalf("错误 = %v, want %v", err, tc.want)
			}
		})
	}
}
