package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
)

// grantRecorder 是系统发券那条路的替身：记下最后一次调用的参数，并按需返回错误。
//
// ReserveInventory 是空实现——它属于 BatchRepository，而 New 收的就是那个面；后台发放那条路
// （IssueCoupons）刻意**不**实现，拿它建的 CouponService 走那条路会撞上「仓储没配」，于是
// 这些用例证明不了后台发放也能用——它们本来就不该证明。
type grantRecorder struct {
	calls  int
	last   repository.GrantParams
	result *dto.IssueCouponsResponse
	err    error
}

func (g *grantRecorder) ReserveInventory(context.Context, string, int64) error { return nil }

func (g *grantRecorder) GrantSystemCoupons(_ context.Context, p repository.GrantParams) (*dto.IssueCouponsResponse, error) {
	g.calls++
	g.last = p
	if g.err != nil {
		return nil, g.err
	}
	if g.result != nil {
		return g.result, nil
	}
	return &dto.IssueCouponsResponse{BatchID: "batch-1", IssuedQuantity: len(p.UserIDs) * p.QuantityPerUser}, nil
}

func newGrantService(g *grantRecorder) *CouponService { return New(g, couponMock{}, idemMock{}) }

// mustPayload 把一份载荷编成 Envelope，字段名与 membership-service 那边逐字一致。
func mustPayload(t *testing.T, eventType string, payload any) messaging.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return messaging.Envelope{EventID: uuid.NewString(), EventType: eventType, EventVersion: "1", Payload: raw}
}

// memberPriceEvent 是一份合格的「买了 coupon 模式会员套餐」事件。
func memberPriceEvent(t *testing.T, mutate func(*dto.MembershipChangedEvent)) messaging.Envelope {
	t.Helper()
	e := dto.MembershipChangedEvent{
		MembershipID:                uuid.NewString(),
		UserID:                      uuid.NewString(),
		PlanCode:                    "MONTHLY",
		PlanName:                    "连续包月",
		MemberPriceMode:             memberPriceModeCoupon,
		MemberPriceCouponTemplateID: uuid.NewString(),
		MemberPriceCouponsPerPeriod: 20,
		Status:                      "active",
		ExpireAt:                    time.Now().Add(30 * 24 * time.Hour),
		OrderID:                     uuid.NewString(),
		OccurredAt:                  time.Now(),
	}
	if mutate != nil {
		mutate(&e)
	}
	return mustPayload(t, dto.EventMembershipActivated, e)
}

func campaignEvent(t *testing.T, mutate func(*dto.MembershipCampaignClaimedEvent)) messaging.Envelope {
	t.Helper()
	e := dto.MembershipCampaignClaimedEvent{
		ClaimID:          uuid.NewString(),
		CampaignID:       uuid.NewString(),
		UserID:           uuid.NewString(),
		StoreID:          uuid.NewString(),
		PlanName:         "连续包月",
		GiftDays:         30,
		CouponTemplateID: uuid.NewString(),
		CouponCount:      2,
		OccurredAt:       time.Now(),
	}
	if mutate != nil {
		mutate(&e)
	}
	return mustPayload(t, dto.EventMembershipCampaignClaimed, e)
}

// TestHandleEventIssuesMemberPriceCoupons：coupon 模式 + 有订单号 ⇒ 发一期，幂等键是
// 「会员 id : 订单号」。
func TestHandleEventIssuesMemberPriceCoupons(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := memberPriceEvent(t, nil)
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 1 {
		t.Fatalf("grant called %d times, want 1", g.calls)
	}
	var payload dto.MembershipChangedEvent
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if want := payload.MembershipID + ":" + payload.OrderID; g.last.Key != want {
		t.Fatalf("idempotency key = %q, want %q", g.last.Key, want)
	}
	if g.last.Scope != scopeMemberPriceCoupon {
		t.Fatalf("scope = %q, want %q", g.last.Scope, scopeMemberPriceCoupon)
	}
	if g.last.TemplateID != payload.MemberPriceCouponTemplateID || g.last.QuantityPerUser != 20 {
		t.Fatalf("template/quantity = %q/%d, want the snapshot values", g.last.TemplateID, g.last.QuantityPerUser)
	}
	if g.last.Reason != reasonMemberPriceCoupon {
		t.Fatalf("reason = %q, want %q", g.last.Reason, reasonMemberPriceCoupon)
	}
	if len(g.last.UserIDs) != 1 || g.last.UserIDs[0] != payload.UserID {
		t.Fatalf("user ids = %v, want the event's user", g.last.UserIDs)
	}
	// 会员价券不锁门店：适用范围抄模板自带的（StoreScopeID 空，仓储那条分支才会去读模板）。
	if g.last.StoreScopeID != "" || g.last.CampaignClaimID != "" {
		t.Fatalf("member price coupons must not carry a store or claim id: %+v", g.last)
	}
}

// TestHandleEventRenewedAlsoIssues：续费与开通走同一条路（同一期一笔券）。
func TestHandleEventRenewedAlsoIssues(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := memberPriceEvent(t, nil)
	event.EventType = dto.EventMembershipRenewed
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 1 {
		t.Fatalf("grant called %d times, want 1", g.calls)
	}
}

// TestHandleEventSkipsRenewalWithoutOrder 是这一刀最容易写漏的一条：后台人工调整有效期也发
// membership.renewed，那条路上没有订单号。漏了它，后台每改一次到期日就白送一批券。
func TestHandleEventSkipsRenewalWithoutOrder(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := memberPriceEvent(t, func(e *dto.MembershipChangedEvent) {
		e.OrderID = ""
	})
	event.EventType = dto.EventMembershipRenewed
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 0 {
		t.Fatal("coupons issued for a renewal that no order paid for")
	}
}

// TestHandleEventSkipsAutoMode：auto 模式是自动享会员价，没有券这回事。
func TestHandleEventSkipsAutoMode(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := memberPriceEvent(t, func(e *dto.MembershipChangedEvent) {
		e.MemberPriceMode = "auto"
		e.MemberPriceCouponTemplateID = ""
		e.MemberPriceCouponsPerPeriod = 0
	})
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 0 {
		t.Fatal("coupons issued for an auto-mode plan")
	}
}

// TestHandleEventSkipsCouponModeWithoutTemplate：模式是 coupon 却缺模板/张数。
//
// 库上那条 CHECK 让它在生产代码里不可能出现，所以**它不是故障**——记一条日志 ack 掉，别让
// 队列一直重投一条永远解不开自己矛盾的旧消息。
func TestHandleEventSkipsCouponModeWithoutTemplate(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := memberPriceEvent(t, func(e *dto.MembershipChangedEvent) {
		e.MemberPriceCouponTemplateID = ""
	})
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 0 {
		t.Fatal("coupons issued without a template")
	}
}

// TestHandleEventIssuesCampaignCoupons：领了配券的活动 ⇒ 发活动配的那批，券锁在活动门店。
func TestHandleEventIssuesCampaignCoupons(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := campaignEvent(t, nil)
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 1 {
		t.Fatalf("grant called %d times, want 1", g.calls)
	}
	var payload dto.MembershipCampaignClaimedEvent
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if g.last.Key != payload.ClaimID || g.last.Scope != scopeCampaignCoupon {
		t.Fatalf("key/scope = %q/%q, want %q/%q", g.last.Key, g.last.Scope, payload.ClaimID, scopeCampaignCoupon)
	}
	if g.last.CampaignClaimID != payload.ClaimID {
		t.Fatalf("campaign_claim_id = %q, want %q", g.last.CampaignClaimID, payload.ClaimID)
	}
	// 券的门店范围**整份换成活动门店**，而不是抄模板自带的（老系统同一口径）。
	if g.last.StoreScopeID != payload.StoreID {
		t.Fatalf("store scope = %q, want the campaign store %q", g.last.StoreScopeID, payload.StoreID)
	}
	if g.last.Reason != reasonCampaignCoupon {
		t.Fatalf("reason = %q, want %q", g.last.Reason, reasonCampaignCoupon)
	}
	if g.last.Source != "event" || g.last.ClaimType != "event_reward" {
		t.Fatalf("source/claim type = %q/%q, want event/event_reward", g.last.Source, g.last.ClaimType)
	}
}

// TestHandleEventSkipsCampaignWithoutCoupons：没配券的活动是**常态**，不是错误。
func TestHandleEventSkipsCampaignWithoutCoupons(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := campaignEvent(t, func(e *dto.MembershipCampaignClaimedEvent) {
		e.CouponTemplateID = ""
		e.CouponCount = 0
	})
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 0 {
		t.Fatal("coupons issued for a campaign that carries none")
	}
}

// TestHandleEventIgnoresUnknownEventType：不认识 ≠ 出错。把陌生事件当错误，别的域新增一条
// 事件就能把这个队列堵死。
func TestHandleEventIgnoresUnknownEventType(t *testing.T) {
	g := &grantRecorder{}
	s := newGrantService(g)
	event := mustPayload(t, "membership.expired", map[string]string{"membershipId": uuid.NewString()})
	if err := s.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if g.calls != 0 {
		t.Fatal("coupons issued for an unrelated event type")
	}
}

// TestHandleEventRejectsMalformedPayload：载荷坏了**要报错**（让它重投/进死信），不能 ack——
// 假装成功就是「付了钱却没有券」，而没有人会知道。
func TestHandleEventRejectsMalformedPayload(t *testing.T) {
	cases := map[string]messaging.Envelope{
		"不是 json":        {EventType: dto.EventMembershipActivated, Payload: []byte("{oops")},
		"缺 membershipId": mustPayload(t, dto.EventMembershipActivated, map[string]any{"userId": uuid.NewString()}),
		"membershipId 不是 uuid": mustPayload(t, dto.EventMembershipActivated, map[string]any{
			"membershipId": "not-a-uuid", "userId": uuid.NewString(),
		}),
		"多了一个字段": mustPayload(t, dto.EventMembershipActivated, map[string]any{
			"membershipId": uuid.NewString(), "userId": uuid.NewString(), "surprise": true,
		}),
		"活动事件缺 claimId": mustPayload(t, dto.EventMembershipCampaignClaimed, map[string]any{
			"userId": uuid.NewString(),
		}),
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := &grantRecorder{}
			err := newGrantService(recorder).HandleEvent(context.Background(), event)
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("HandleEvent() error = %v, want ErrInvalidEvent", err)
			}
			if recorder.calls != 0 {
				t.Fatal("repository called for a malformed event")
			}
		})
	}
}

// TestHandleEventAcksWhenTemplateUnavailable：模板被停用/删掉是**永久失败**，重投多少次都
// 还是这个结果——记日志 ack，别把队列堵到死信，真正要看的那行日志会被淹掉。
func TestHandleEventAcksWhenTemplateUnavailable(t *testing.T) {
	g := &grantRecorder{err: repository.ErrTemplateUnavailable}
	s := newGrantService(g)
	if err := s.HandleEvent(context.Background(), memberPriceEvent(t, nil)); err != nil {
		t.Fatalf("HandleEvent() error = %v, want nil (ack)", err)
	}
	if g.calls != 1 {
		t.Fatalf("grant called %d times, want 1", g.calls)
	}
}

// TestHandleEventPropagatesRetryableErrors：库存不够、数据库抖动这类是**可以好的**，原样返
// 回让平台重投——ack 掉就等于这批券永远不会发。
func TestHandleEventPropagatesRetryableErrors(t *testing.T) {
	boom := errors.New("database is on fire")
	s := newGrantService(&grantRecorder{err: boom})
	if err := s.HandleEvent(context.Background(), memberPriceEvent(t, nil)); !errors.Is(err, boom) {
		t.Fatalf("HandleEvent() error = %v, want %v", err, boom)
	}
}

// TestGrantHashSeparatesTemplates：同一个幂等键、换了券模板时必须算出不同的摘要。
//
// 仓储拿这个摘要判 IDEMPOTENCY_CONFLICT——不这样，运营把活动的券模板换掉之后重投，旧批次会
// 被当成答案静默返回，而活动上写着送新券。
func TestGrantHashSeparatesTemplates(t *testing.T) {
	template, user := uuid.NewString(), uuid.NewString()
	base := grantHash(template, user, 1, reasonCampaignCoupon)
	// 同一份输入必须算出同一个摘要，否则重投时那条消息会被判成 IDEMPOTENCY_CONFLICT。
	if again := grantHash(template, user, 1, reasonCampaignCoupon); again != base {
		t.Fatalf("grant hash is not deterministic: %s vs %s", base, again)
	}
	cases := map[string]string{
		"换模板": grantHash(uuid.NewString(), user, 1, reasonCampaignCoupon),
		"换张数": grantHash(template, user, 2, reasonCampaignCoupon),
		"换用户": grantHash(template, uuid.NewString(), 1, reasonCampaignCoupon),
		"换原因": grantHash(template, user, 1, reasonMemberPriceCoupon),
	}
	for name, got := range cases {
		if got == base {
			t.Fatalf("grant hash does not depend on %s", name)
		}
	}
}
