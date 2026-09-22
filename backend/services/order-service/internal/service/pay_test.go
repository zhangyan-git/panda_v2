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
	// lines 是这一单的订单行，分账的 biz_type 由它推。默认一行饮品（见 defaultLines）——
	// 绝大多数用例要判的不是分账那一段。
	lines []*model.OrderLine
	// lineErr 让「读行失败」那条路可测。
	lineErr error
	// lineLookups 与 lookups 同理：证明某些分支根本没读到行。
	lineLookups int
}

func (f *fakePayRepo) FindOrderByNo(_ context.Context, _ string) (*model.Order, error) {
	f.lookups++
	if f.err != nil {
		return nil, f.err
	}
	return f.order, nil
}

func (f *fakePayRepo) ListOrderLines(_ context.Context, _ string) ([]*model.OrderLine, error) {
	f.lineLookups++
	if f.lineErr != nil {
		return nil, f.lineErr
	}
	if f.lines == nil {
		// 没预置就是一行饮品。**这不是替真实情况兜底**（订单至少有一行，nil 行在库里
		// 不存在），而是让那些与分账无关的用例不必各自写一遍同样的三行。
		// 空切片是「这一单确实没有行」，与没预置分得开。
		return []*model.OrderLine{{LineNo: 1, LineType: model.LineTypeDrink}}, nil
	}
	return f.lines, nil
}

// stubPaymentCreator 记下支付侧真正收到的入参。
type stubPaymentCreator struct {
	calls  int
	got    client.CreatePaymentInput
	action *dto.PayAction
	err    error

	// —— 退款（见 refund.go）——
	//
	// 默认**没配**：绝大多数用例走的是发起支付那条路，绝不碰退款，而一个「默认成功」的
	// 退款桩会让「审核通过时到底有没有真的去退钱」这类断言失去意义。没配时明确报错，
	// 于是「这条路意外地被走到了」会当场暴露，而不是悄悄退了钱。
	refundCalls   int
	refundGot     client.CreateRefundInput
	refundOutcome *client.RefundOutcome
	refundErr     error
}

func (s *stubPaymentCreator) Create(_ context.Context, in client.CreatePaymentInput) (*dto.PayAction, error) {
	s.calls++
	s.got = in
	if s.err != nil {
		return nil, s.err
	}
	return s.action, nil
}

func (s *stubPaymentCreator) CreateRefund(_ context.Context, in client.CreateRefundInput) (*client.RefundOutcome, error) {
	s.refundCalls++
	s.refundGot = in
	if s.refundErr != nil {
		return nil, s.refundErr
	}
	if s.refundOutcome == nil {
		return nil, errors.New("stub refund is not configured")
	}
	return s.refundOutcome, nil
}

// stubWalletIdentity 是身份域的桩。默认「这个人绑了微信」：绝大多数用例要判的不是身份
// 那一段，而一个默认不绑的桩会让它们全部撞上「没绑微信」那条分支。
type stubWalletIdentity struct {
	openID string
	found  bool
	err    error
	// users 记下被问过谁，用来断言问的是付款人自己的 id。
	users []string
}

func (s *stubWalletIdentity) MiniappOpenID(_ context.Context, userID string) (string, bool, error) {
	s.users = append(s.users, userID)
	if s.err != nil {
		return "", false, s.err
	}
	return s.openID, s.found, nil
}

const (
	testOrderNo = "ORD20260914000001"
	testUserID  = "11111111-1111-1111-1111-111111111111"
	// testMethodCode 是一个真实存在于支付服务目录里的 code。用例断言的是「原样透传」，
	// 所以具体是哪一条不重要，重要的是它长得像 code 而不是 uuid——支付方式不再是一行数据。
	testMethodCode = "ums_miniapp_wechat"
	testRequestID  = "req-1"
	testOpenID     = "o_test_openid_1"
)

// userIDPtr 是订单上那个用户 ID 的指针形式。
//
// model.Order.UserID 可空（设备单没有用户，见 order/005），所以测试里构造订单要给指针：
// 用空串代替的话，「这张单没有用户」与「这张单的用户 id 是空」就分不开了，而 pay 那条路
// 的归属判定正是拿这个区分「这张单能不能付」。
func userIDPtr(id string) *string { return &id }

// futureExpiry 是「还没到期」的订单期限。用固定的时钟而不是 time.Now()：这个用例要判的是
// 「期限在不在未来」，而不是「运行这一刻机器上的表准不准」。
var futureExpiry = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func newPayService(repo Repository, payments PaymentCreator) *OrderService {
	return New(repo, nil, payments, nil, &stubWalletIdentity{openID: testOpenID, found: true},
		nil, Options{Now: func() time.Time { return futureExpiry.Add(-time.Minute) }})
}

func pendingOrder() *model.Order {
	expiry := futureExpiry
	return &model.Order{
		OrderNo:       testOrderNo,
		UserID:        userIDPtr(testUserID),
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
			in:   InitiatePaymentInput{UserID: testUserID, PaymentMethod: testMethodCode, RequestID: testRequestID},
			want: ErrOrderNotFound,
		},
		{
			name: "missing payment method",
			in:   InitiatePaymentInput{OrderNo: testOrderNo, UserID: testUserID, RequestID: testRequestID},
			want: ErrPaymentMethodRequired,
		},
		{
			name: "blank payment method",
			in:   InitiatePaymentInput{OrderNo: testOrderNo, UserID: testUserID, PaymentMethod: "   ", RequestID: testRequestID},
			want: ErrPaymentMethodRequired,
		},
		{
			name: "missing idempotency key",
			in:   InitiatePaymentInput{OrderNo: testOrderNo, UserID: testUserID, PaymentMethod: testMethodCode},
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
			order: func() *model.Order { o := pendingOrder(); o.UserID = userIDPtr(otherUser); return o }(),
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
				PaymentMethod: testMethodCode, RequestID: testRequestID,
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
			PaymentMethod: testMethodCode, RequestID: testRequestID,
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
			PaymentMethod: testMethodCode, RequestID: testRequestID,
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
		OrderNo:       "  " + testOrderNo + "  ",
		UserID:        testUserID,
		PaymentMethod: " " + testMethodCode + " ",
		RequestID:     " " + testRequestID + " ",
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
	if creator.got.PaymentMethod != testMethodCode {
		t.Errorf("paymentMethod = %q, want %q", creator.got.PaymentMethod, testMethodCode)
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

// TestInitiatePaymentSendsTheSettlementDimensions 守住分账要用的那三个维度真的交了出去。
//
// 这一条为什么重要：三个维度里任何一个没送到，支付侧的表现都是「安静地命不中规则、整单归
// 平台」——没有报错、没有异常，只是钱分错了地方。所以它必须是断言，不能靠肉眼过一遍调用点。
func TestInitiatePaymentSendsTheSettlementDimensions(t *testing.T) {
	storeID := "33333333-3333-3333-3333-333333333333"
	deviceID := "44444444-4444-4444-4444-444444444444"

	cases := []struct {
		name  string
		lines []*model.OrderLine
		want  string
	}{
		{
			name:  "a drink line is a coffee order",
			lines: []*model.OrderLine{{LineType: model.LineTypeAddon}, {LineType: model.LineTypeDrink}},
			want:  model.SettlementBizCoffee,
		},
		{
			name:  "drink wins over membership (混合单压成一个值)",
			lines: []*model.OrderLine{{LineType: model.LineTypeMembership}, {LineType: model.LineTypeDrink}},
			want:  model.SettlementBizCoffee,
		},
		{
			name:  "membership only",
			lines: []*model.OrderLine{{LineType: model.LineTypeMembership}},
			want:  model.SettlementBizMembership,
		},
		{
			name:  "addon only",
			lines: []*model.OrderLine{{LineType: model.LineTypeAddon}},
			want:  model.SettlementBizAddonProduct,
		},
		{
			name:  "membership wins over addon",
			lines: []*model.OrderLine{{LineType: model.LineTypeAddon}, {LineType: model.LineTypeMembership}},
			want:  model.SettlementBizMembership,
		},
		{
			name:  "empty lines fall back to coffee",
			lines: []*model.OrderLine{},
			want:  model.SettlementBizCoffee,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order := pendingOrder()
			order.StoreID = &storeID
			order.DeviceID = &deviceID
			repo := &fakePayRepo{order: order, lines: tc.lines}
			creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}

			if _, err := newPayService(repo, creator).InitiatePayment(context.Background(), InitiatePaymentInput{
				OrderNo: testOrderNo, UserID: testUserID,
				PaymentMethod: testMethodCode, RequestID: testRequestID,
			}); err != nil {
				t.Fatalf("InitiatePayment: %v", err)
			}
			if creator.got.StoreID != storeID || creator.got.DeviceID != deviceID {
				t.Errorf("store/device = %q/%q, want %q/%q",
					creator.got.StoreID, creator.got.DeviceID, storeID, deviceID)
			}
			if creator.got.BizType != tc.want {
				t.Errorf("bizType = %q, want %q", creator.got.BizType, tc.want)
			}
		})
	}
}

// TestInitiatePaymentWithoutAStoreSendsEmptyDimensions 守住「点位可空」：纯会员订单没有
// 点位与设备，这时发出去的是空串，而不是让整次发起失败。空值在支付侧只会让 store/device
// 两档规则命不中，那正是「这一单没有点位」应有的结果。
func TestInitiatePaymentWithoutAStoreSendsEmptyDimensions(t *testing.T) {
	repo := &fakePayRepo{order: pendingOrder(), lines: []*model.OrderLine{{LineType: model.LineTypeMembership}}}
	creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}

	if _, err := newPayService(repo, creator).InitiatePayment(context.Background(), InitiatePaymentInput{
		OrderNo: testOrderNo, UserID: testUserID,
		PaymentMethod: testMethodCode, RequestID: testRequestID,
	}); err != nil {
		t.Fatalf("InitiatePayment: %v", err)
	}
	if creator.got.StoreID != "" || creator.got.DeviceID != "" {
		t.Errorf("store/device = %q/%q, want empty", creator.got.StoreID, creator.got.DeviceID)
	}
	if creator.got.BizType != model.SettlementBizMembership {
		t.Errorf("bizType = %q, want %q", creator.got.BizType, model.SettlementBizMembership)
	}
}

// TestInitiatePaymentFailsWhenTheOrderLinesCannotBeRead：读不到行就没有 biz_type，
// 也就没有分账。这一条宁可不收这笔钱，也不收一笔分不了账的钱。
func TestInitiatePaymentFailsWhenTheOrderLinesCannotBeRead(t *testing.T) {
	repo := &fakePayRepo{order: pendingOrder(), lineErr: errors.New("lines are unreachable")}
	creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}

	_, err := newPayService(repo, creator).InitiatePayment(context.Background(), InitiatePaymentInput{
		OrderNo: testOrderNo, UserID: testUserID,
		PaymentMethod: testMethodCode, RequestID: testRequestID,
	})
	if err == nil {
		t.Fatal("reading lines failed but the payment went out")
	}
	if creator.calls != 0 {
		t.Fatalf("payment service calls = %d; 读不到行就不该发起", creator.calls)
	}
}

// TestInitiatePaymentSendsThePayersOpenID 是 openid 链路的下半段：付款人的 openid 必须
// 真的进到交给支付服务的入参里（上半段是身份域的查询，见 client.WalletIdentityReader）。
//
// 问的必须是**付款人**：openid 决定渠道向谁收钱，拿别人的 openid 过去就是替别人付款。
func TestInitiatePaymentSendsThePayersOpenID(t *testing.T) {
	wallets := &stubWalletIdentity{openID: testOpenID, found: true}
	creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}
	svc := New(&fakePayRepo{order: pendingOrder()}, nil, creator, nil, wallets,
		nil, Options{Now: func() time.Time { return futureExpiry.Add(-time.Minute) }})

	if _, err := svc.InitiatePayment(context.Background(), InitiatePaymentInput{
		OrderNo: testOrderNo, UserID: testUserID,
		PaymentMethod: testMethodCode, RequestID: testRequestID,
	}); err != nil {
		t.Fatalf("InitiatePayment: %v", err)
	}
	if len(wallets.users) != 1 || wallets.users[0] != testUserID {
		t.Fatalf("identity lookups = %v; want the payer %s", wallets.users, testUserID)
	}
	if creator.got.WalletOpenID != testOpenID {
		t.Fatalf("walletOpenId = %q; want %q", creator.got.WalletOpenID, testOpenID)
	}
}

// TestInitiatePaymentLeavesAGapForTheUnboundPayer：没绑微信**不是一个错误**，这一层不能
// 拿它把用户挡在收银台外面——扫码、H5、咖啡豆都不需要 openid，一刀切会让没绑微信的人连豆
// 都花不出去。
//
// 空 openid 照样发出去，但这不是静默降级：收场由支付侧按那条方式的 action 决定，需要用户
// 身份的渠道会把这一笔落成 failed + failure_code（见 payment-service 的 CreatePayment）。
func TestInitiatePaymentLeavesAGapForTheUnboundPayer(t *testing.T) {
	wallets := &stubWalletIdentity{found: false}
	creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}
	svc := New(&fakePayRepo{order: pendingOrder()}, nil, creator, nil, wallets,
		nil, Options{Now: func() time.Time { return futureExpiry.Add(-time.Minute) }})

	if _, err := svc.InitiatePayment(context.Background(), InitiatePaymentInput{
		OrderNo: testOrderNo, UserID: testUserID,
		PaymentMethod: testMethodCode, RequestID: testRequestID,
	}); err != nil {
		t.Fatalf("an unbound payer must not be rejected here: %v", err)
	}
	if creator.calls != 1 || creator.got.WalletOpenID != "" {
		t.Fatalf("calls = %d, walletOpenId = %q; want one call with no openid",
			creator.calls, creator.got.WalletOpenID)
	}
}

// TestInitiatePaymentFailsClosedWithoutTheIdentityService：问不到身份域是**故障**，与
// 「这个人没绑微信」完全不同——后者是一个事实，可以交给支付侧收场；前者重发一次可能就好。
//
// 所以这里既不回一次「支付失败」，也不发一次没有 openid 的发起（那会让用户去渠道那里碰
// 一次壁，而我们拿不回一个能查的失败）。
func TestInitiatePaymentFailsClosedWithoutTheIdentityService(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wallets WalletIdentityReader
	}{
		{name: "lookup failed", wallets: &stubWalletIdentity{err: errors.New("connection refused")}},
		{name: "reader not configured", wallets: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creator := &stubPaymentCreator{action: &dto.PayAction{PaymentNo: "PAY1", Status: "pending"}}
			svc := New(&fakePayRepo{order: pendingOrder()}, nil, creator, nil, tc.wallets,
				nil, Options{Now: func() time.Time { return futureExpiry.Add(-time.Minute) }})

			_, err := svc.InitiatePayment(context.Background(), InitiatePaymentInput{
				OrderNo: testOrderNo, UserID: testUserID,
				PaymentMethod: testMethodCode, RequestID: testRequestID,
			})
			if !errors.Is(err, ErrWalletIdentityUnavailable) {
				t.Fatalf("err = %v, want %v", err, ErrWalletIdentityUnavailable)
			}
			if creator.calls != 0 {
				t.Fatalf("payment service calls = %d; 没有身份就不该发起", creator.calls)
			}
		})
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
		PaymentMethod: testMethodCode, RequestID: testRequestID,
	})
	if err != nil {
		t.Fatalf("a declined payment must not come back as an error: %v", err)
	}
	if action != declined {
		t.Fatalf("action = %+v, want the payment service's own result", action)
	}
}
