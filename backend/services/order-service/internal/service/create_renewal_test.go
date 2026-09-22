package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// 这一层测的是续费单那条路上**不碰库**的那部分：冻结快照怎么落、行与主表的恒等式成不成立、
// 重投是不是真的不报错、哪几条校验是本地挡下的。
//
// 「source 是不是 renewal」不在这里判——那个取值写死在仓储的 INSERT 里（见
// repository.CreateRenewalOrder），service 这一层根本不带它，所以那条断言属于仓储的集成用例。

// testRenewalTxID 是这一笔代扣在微信那一侧的流水号，也是这条路唯一的幂等键。
const (
	testRenewalTxID       = "4200001234202609161234567890"
	testRenewalMembership = "7d6c5b4a-1111-4222-8333-444455556666"
)

// fakeRenewalRepo 收下这一单真正要写进去的东西，什么都不落库。
//
// 它嵌的是 Repository 接口（nil）：这一层只关心续费那一个方法，别的路子这些用例一个都不会
// 走到——真走到了会 nil panic，而不是静默通过。
type fakeRenewalRepo struct {
	Repository

	calls   int
	got     repository.CreateRenewalOrderParams
	created bool
	// existing 是 created=false 时仓储会回的那一张（幂等命中）。与真实仓储的行为一致：
	// 调用方拿到的永远是「这个流水号对应的那一张单」，不是这一轮新编的号。
	existing *repository.CreateRenewalOrderResult
}

func (f *fakeRenewalRepo) CreateRenewalOrder(_ context.Context, p repository.CreateRenewalOrderParams) (*repository.CreateRenewalOrderResult, bool, error) {
	f.calls++
	f.got = p
	if !f.created && f.existing != nil {
		return f.existing, false, nil
	}
	return &repository.CreateRenewalOrderResult{OrderID: "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e", OrderNo: p.OrderNo, Created: f.created}, f.created, nil
}

// renewalOrderFixture 是一份签约时冻结下来的套餐快照（月卡 9.9 元、会员价靠发券）。
//
// 它刻意**不是**零值：member_price_mode 与那两列有值，落库的 JSON 才有东西可断——
// 全零的快照编出来和空对象长得太像，断言会变成在比两个空。
func renewalOrderFixture() *fakeRenewalRepo {
	return &fakeRenewalRepo{created: true}
}

func fullRenewalOrderInput() CreateRenewalOrderInput {
	return CreateRenewalOrderInput{
		ThirdPartyOrderNo: testRenewalTxID,
		UserID:            testUserID,
		Amount:            990,
		Plan: dto.MembershipPlanSnapshot{
			PlanID:                      "1a2b3c4d-1111-4222-8333-444455556666",
			PlanCode:                    "MONTHLY_990",
			PlanName:                    "连续包月",
			PriceCents:                  990,
			Period:                      "month",
			PeriodCount:                 1,
			AutoRenew:                   true,
			MemberPriceMode:             "coupon",
			MemberPriceCouponTemplateID: "2b3c4d5e-1111-4222-8333-444455556666",
			MemberPriceCouponsPerPeriod: 4,
		},
		MembershipID: testRenewalMembership,
		Remark:       "第 3 期",
	}
}

func newRenewalOrderService(repo Repository) *OrderService {
	return New(repo, nil, nil, nil, nil, nil, Options{Now: func() time.Time {
		return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	}})
}

// TestCreateRenewalOrderRecordsTheFrozenSnapshot 是这条路的正例：一张有用户、纯会员行、
// 直接落成已支付的订单，套餐快照原样落进会员行。
func TestCreateRenewalOrderRecordsTheFrozenSnapshot(t *testing.T) {
	repo := renewalOrderFixture()
	in := fullRenewalOrderInput()
	result, err := newRenewalOrderService(repo).CreateRenewalOrder(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Created || result.OrderID == "" || result.OrderNo == "" {
		t.Fatalf("result = %+v, want a created order with an id and a number", result)
	}
	if repo.calls != 1 {
		t.Fatalf("repository calls = %d, want 1", repo.calls)
	}

	got := repo.got
	if got.ThirdPartyOrderNo != testRenewalTxID {
		t.Errorf("thirdPartyOrderNo = %q, want the channel transaction id %q", got.ThirdPartyOrderNo, testRenewalTxID)
	}
	// 这一单**有**用户：与设备单相反（那一格是 NULL）。
	if got.UserID != testUserID {
		t.Errorf("userId = %q, want %q", got.UserID, testUserID)
	}
	if got.MembershipID == nil || *got.MembershipID != testRenewalMembership {
		t.Errorf("membershipId = %v, want %q", got.MembershipID, testRenewalMembership)
	}
	if got.Amount != 990 {
		t.Errorf("amount = %d, want the period price 990", got.Amount)
	}
	if got.PaymentMethod != renewalOrderPaymentMethod {
		t.Errorf("paymentMethod = %q, want %q", got.PaymentMethod, renewalOrderPaymentMethod)
	}
	if !strings.HasPrefix(got.OrderNo, "3CYM") {
		t.Errorf("orderNo = %q, want the 3CYM prefix the acquirer requires", got.OrderNo)
	}
	if !got.PaidAt.Equal(time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("paidAt = %v, want the time the call arrived", got.PaidAt)
	}
	// 备注带固定前缀：这一单从哪来不该由调用方决定，它给的那段只能当备注。
	if got.Remark != renewalOrderReason+"："+in.Remark {
		t.Errorf("remark = %q, want the reason prefix plus the caller's text", got.Remark)
	}
	if got.TransitionReason != renewalOrderReason {
		t.Errorf("transitionReason = %q, want %q", got.TransitionReason, renewalOrderReason)
	}

	line := got.Line
	if line == nil {
		t.Fatal("order line is missing")
	}
	if line.LineType != model.LineTypeMembership || line.LineNo != 1 || line.Quantity != 1 {
		t.Errorf("line = (%q, %d, %d), want (membership, 1, 1)", line.LineType, line.LineNo, line.Quantity)
	}
	if line.ItemID == nil || *line.ItemID != in.Plan.PlanID {
		t.Errorf("itemId = %v, want the plan %q", line.ItemID, in.Plan.PlanID)
	}
	if line.ItemCode != in.Plan.PlanCode || line.ItemName != in.Plan.PlanName {
		t.Errorf("line item = (%q, %q), want the frozen copy (%q, %q)",
			line.ItemCode, line.ItemName, in.Plan.PlanCode, in.Plan.PlanName)
	}
	// 三个价格同值、优惠为零：会员套餐没有「标价」与「成交价」之分（与 applyMembershipPlan 同）。
	if line.OriginalUnitPrice != 990 || line.UnitPrice != 990 || line.PayableAmount != 990 {
		t.Errorf("line prices = (%d, %d, %d), want all 990", line.OriginalUnitPrice, line.UnitPrice, line.PayableAmount)
	}
	if line.DiscountAmount != 0 || line.PriceDiscountAmount != 0 || line.CouponDiscountAmount != 0 {
		t.Errorf("line discounts = (%d, %d, %d), want all zero",
			line.DiscountAmount, line.PriceDiscountAmount, line.CouponDiscountAmount)
	}
	if line.CouponID != nil || line.CampaignID != nil || line.DeviceID != nil {
		t.Error("a renewal line has no coupon, no campaign and no device; one of them was written")
	}
	// 主表那三个金额**不在参数里**：仓储的 INSERT 把 original = payable = paid = amount、
	// discount = 0 写死在同一句 SQL 里（见 repository.CreateRenewalOrderParams.Amount 的说明），
	// 所以这一层只需要保证 Amount 是权威值、行上的三个价格与它同源。
	if line.PayableAmount != got.Amount || line.OriginalUnitPrice != got.Amount || line.UnitPrice != got.Amount {
		t.Errorf("the line's prices (%d, %d, %d) disagree with the period amount %d",
			line.OriginalUnitPrice, line.UnitPrice, line.PayableAmount, got.Amount)
	}
	if line.PayableAmount != line.OriginalUnitPrice*int64(line.Quantity)-line.DiscountAmount {
		t.Errorf("line payable %d does not match original %d - discount %d",
			line.PayableAmount, line.OriginalUnitPrice, line.DiscountAmount)
	}
}

// TestCreateRenewalOrderWritesThePlanSnapshotVerbatim：快照整份落进那一列，一个字段都不少。
//
// 少一个字段不会报错——那是 JSONB，写什么进去都不报——但下游（以及后台）从这一列读回来的
// 就是一份残缺的套餐，而这份快照是事后唯一能还原「这一期他买到的是什么」的东西。
func TestCreateRenewalOrderWritesThePlanSnapshotVerbatim(t *testing.T) {
	repo := renewalOrderFixture()
	in := fullRenewalOrderInput()
	if _, err := newRenewalOrderService(repo).CreateRenewalOrder(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded dto.MembershipPlanSnapshot
	if err := json.Unmarshal(repo.got.Line.MembershipPlanSnapshot, &decoded); err != nil {
		t.Fatalf("the stored snapshot is not decodable: %v", err)
	}
	// 与 create.go 里那份快照共用一个结构体，所以这里比的是**整个**结构——将来给
	// dto.MembershipPlanSnapshot 加一个字段而 rpc 那边忘了带过来，这条会红。
	if decoded != in.Plan {
		t.Errorf("stored snapshot = %+v, want %+v", decoded, in.Plan)
	}

	// 另外三列是 NOT NULL DEFAULT '{}'，而 INSERT 每一列都写值——留 nil 会直接撞 NOT NULL
	// （漏掉 campaign_snapshot 这条正是设备单那条路的集成用例先抓出来的）。
	for name, raw := range map[string][]byte{
		"specs":             repo.got.Line.Specs,
		"selectionSnapshot": repo.got.Line.SelectionSnapshot,
		"campaignSnapshot":  repo.got.Line.CampaignSnapshot,
	} {
		if string(raw) != "{}" {
			t.Errorf("%s = %q, want an empty object", name, raw)
		}
	}
}

// TestCreateRenewalOrderReplaysInsteadOfFailing：渠道重投或消息重投时返回既有那张单、
// created=false，**不报错**——扣款结果是 MQ 消息，至少一次投递，重投是常态。
func TestCreateRenewalOrderReplaysInsteadOfFailing(t *testing.T) {
	repo := renewalOrderFixture()
	repo.created = false
	repo.existing = &repository.CreateRenewalOrderResult{
		OrderID: "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e",
		OrderNo: "3CYM20260922100000123456",
	}
	result, err := newRenewalOrderService(repo).CreateRenewalOrder(context.Background(), fullRenewalOrderInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Created {
		t.Error("created = true, want false on an idempotent replay")
	}
	// 回的是**既有那一张**的单号，不是这一轮新编的：调用方要拿它写进自己的续费流水，
	// 两边的订单号必须是同一个。
	if result.OrderID != repo.existing.OrderID || result.OrderNo != repo.existing.OrderNo {
		t.Errorf("result = (%q, %q), want the existing order (%q, %q)",
			result.OrderID, result.OrderNo, repo.existing.OrderID, repo.existing.OrderNo)
	}
}

// TestCreateRenewalOrderRejectsBadShapeWithoutTouchingAnything：四条校验缺一个就本地停住，
// 一行都不写。
//
// 这四条与金额、快照那一类**有意分开**：金额与快照不校验（调用方填的就是签约时冻结的那一份，
// 而这次调用发生在钱已经收走之后，拒收只会让一笔真实发生过的交易进死信），只有「这一单根本
// 立不起来」的才拒。
func TestCreateRenewalOrderRejectsBadShapeWithoutTouchingAnything(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateRenewalOrderInput)
		want   error
	}{
		{name: "missing third party order no", mutate: func(in *CreateRenewalOrderInput) { in.ThirdPartyOrderNo = "  " }, want: ErrThirdPartyOrderNoRequired},
		{name: "missing user", mutate: func(in *CreateRenewalOrderInput) { in.UserID = " " }, want: ErrUserRequired},
		{name: "negative amount", mutate: func(in *CreateRenewalOrderInput) { in.Amount = -1 }, want: ErrRenewalAmountInvalid},
		{name: "missing plan", mutate: func(in *CreateRenewalOrderInput) { in.Plan = dto.MembershipPlanSnapshot{} }, want: ErrRenewalPlanRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := renewalOrderFixture()
			in := fullRenewalOrderInput()
			tc.mutate(&in)
			_, err := newRenewalOrderService(repo).CreateRenewalOrder(context.Background(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !IsValidationError(err) {
				t.Errorf("%v is not in ValidationErrors; the rpc layer would report it as our fault", tc.want)
			}
			if repo.calls != 0 {
				t.Error("a bad shape reached the database")
			}
		})
	}
}

// TestCreateRenewalOrderNormalizesTheMembershipID：「没有会员 ID」只有一种表示（nil），
// 空串不能进库——那一列是 uuid，空串要靠 NULLIF 才会变 NULL，漏写一处就是一次 22P02。
func TestCreateRenewalOrderNormalizesTheMembershipID(t *testing.T) {
	cases := []struct {
		name string
		give string
		// want 为空串表示这一格应当是 nil（「没有」只有一种表示）。
		want string
	}{
		{name: "empty becomes nil", give: ""},
		{name: "blank becomes nil", give: "   "},
		{name: "a real id is trimmed and kept", give: " " + testRenewalMembership + " ", want: testRenewalMembership},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := renewalOrderFixture()
			in := fullRenewalOrderInput()
			in.MembershipID = tc.give
			if _, err := newRenewalOrderService(repo).CreateRenewalOrder(context.Background(), in); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == "" {
				if repo.got.MembershipID != nil {
					t.Fatalf("membershipId = %q, want nil", *repo.got.MembershipID)
				}
				return
			}
			if repo.got.MembershipID == nil || *repo.got.MembershipID != tc.want {
				t.Fatalf("membershipId = %v, want %q", repo.got.MembershipID, tc.want)
			}
		})
	}
}

// TestCreateRenewalOrderAcceptsAZeroAmount：0 元的续期照建（一条真实发生过的调用要如实记下来）,
// 只有**负数**才拒——那是调用方算错了，吞成 0 会把真实的扣款金额永远弄丢。
func TestCreateRenewalOrderAcceptsAZeroAmount(t *testing.T) {
	repo := renewalOrderFixture()
	in := fullRenewalOrderInput()
	in.Amount = 0
	in.Plan.PriceCents = 0
	if _, err := newRenewalOrderService(repo).CreateRenewalOrder(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.got.Amount != 0 || repo.got.Line.PayableAmount != 0 {
		t.Errorf("amounts = (%d, %d), want both zero", repo.got.Amount, repo.got.Line.PayableAmount)
	}
}
