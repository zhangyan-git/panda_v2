package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// 这一层测的是**发起支付前的那几道判断**：归属、状态、金额、期限。它们全是「不碰网络也
// 判得了」的事实，所以能在这里测；而它们也正是这条路最容易出错的地方——判松一道，钱就
// 收在一个不该付钱的单上。
//
// 内嵌 Repository 同 after_sale_test.go：只实现本文件真正走到的方法，走到别的会 nil panic。

type fakePayRepo struct {
	Repository

	order *model.Order
	err   error
	// lookups 计的是按订单号读取的次数。用来证明「形状不对时根本没碰库」——
	// 断言 order 字段没用，因为它已经被预置好了。
	lookups int
}

func (f *fakePayRepo) FindOrderByNo(_ context.Context, _ string) (*model.Order, error) {
	f.lookups++
	if f.err != nil {
		return nil, f.err
	}
	return f.order, nil
}

// stubPaymentCreator 记下支付侧真正收到的入参。
type stubPaymentCreator struct {
	calls  int
	got    client.CreatePaymentInput
	action *dto.PayAction
	err    error
}

func (s *stubPaymentCreator) Create(_ context.Context, in client.CreatePaymentInput) (*dto.PayAction, error) {
	s.calls++
	s.got = in
	if s.err != nil {
		return nil, s.err
	}
	return s.action, nil
}

const (
	testOrderNo   = "ORD20260914000001"
	testUserID    = "11111111-1111-1111-1111-111111111111"
	testMethodID  = "22222222-2222-2222-2222-222222222222"
	testRequestID = "req-1"
)

// futureExpiry 是「还没到期」的订单期限。用固定的时钟而不是 time.Now()：这个用例要判的是
// 「期限在不在未来」，而不是「运行这一刻机器上的表准不准」。
var futureExpiry = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func newPayService(repo Repository, payments PaymentCreator) *OrderService {
	return New(repo, nil, payments, Options{Now: func() time.Time { return futureExpiry.Add(-time.Minute) }})
}

func pendingOrder() *model.Order {
	expiry := futureExpiry
	return &model.Order{
		OrderNo:       testOrderNo,
		UserID:        testUserID,
		Status:        model.OrderStatusPendingPayment,
		PayableAmount: 1980,
		ExpiresAt:     &expiry,
	}
}

// TestInitiatePaymentRejectsBadShapeWithoutTouchingStorage 覆盖请求本身的形状问题。
//
// 每一个用例都断言**没有读库、没有调支付**：形状不对就该在本地停住，让一次明显的坏请求
// 变成一次磁盘往返（更别说一次渠道往返）是白花的。
func TestInitiatePaymentRejectsBadShapeWithoutTouchingStorage(t *testing.T) {
	cases := []struct {
		name string
		in   InitiatePaymentInput
		want error
	}{
		{
			name: "empty order number",
			in:   InitiatePaymentInput{UserID: testUserID, PaymentMethodID: testMethodID, RequestID: testRequestID},
			want: ErrOrderNotFound,
		},
		{
			name: "missing payment method",
			in:   InitiatePaymentInput{OrderNo: testOrderNo, UserID: testUserID, RequestID: testRequestID},
			want: ErrPaymentMethodRequired,
		},
		{
			name: "blank payment method",
			in:   InitiatePaymentInput{OrderNo: testOrderNo, UserID: testUserID, PaymentMethodID: "   ", RequestID: testRequestID},
			want: ErrPaymentMethodRequired,
		},
		{
			name: "missing idempotency key",
			in:   InitiatePaymentInput{OrderNo: testOrderNo, UserID: testUserID, PaymentMethodID: testMethodID},
			want: ErrIdempotencyKeyRequired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakePayRepo{order: pendingOrder()}
			creator := &stubPaymentCreator{}
			_, err := newPayService(repo, creator).InitiatePayment(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if repo.lookups != 0 {
				t.Errorf("invalid input reached storage: %d lookups", repo.lookups)
			}
			if creator.calls != 0 {
				t.Errorf("invalid input reached the payment service: %d calls", creator.calls)
			}
		})
	}
}

// TestInitiatePaymentChecksTheOrderNotTheRequest 覆盖拿到订单之后的那几道判断。
func TestInitiatePaymentChecksTheOrderNotTheRequest(t *testing.T) {
	otherUser := "33333333-3333-3333-3333-333333333333"
	// 明确落在测试时钟（futureExpiry - 1min）之前。差一秒那种写法很容易写成「其实还没到」，
	// 而这条用例要判的就是那条线在哪一边。
	expired := futureExpiry.Add(-time.Hour)
	cases := []struct {
		name  string
		order *model.Order
		err   error
		want  error
	}{
		{
			name:  "someone else's order",
			order: func() *model.Order { o := pendingOrder(); o.UserID = otherUser; return o }(),
			want:  ErrOrderNotFound,
		},
		{
			name:  "already paid",
			order: func() *model.Order { o := pendingOrder(); o.Status = model.OrderStatusPaid; return o }(),
			want:  ErrOrderNotPending,
		},
		{
			name:  "cancelled",
			order: func() *model.Order { o := pendingOrder(); o.Status = model.OrderStatusCancelled; return o }(),
			want:  ErrOrderNotPending,
		},
		{
			name:  "nothing to pay",
			order: func() *model.Order { o := pendingOrder(); o.PayableAmount = 0; return o }(),
			want:  ErrOrderNotPayable,
		},
		{
			// 期限到了、但超时关单还没扫到它：状态行上还是待支付，事实上已经不是了。
			// 放它过去会造出「钱收了、单被关掉」。
			name:  "past its expiry but not yet swept",
			order: func() *model.Order { o := pendingOrder(); o.ExpiresAt = &expired; return o }(),
			want:  ErrOrderNotPending,
		},
		{
			name:  "storage says not found",
			order: pendingOrder(),
			err:   repository.ErrOrderNotFound,
			want:  ErrOrderNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakePayRepo{order: tc.order, err: tc.err}
			creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}
			_, err := newPayService(repo, creator).InitiatePayment(context.Background(), InitiatePaymentInput{
				OrderNo: testOrderNo, UserID: testUserID,
				PaymentMethodID: testMethodID, RequestID: testRequestID,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if creator.calls != 0 {
				t.Errorf("a rejected order reached the payment service: %d calls", creator.calls)
			}
		})
	}
}

// TestInitiatePaymentWithoutAnExpiryIsNotExpired 单独一条：ExpiresAt 为 nil 是
// 「这一单不设付款期限」，不是「1970 年就到期了」。把 nil 当过期会把这类单永远挡在
// 收银台外面，而且报的是「这一单不该再发起支付」——一句看不出真正原因的话。
func TestInitiatePaymentWithoutAnExpiryIsNotExpired(t *testing.T) {
	order := pendingOrder()
	order.ExpiresAt = nil
	creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}
	if _, err := newPayService(&fakePayRepo{order: order}, creator).InitiatePayment(
		context.Background(), InitiatePaymentInput{
			OrderNo: testOrderNo, UserID: testUserID,
			PaymentMethodID: testMethodID, RequestID: testRequestID,
		}); err != nil {
		t.Fatalf("an order with no expiry must not be treated as expired: %v", err)
	}
	if creator.calls != 1 {
		t.Errorf("payment service calls = %d, want 1", creator.calls)
	}
}

// TestInitiatePaymentWithoutAPaymentService 是「这个部署根本没接支付域」那一种：
// 与「支付服务这次没答上来」同一个结论（503、稍后可能成），不是一个 500。
//
// payments 传 nil 而不是传一个会报错的桩：正是那个 nil 会 panic，所以这条用例真的在测
// 「null 分支存在」。
func TestInitiatePaymentWithoutAPaymentService(t *testing.T) {
	_, err := newPayService(&fakePayRepo{order: pendingOrder()}, nil).InitiatePayment(
		context.Background(), InitiatePaymentInput{
			OrderNo: testOrderNo, UserID: testUserID,
			PaymentMethodID: testMethodID, RequestID: testRequestID,
		})
	if !errors.Is(err, ErrPaymentServiceUnavailable) {
		t.Fatalf("err = %v, want %v", err, ErrPaymentServiceUnavailable)
	}
}

// TestInitiatePaymentSendsTheOrdersAmount 是这条路最重要的一条不变量：
// **金额只来自订单**。请求体里没有任何能影响它的字段，所以这条断言守的是
// 「以后有人往输入里加一个 amount」。
func TestInitiatePaymentSendsTheOrdersAmount(t *testing.T) {
	order := pendingOrder()
	order.PayableAmount = 1980
	repo := &fakePayRepo{order: order}
	creator := &stubPaymentCreator{action: &dto.PayAction{
		PaymentNo: "PAY20260914120000000001",
		Status:    "pending",
		Action:    "jump_miniapp",
		PayParams: map[string]string{"manualPayUrl": "https://pay.example.test/x"},
	}}
	action, err := newPayService(repo, creator).InitiatePayment(context.Background(), InitiatePaymentInput{
		OrderNo:         "  " + testOrderNo + "  ",
		UserID:          testUserID,
		PaymentMethodID: " " + testMethodID + " ",
		RequestID:       " " + testRequestID + " ",
	})
	if err != nil {
		t.Fatalf("InitiatePayment: %v", err)
	}
	if creator.calls != 1 {
		t.Fatalf("payment service calls = %d, want 1", creator.calls)
	}
	if creator.got.Amount != order.PayableAmount {
		t.Errorf("amount = %d, want the order's payable amount %d", creator.got.Amount, order.PayableAmount)
	}
	// 三个字符串都去掉空白再交出去：支付侧拿 order_no 做值引用、拿 request_id 做幂等键，
	// 带着空格过去会变成一次查不到订单 / 一把和上次不等的钥匙。
	if creator.got.OrderNo != testOrderNo {
		t.Errorf("orderNo = %q, want %q", creator.got.OrderNo, testOrderNo)
	}
	if creator.got.UserID != testUserID {
		t.Errorf("userId = %q, want %q", creator.got.UserID, testUserID)
	}
	if creator.got.PaymentMethodID != testMethodID {
		t.Errorf("paymentMethodId = %q, want %q", creator.got.PaymentMethodID, testMethodID)
	}
	if creator.got.RequestID != testRequestID {
		t.Errorf("requestId = %q, want %q", creator.got.RequestID, testRequestID)
	}
	// 支付侧的结论原样回去（包括 status=failed 那一次）：这一层不加戏。
	if action == nil || action.PaymentNo != "PAY20260914120000000001" || action.Action != "jump_miniapp" {
		t.Fatalf("action did not come back unchanged: %+v", action)
	}
	if action.PayParams["manualPayUrl"] != "https://pay.example.test/x" {
		t.Errorf("payParams did not come back unchanged: %+v", action.PayParams)
	}
}

// TestInitiatePaymentPassesThroughThePaymentsVerdict 守住「渠道拒绝不是错误」：
// 支付侧回 status='failed' 时它是一次**结果**，这一层不能把它变成 error。
func TestInitiatePaymentPassesThroughThePaymentsVerdict(t *testing.T) {
	repo := &fakePayRepo{order: pendingOrder()}
	declined := &dto.PayAction{PaymentNo: "PAY1", Status: "failed",
		FailureCode: "provider_declined", FailureMessage: "余额不足"}
	creator := &stubPaymentCreator{action: declined}
	action, err := newPayService(repo, creator).InitiatePayment(context.Background(), InitiatePaymentInput{
		OrderNo: testOrderNo, UserID: testUserID,
		PaymentMethodID: testMethodID, RequestID: testRequestID,
	})
	if err != nil {
		t.Fatalf("a declined payment must not come back as an error: %v", err)
	}
	if action != declined {
		t.Fatalf("action = %+v, want the payment service's own result", action)
	}
}
