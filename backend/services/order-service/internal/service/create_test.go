package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// 这一层测的是「下单买会员」这条路上**不碰网络的那部分**：谁的价进得去、谁的价进不去、
// 没有会员服务时怎么结束。这三件事都不需要真库真服务，而它们正是这条路出事的地方——
// 出事的样子是「用户花一分钱开了一年会员」，而且库里看起来一切正常。

// fakeCreateRepo 收下这次下单真正写进去的东西，什么都不落库。
//
// 内嵌 Repository（同 after_sale_test.go / pay_test.go）：只实现本文件走到的方法。
type fakeCreateRepo struct {
	Repository

	calls int
	got   repository.CreateOrderParams
}

func (f *fakeCreateRepo) CreateOrder(_ context.Context, p repository.CreateOrderParams) (*repository.CreateOrderResult, bool, error) {
	f.calls++
	f.got = p
	return &repository.CreateOrderResult{
		OrderID:           "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		OrderNo:           p.Order.OrderNo,
		Status:            p.Order.Status,
		FulfillmentStatus: p.Order.FulfillmentStatus,
		OriginalAmount:    p.Order.OriginalAmount,
		DiscountAmount:    p.Order.DiscountAmount,
		PayableAmount:     p.Order.PayableAmount,
		ExpiresAt:         p.Order.ExpiresAt,
		CreatedAt:         time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC),
	}, false, nil
}

// stubPlanReader 是一份「会员域此刻的回答」。
//
// 两个问题各有各的答案（entitlement / entitlementErr）：它们是会员域应答里的两件事，
// 一个不给也会答另一个。塞在同一组字段里，就没法测「套餐问到了、资格没问到」这种
// 恰好只发生在一半上的故障。
type stubPlanReader struct {
	plan  *client.MembershipPlan
	found bool
	err   error

	entitlement    *client.MemberPriceEntitlement
	entitlementErr error

	calls              int
	gotPlan            string
	entitlementCalls   int
	gotEntitlementUser string
}

func (s *stubPlanReader) Get(_ context.Context, planID string) (*client.MembershipPlan, bool, error) {
	s.calls++
	s.gotPlan = planID
	return s.plan, s.found, s.err
}

// Entitlement 默认答「这个人没有会员价资格」——绝大多数请求的正常答案，也是用例不关心
// 会员价时最安全的那个：它让饮品行按原价算，而不是悄悄按会员价卖。
func (s *stubPlanReader) Entitlement(_ context.Context, userID string) (*client.MemberPriceEntitlement, error) {
	s.entitlementCalls++
	s.gotEntitlementUser = userID
	if s.entitlementErr != nil {
		return nil, s.entitlementErr
	}
	if s.entitlement == nil {
		return &client.MemberPriceEntitlement{}, nil
	}
	return s.entitlement, nil
}

const (
	testPlanID   = "7f0f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"
	testPlanCode = "MONTHLY"
	// 会员域说的价，与请求里那个 1 分钱不是一回事。
	testPlanPrice = 990
	testIdemKey   = "idem-membership-1"
)

func testPlan() *client.MembershipPlan {
	return &client.MembershipPlan{
		ID: testPlanID, Code: testPlanCode, Name: "连续包月会员",
		PriceCents: testPlanPrice, Period: "month", PeriodCount: 1, AutoRenew: true,
		MemberPriceMode: "coupon", MemberPriceCouponTemplateID: "TPL-1",
		MemberPriceCouponsPerPeriod: 2,
	}
}

// membershipOrder 是「客户端想花一分钱买一年会员」那一次请求。
//
// 价格字段一律填 1（其余行类型共用的那些字段，客户端能填什么就填什么），套餐 ID 才是
// 唯一应当被采信的值。快照字段已经在 DTO 上删掉了，所以这里连填的机会都没有——
// 这正是修复的形态：不是「校验客户端那份快照」，而是请求里根本没有它。
func membershipOrder() dto.CreateOrderRequest {
	planID := testPlanID
	one := int64(1)
	return dto.CreateOrderRequest{
		Source: model.SourceMiniapp,
		Lines: []dto.CreateOrderLine{{
			LineType:          model.LineTypeMembership,
			MembershipPlanID:  &planID,
			ItemCode:          "FAKE-YEAR",
			ItemName:          "假年卡",
			Quantity:          1,
			OriginalUnitPrice: one,
			UnitPrice:         one,
		}},
	}
}

func newCreateService(repo Repository, devices DeviceReader, plans MembershipPlanReader) *OrderService {
	// wallets 为 nil：下单不发起支付，openid 是发起那一步的事（见 InitiatePayment）。
	return New(repo, devices, nil, plans, nil, nil, Options{Now: func() time.Time {
		return time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	}})
}

func createMembershipOrder(t *testing.T, svc *OrderService) error {
	t.Helper()
	_, _, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		UserID:         testUserID,
		IdempotencyKey: testIdemKey,
		Request:        membershipOrder(),
	})
	return err
}

// TestCreateOrderPricesAMembershipLineFromTheMembershipService 是这条路的核心不变量。
//
// 请求里那个 1 分钱**一个字段都不许**落到订单上：行的原价、成交价、应付额、订单的合计
// 全部只能来自会员域这次回答的价。断言的是落库前那一份 params，不是响应体——响应体是
// 从仓储回来的，万一仓储写进去的是客户端那个价，看响应是看不出来的。
func TestCreateOrderPricesAMembershipLineFromTheMembershipService(t *testing.T) {
	repo := &fakeCreateRepo{}
	reader := &stubPlanReader{plan: testPlan(), found: true}
	if err := createMembershipOrder(t, newCreateService(repo, nil, reader)); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("仓储调用次数 = %d, want 1", repo.calls)
	}
	if reader.gotPlan != testPlanID {
		t.Fatalf("问会员域的套餐 ID = %q, want %q", reader.gotPlan, testPlanID)
	}
	if len(repo.got.Lines) != 1 {
		t.Fatalf("订单行数 = %d, want 1", len(repo.got.Lines))
	}
	line := repo.got.Lines[0]
	for _, field := range []struct {
		name string
		got  int64
	}{
		{"行的目录价", line.OriginalUnitPrice},
		{"行的成交单价", line.UnitPrice},
		{"行的应付额", line.PayableAmount},
		{"订单的合计原价", repo.got.Order.OriginalAmount},
		{"订单的应付额", repo.got.Order.PayableAmount},
	} {
		if field.got != testPlanPrice {
			t.Errorf("%s = %d, want 会员域说的 %d（客户端填了 1 分钱）", field.name, field.got, testPlanPrice)
		}
	}
	if repo.got.Order.DiscountAmount != 0 || line.DiscountAmount != 0 {
		t.Errorf("优惠额 = %d/%d, want 0（会员套餐不打折）",
			repo.got.Order.DiscountAmount, line.DiscountAmount)
	}
	// 名称与编码同样是会员域的副本：客户端那个「假年卡」不能被采信，否则用户在小程序
	// 看到的与他买到的就不是同一个东西。
	if line.ItemCode != testPlanCode || line.ItemName != "连续包月会员" {
		t.Errorf("行的编码/名称 = %q/%q, want 会员域的 %q/连续包月会员", line.ItemCode, line.ItemName, testPlanCode)
	}
	if line.ItemID == nil || *line.ItemID != testPlanID {
		t.Errorf("行的 itemId = %v, want 套餐 ID %q", line.ItemID, testPlanID)
	}
	// 纯会员订单不落设备、不进履约队列：否则它会永远卡在「待制作」里没人能出杯。
	if repo.got.Order.DeviceID != nil || line.DeviceID != nil {
		t.Errorf("纯会员订单不该带设备: order=%v line=%v", repo.got.Order.DeviceID, line.DeviceID)
	}
	if repo.got.Order.FulfillmentStatus != model.FulfillmentNone {
		t.Errorf("履约状态 = %q, want %q", repo.got.Order.FulfillmentStatus, model.FulfillmentNone)
	}
	// 买会员的人还没有会员，所以这一单不该被要求带会员资格；反过来它也不该凭空造一个。
	if repo.got.Order.MembershipID != nil {
		t.Errorf("纯会员订单不该带会员资格: %v", *repo.got.Order.MembershipID)
	}
}

// TestCreateOrderFreezesThePlanSnapshotTheMembershipServiceGave 钉住快照的内容。
//
// 这份快照有两个去处，都必须对：落进 order_lines.membership_plan_snapshot，以及付款
// 之后作为 order.paid 的 membership 段发给会员域。字段名与那边逐字对应（那边开着
// DisallowUnknownFields），少一个字段会员域就发不了会员价券。所以这里按**线上格式**比，
// 不比 struct——比 struct 的话，一个改错的 json tag 会被两边一起改对而悄悄通过。
func TestCreateOrderFreezesThePlanSnapshotTheMembershipServiceGave(t *testing.T) {
	repo := &fakeCreateRepo{}
	if err := createMembershipOrder(t, newCreateService(repo, nil, &stubPlanReader{plan: testPlan(), found: true})); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(repo.got.Lines[0].MembershipPlanSnapshot, &snapshot); err != nil {
		t.Fatalf("快照解不开: %v（%s）", err, repo.got.Lines[0].MembershipPlanSnapshot)
	}
	want := map[string]any{
		"planId": testPlanID, "planCode": testPlanCode, "planName": "连续包月会员",
		"priceCents": float64(testPlanPrice), "period": "month", "periodCount": float64(1),
		"autoRenew": true, "memberPriceMode": "coupon",
		"memberPriceCouponTemplateId": "TPL-1", "memberPriceCouponsPerPeriod": float64(2),
	}
	if len(snapshot) != len(want) {
		t.Fatalf("快照字段数 = %d, want %d: %s", len(snapshot), len(want), repo.got.Lines[0].MembershipPlanSnapshot)
	}
	// 一个都不能少、也不能多：少了会员域发不了券，多了它整条消息进死信。
	for key, value := range want {
		if snapshot[key] != value {
			t.Errorf("快照 %s = %v, want %v", key, snapshot[key], value)
		}
	}
	for key := range snapshot {
		if _, ok := want[key]; !ok {
			t.Errorf("快照多了一个字段 %q，会员域会因此把整条消息丢进死信", key)
		}
	}
}

// TestCreateOrderFailsClosedWithoutAMembershipReader 是这条路最重要的一条兜底：
// **绝不退化成「那就用请求里那份」**（那份已经不存在了，但退化成 0 元更糟）。
//
// plans 传 nil 而不是传一个会报错的桩：正是那个 nil 会 panic，所以这条用例真的在测
// 「null 分支存在」。断言仓储没被碰过——半个订单也不许落。
func TestCreateOrderFailsClosedWithoutAMembershipReader(t *testing.T) {
	repo := &fakeCreateRepo{}
	err := createMembershipOrder(t, newCreateService(repo, nil, nil))
	if !errors.Is(err, ErrMembershipPlanUnavailable) {
		t.Fatalf("err = %v, want %v", err, ErrMembershipPlanUnavailable)
	}
	if repo.calls != 0 {
		t.Fatalf("没有会员读端却落了单：仓储调用 %d 次", repo.calls)
	}
	// 它是「我们暂时答不上来」（503），不是「你选的套餐买不了」（400）。混成后者会让用户
	// 换一个套餐重下，而其实原样重发一次就好了。
	if IsValidationError(err) {
		t.Fatal("ErrMembershipPlanUnavailable 不该被当成请求不合法：它是我们这侧答不上来")
	}
}

// TestCreateOrderMembershipVerdictsFromTheMembershipService 覆盖会员域给出的另外两种答案。
func TestCreateOrderMembershipVerdictsFromTheMembershipService(t *testing.T) {
	cases := []struct {
		name  string
		plan  *client.MembershipPlan
		found bool
		err   error
		want  error
	}{
		{
			// 套餐不存在，或者存在但已下架（draft / disabled）——读端把两者都报成「没找到」。
			name: "套餐买不了",
			err:  nil, found: false,
			want: ErrMembershipPlanNotFound,
		},
		{
			name: "会员服务没答上来",
			err:  errors.New("connection refused"),
			want: ErrMembershipPlanUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeCreateRepo{}
			err := createMembershipOrder(t, newCreateService(repo, nil,
				&stubPlanReader{plan: tc.plan, found: tc.found, err: tc.err}))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if repo.calls != 0 {
				t.Fatalf("结论还没拿到就落了单：仓储调用 %d 次", repo.calls)
			}
		})
	}
}

// TestCreateOrderDoesNotStutterTheUnavailableMessage 守一句人话。
//
// 读端对「问不到会员域」回的就是这个哨兵，而这一层的错误串会原样进 503 响应体的
// errorMessage。包一层就成了「...不可用：...不可用」——一个看起来像 bug 的句子，而它
// 恰恰是用户在故障时唯一看到的东西。
func TestCreateOrderDoesNotStutterTheUnavailableMessage(t *testing.T) {
	repo := &fakeCreateRepo{}
	err := createMembershipOrder(t, newCreateService(repo, nil,
		&stubPlanReader{err: ErrMembershipPlanUnavailable}))
	if !errors.Is(err, ErrMembershipPlanUnavailable) {
		t.Fatalf("err = %v, want %v", err, ErrMembershipPlanUnavailable)
	}
	if err.Error() != ErrMembershipPlanUnavailable.Error() {
		t.Fatalf("错误串重复了自己: %q", err.Error())
	}
	if repo.calls != 0 {
		t.Fatalf("问不到会员域却落了单：仓储调用 %d 次", repo.calls)
	}
}

// TestCreateOrderRejectsBadMembershipLinesWithoutAskingTheMembershipService 覆盖请求形状。
//
// 每一条都断言**没问过会员域**：形状不对就该在本地停住。套餐 ID 不合法那次尤其明显——
// 明知会拿回一个 InvalidArgument 还要跑一趟，是把一次必然失败的往返挂在用户的等待上。
func TestCreateOrderRejectsBadMembershipLinesWithoutAskingTheMembershipService(t *testing.T) {
	valid := testPlanID
	blank := "   "
	notUUID := "MONTHLY"
	planID := "7f0f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"

	cases := []struct {
		name string
		line dto.CreateOrderLine
		want error
	}{
		{
			name: "没给套餐 ID",
			line: dto.CreateOrderLine{LineType: model.LineTypeMembership, Quantity: 1},
			want: ErrMembershipPlanIDRequired,
		},
		{
			name: "套餐 ID 是空白",
			line: dto.CreateOrderLine{LineType: model.LineTypeMembership, MembershipPlanID: &blank, Quantity: 1},
			want: ErrMembershipPlanIDRequired,
		},
		{
			name: "套餐 ID 不是 uuid",
			line: dto.CreateOrderLine{LineType: model.LineTypeMembership, MembershipPlanID: &notUUID, Quantity: 1},
			want: ErrMembershipPlanIDInvalid,
		},
		{
			// quantity 是「买几份」，不是「买几期」：放过它等于收三份钱、只开一期会员。
			name: "会员行买了三份",
			line: dto.CreateOrderLine{LineType: model.LineTypeMembership, MembershipPlanID: &valid, Quantity: 3},
			want: ErrMembershipQuantityInvalid,
		},
		{
			name: "饮品行带了套餐 ID",
			line: dto.CreateOrderLine{LineType: model.LineTypeDrink, MembershipPlanID: &valid, Quantity: 1},
			want: ErrMembershipPlanIDNotAllowed,
		},
		{
			name: "加购行带了套餐 ID",
			line: func() dto.CreateOrderLine {
				campaign := "campaign-1"
				return dto.CreateOrderLine{
					LineType: model.LineTypeAddon, MembershipPlanID: &valid,
					CampaignID: &campaign, Quantity: 1,
				}
			}(),
			want: ErrMembershipPlanIDNotAllowed,
		},
		{
			// 会员套餐不打折：会员价那条优惠是给饮品的。带券进来还留着的话，下面那条
			// 「券只优惠饮品」的规则就会先把它拒掉——两种拒法都对，这里钉的是后者。
			name: "会员行带券",
			line: func() dto.CreateOrderLine {
				coupon := "coupon-1"
				return dto.CreateOrderLine{
					LineType: model.LineTypeMembership, MembershipPlanID: &planID,
					CouponID: &coupon, CouponDiscountAmount: 100, Quantity: 1,
				}
			}(),
			want: ErrCouponOnlyOnDrink,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeCreateRepo{}
			reader := &stubPlanReader{plan: testPlan(), found: true}
			_, _, err := newCreateService(repo, nil, reader).CreateOrder(context.Background(), CreateOrderInput{
				UserID: testUserID, IdempotencyKey: testIdemKey,
				Request: dto.CreateOrderRequest{Source: model.SourceMiniapp, Lines: []dto.CreateOrderLine{tc.line}},
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if reader.calls != 0 {
				t.Errorf("形状不对却问了会员域 %d 次", reader.calls)
			}
			if repo.calls != 0 {
				t.Errorf("形状不对却落了单：仓储调用 %d 次", repo.calls)
			}
		})
	}
}

// TestCreateOrderAllowsOneMembershipLinePerOrder 守住「一单一个会员套餐」。
//
// 数据库那边有 order_lines_one_membership_per_order 兜底，但这里要先讲清楚：让客户端拿到
// 一个从唯一索引里冒出来的错误，它说不清是哪一行的问题，而这一次会出现两张会员单。
func TestCreateOrderAllowsOneMembershipLinePerOrder(t *testing.T) {
	first, second := testPlanID, "8f0f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"
	repo := &fakeCreateRepo{}
	reader := &stubPlanReader{plan: testPlan(), found: true}
	_, _, err := newCreateService(repo, nil, reader).CreateOrder(context.Background(), CreateOrderInput{
		UserID: testUserID, IdempotencyKey: testIdemKey,
		Request: dto.CreateOrderRequest{Source: model.SourceMiniapp, Lines: []dto.CreateOrderLine{
			{LineType: model.LineTypeMembership, MembershipPlanID: &first, Quantity: 1},
			{LineType: model.LineTypeMembership, MembershipPlanID: &second, Quantity: 1},
		}},
	})
	if !errors.Is(err, ErrTooManyMembershipLines) {
		t.Fatalf("err = %v, want %v", err, ErrTooManyMembershipLines)
	}
	if reader.calls != 0 || repo.calls != 0 {
		t.Errorf("两行会员就该在本地停住：会员域 %d 次、仓储 %d 次", reader.calls, repo.calls)
	}
}
