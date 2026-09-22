package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// stubRepository 把「调用方给了什么」原样记下来，不做任何 SQL。
//
// 这一层要验的正是本服务**拥有**的东西：事件体怎么校验、文案怎么渲染、幂等号怎么拼、
// 默认值怎么给。这些规则与数据库无关，用一根桩就能测到；真正与 SQL 有关的幂等与余额
// 不变式在 repository 的集成测试里验。
type stubRepository struct {
	grantParams []repository.GrantParams
	grantErr    error

	deductParams []repository.DeductParams
	deductErr    error

	reverseParams []repository.ReverseParams
	reverseErr    error

	freezeParams  []repository.FreezeParams
	freezeErr     error
	releaseParams []repository.ReleaseParams
	releaseErr    error

	// 预览冻结（只读）。三个回答值各自可设：默认是两个 0，与「这一单还没发放、账上也没有
	// 可用」同一件事。
	previewParams    []repository.PreviewFreezeParams
	previewGranted   int64
	previewFreezable int64
	previewErr       error
	// recoverParams 记的是追回那一次。recoverResult 是「追回了几张」的回答，默认 0——
	// 事件侧不读它，但把它记下来能让「这一次到底动了没有」在断言里看得见。
	recoverParams []repository.RecoverParams
	recoverResult int64
	recoverErr    error

	// 咖啡豆那一半。与上面同一套记法：把调用方给的原样记下来，让测试去断言键怎么拼、
	// 金额有没有取负、文案的兜底是哪一句。
	consumeBeanParams []repository.BeanConsumeParams
	consumeBeanErr    error

	beanReverseParams []repository.BeanReverseParams
	// beanReverseResult 是「本次真的冲了一笔」的回答，默认 false（no-op）——事件侧最常见的
	// 情况就是渠道支付的订单走到这里，什么都不做。
	beanReverseResult bool
	beanReverseErr    error

	// 读路径。零值就是「这个用户没有账户行」（account 为 nil、err 为 nil），与真实仓储
	// 在找不到行时的回答一致。
	beanAccount    *model.CoffeeBeanAccount
	beanAccountErr error

	beanEntries      []*model.CoffeeBeanEntry
	beanEntriesTotal int
	beanEntriesErr   error
	// beanEntriesQuery 记的是**兜底之后**的查询条件：断言分页兜底只能从这里看。
	beanEntriesQuery dto.BeanEntryQuery
}

func (s *stubRepository) GrantOrderFortune(_ context.Context, params repository.GrantParams) ([]repository.EntryResult, error) {
	s.grantParams = append(s.grantParams, params)
	return nil, s.grantErr
}

func (s *stubRepository) Deduct(_ context.Context, params repository.DeductParams) (repository.EntryResult, error) {
	s.deductParams = append(s.deductParams, params)
	return repository.EntryResult{}, s.deductErr
}

func (s *stubRepository) Reverse(_ context.Context, params repository.ReverseParams) (repository.EntryResult, error) {
	s.reverseParams = append(s.reverseParams, params)
	return repository.EntryResult{}, s.reverseErr
}

func (s *stubRepository) FreezeAfterSale(_ context.Context, params repository.FreezeParams) (bool, error) {
	s.freezeParams = append(s.freezeParams, params)
	return false, s.freezeErr
}

// PreviewFreezeAfterSale 是只读的那一条：桩上只要把两个数原样交回，让「预览算出来的两个
// 数怎么被翻译成 gRPC 响应」这件事能被测到——判定在订单域那侧，本服务不持有规则。
func (s *stubRepository) PreviewFreezeAfterSale(_ context.Context, params repository.PreviewFreezeParams) (int64, int64, error) {
	s.previewParams = append(s.previewParams, params)
	return s.previewGranted, s.previewFreezable, s.previewErr
}

func (s *stubRepository) ReleaseAfterSale(_ context.Context, params repository.ReleaseParams) (bool, error) {
	s.releaseParams = append(s.releaseParams, params)
	return false, s.releaseErr
}

func (s *stubRepository) RecoverAfterSale(_ context.Context, params repository.RecoverParams) (int64, error) {
	s.recoverParams = append(s.recoverParams, params)
	return s.recoverResult, s.recoverErr
}

func (s *stubRepository) GetAccount(context.Context, string) (*model.FortuneCardAccount, error) {
	return nil, errors.New("not used")
}

func (s *stubRepository) ListEntries(context.Context, dto.EntryQuery) ([]*model.FortuneCardEntry, int, error) {
	return nil, 0, errors.New("not used")
}

func (s *stubRepository) ListFreezes(context.Context, dto.FreezeQuery) ([]*model.FortuneCardFreeze, int, error) {
	return nil, 0, errors.New("not used")
}

func (s *stubRepository) ConsumeBeans(_ context.Context, params repository.BeanConsumeParams) (repository.EntryResult, error) {
	s.consumeBeanParams = append(s.consumeBeanParams, params)
	return repository.EntryResult{}, s.consumeBeanErr
}

func (s *stubRepository) ReverseBeans(_ context.Context, params repository.BeanReverseParams) (bool, error) {
	s.beanReverseParams = append(s.beanReverseParams, params)
	return s.beanReverseResult, s.beanReverseErr
}

func (s *stubRepository) GetBeanAccount(context.Context, string) (*model.CoffeeBeanAccount, error) {
	if s.beanAccountErr != nil {
		return nil, s.beanAccountErr
	}
	if s.beanAccount == nil {
		// 与真实仓储一致：没有账户行时回 pgx.ErrNoRows，由服务层翻成「0 分」。
		return nil, pgx.ErrNoRows
	}
	return s.beanAccount, nil
}

func (s *stubRepository) ListBeanEntries(_ context.Context, q dto.BeanEntryQuery) ([]*model.CoffeeBeanEntry, int, error) {
	s.beanEntriesQuery = q
	if s.beanEntriesErr != nil {
		return nil, 0, s.beanEntriesErr
	}
	return s.beanEntries, s.beanEntriesTotal, nil
}

// completedEvent 拼一条 order.completed 信封。payload 由调用方给，可以是故意坏掉的。
func completedEvent(t *testing.T, payload any) messaging.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return messaging.Envelope{EventType: dto.EventOrderCompleted, EventVersion: "v1", Payload: raw}
}

func validPayload() dto.OrderCompletedEventPayload {
	return dto.OrderCompletedEventPayload{
		OrderNo:        "CO20260914001",
		OrderID:        uuid.NewString(),
		UserID:         uuid.NewString(),
		FinishedAtUnix: time.Date(2026, 9, 14, 3, 4, 5, 0, time.UTC).Unix(),
		FortuneCards: []dto.OrderFortuneGrant{
			{Kind: model.GrantKindBase, Amount: 1, EntryKey: "order:x:base"},
			{Kind: model.GrantKindBonus, CampaignID: "c-1", CampaignName: "幸运杯套", Amount: 1, EntryKey: "order:x:bonus:c-1"},
		},
	}
}

func TestHandleOrderEventIgnoresOtherEventTypes(t *testing.T) {
	repo := &stubRepository{}
	svc := New(repo)

	// 主题里有别的类型（payment.succeeded 之类）。它不是发给我们的，ack 就好。
	if err := svc.HandleOrderEvent(context.Background(), messaging.Envelope{
		EventType: "payment.succeeded",
		Payload:   []byte(`{"nonsense":true}`),
	}); err != nil {
		t.Fatalf("unrelated event type must be acked, got %v", err)
	}
	if len(repo.grantParams) != 0 {
		t.Fatalf("unrelated event type must not grant, got %d calls", len(repo.grantParams))
	}
}

func TestHandleOrderEventRejectsBadPayloads(t *testing.T) {
	badUUID := validPayload()
	badUUID.UserID = "not-a-uuid"

	missingOrderNo := validPayload()
	missingOrderNo.OrderNo = "  "

	missingEntryKey := validPayload()
	missingEntryKey.FortuneCards[1].EntryKey = ""

	nonPositiveAmount := validPayload()
	nonPositiveAmount.FortuneCards[1].Amount = 0

	unknownKind := validPayload()
	unknownKind.FortuneCards[1].Kind = "mystery"

	cases := []struct {
		name    string
		event   messaging.Envelope
		wantErr error
	}{
		{
			name:    "unknown field in payload",
			event:   completedEvent(t, map[string]any{"orderNo": "X", "orderId": "Y", "userId": uuid.NewString(), "fortuneCards": []any{}, "surprise": 1}),
			wantErr: ErrInvalidEvent,
		},
		{name: "user id is not a uuid", event: completedEvent(t, badUUID), wantErr: ErrInvalidUserID},
		{name: "order number is missing", event: completedEvent(t, missingOrderNo), wantErr: ErrInvalidEvent},
		{name: "grant has no entry key", event: completedEvent(t, missingEntryKey), wantErr: ErrInvalidEvent},
		{name: "grant amount is not positive", event: completedEvent(t, nonPositiveAmount), wantErr: ErrInvalidAmount},
		{name: "grant kind is unknown", event: completedEvent(t, unknownKind), wantErr: ErrInvalidEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubRepository{}
			err := New(repo).HandleOrderEvent(context.Background(), tc.event)
			// 每一条都必须**报错**：这些都是「发出方坏了」，重投不会让它变合法。
			// ack 掉它们才是真的把福卡丢了，而且丢得毫无痕迹。
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if len(repo.grantParams) != 0 {
				t.Fatalf("a rejected event must not grant, got %d calls", len(repo.grantParams))
			}
		})
	}
}

func TestHandleOrderEventAcksOrdersWithoutFortuneCards(t *testing.T) {
	repo := &stubRepository{}
	payload := validPayload()
	payload.FortuneCards = nil

	// 不是每单都送福卡。没有发放项不是错误，更不是「必须记一条 0 张的流水」。
	if err := New(repo).HandleOrderEvent(context.Background(), completedEvent(t, payload)); err != nil {
		t.Fatalf("an order without fortune cards must be acked, got %v", err)
	}
	if len(repo.grantParams) != 0 {
		t.Fatalf("an order without fortune cards must not grant, got %d calls", len(repo.grantParams))
	}
}

func TestHandleOrderEventGrantsBothLines(t *testing.T) {
	repo := &stubRepository{}
	payload := validPayload()

	if err := New(repo).HandleOrderEvent(context.Background(), completedEvent(t, payload)); err != nil {
		t.Fatalf("handle order event: %v", err)
	}
	if len(repo.grantParams) != 1 {
		t.Fatalf("want one grant call, got %d", len(repo.grantParams))
	}
	params := repo.grantParams[0]
	if params.UserID != payload.UserID || params.OrderID != payload.OrderID || params.OrderNo != payload.OrderNo {
		t.Fatalf("grant lost the order identity: %+v", params)
	}
	// 两条流水一次调用：一个事务，要么两条都在，要么一条都没有。拆成两次调用就会出现
	// 「基础发放到了、加赠没到」这种对不上账的中间态。
	if len(params.Lines) != 2 {
		t.Fatalf("want two grant lines, got %d: %+v", len(params.Lines), params.Lines)
	}
	if params.Lines[0].Title != titleOrderGrantBase || params.Lines[0].Amount != 1 || params.Lines[0].EntryKey != "order:x:base" {
		t.Fatalf("base line is wrong: %+v", params.Lines[0])
	}
	if params.Lines[1].Title != titleOrderGrantBase+"（幸运杯套）" || params.Lines[1].Amount != 1 {
		t.Fatalf("bonus line is wrong: %+v", params.Lines[1])
	}
	// 幂等键由订单域给：它才知道这一单的活动是哪一次。服务侧不重拼这个号，否则两边
	// 拼法一旦漂移，同一次发放就会被记成两笔。
	if params.Lines[1].EntryKey != "order:x:bonus:c-1" {
		t.Fatalf("bonus entry key must be the one from the event: %+v", params.Lines[1])
	}
	// 时间取事件的完成时刻，不是 NOW()：补投一条上周完成的事件，明细页上的顺序
	// 必须还是上周，而不是「刚刚」。
	if !params.OccurredAt.Equal(time.Unix(payload.FinishedAtUnix, 0).UTC()) {
		t.Fatalf("occurred at must come from the event, got %v", params.OccurredAt)
	}
}

func TestHandleOrderEventUsesServiceClockWhenFinishedAtIsMissing(t *testing.T) {
	repo := &stubRepository{}
	payload := validPayload()
	payload.FinishedAtUnix = 0

	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := New(repo)
	svc.now = func() time.Time { return clock }

	if err := svc.HandleOrderEvent(context.Background(), completedEvent(t, payload)); err != nil {
		t.Fatalf("handle order event: %v", err)
	}
	// 老事件可能没有完成时刻。退回本服务的时钟，而不是写零时刻——零时刻在明细页上
	// 会排到所有流水前面。
	if got := repo.grantParams[0].OccurredAt; !got.Equal(clock) {
		t.Fatalf("want the service clock %v, got %v", clock, got)
	}
}

func TestGrantTitle(t *testing.T) {
	cases := []struct {
		name  string
		grant dto.OrderFortuneGrant
		want  string
	}{
		{name: "base", grant: dto.OrderFortuneGrant{Kind: model.GrantKindBase}, want: titleOrderGrantBase},
		{name: "named bonus", grant: dto.OrderFortuneGrant{Kind: model.GrantKindBonus, CampaignName: "幸运杯套"}, want: titleOrderGrantBase + "（幸运杯套）"},
		{name: "bonus without a name", grant: dto.OrderFortuneGrant{Kind: model.GrantKindBonus}, want: titleOrderGrantBonusFallback},
		{name: "kind with padding", grant: dto.OrderFortuneGrant{Kind: " base "}, want: titleOrderGrantBase},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := grantTitle(tc.grant)
			if err != nil {
				t.Fatalf("grant title: %v", err)
			}
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}

	// 第三种发放类型：报错而不是退回 base。记成「订单完成赠送」会让这一笔再也查不出来
	// 是怎么来的——订单域加了东西而账户域还不知道，这件事必须有人看见。
	if _, err := grantTitle(dto.OrderFortuneGrant{Kind: "mystery"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("an unknown kind must be rejected, got %v", err)
	}
}

// afterSaleEvent 拼一条售后信封。payload 由调用方给，可以是故意坏掉的。
func afterSaleEvent(t *testing.T, eventType string, payload any) messaging.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return messaging.Envelope{EventType: eventType, EventVersion: "v1", Payload: raw}
}

func validAppliedPayload() dto.AfterSaleAppliedEventPayload {
	return dto.AfterSaleAppliedEventPayload{
		AfterSaleID:          uuid.NewString(),
		AfterSaleNo:          "AS20260914001",
		OrderID:              uuid.NewString(),
		OrderNo:              "CO20260914001",
		UserID:               uuid.NewString(),
		Scope:                "addon",
		RefundAmount:         2900,
		FortuneCardEntryKeys: []string{" order:x:bonus:c-1 "},
	}
}

func TestHandleOrderEventFreezesOnAfterSaleApplied(t *testing.T) {
	repo := &stubRepository{}
	payload := validAppliedPayload()

	if err := New(repo).HandleOrderEvent(context.Background(), afterSaleEvent(t, dto.EventAfterSaleApplied, payload)); err != nil {
		t.Fatalf("a well-formed applied event must be acked, got %v", err)
	}
	if len(repo.freezeParams) != 1 {
		t.Fatalf("want exactly one freeze, got %d", len(repo.freezeParams))
	}
	freeze := repo.freezeParams[0]
	if freeze.AfterSaleNo != payload.AfterSaleNo {
		t.Fatalf("want afterSaleNo %q, got %q", payload.AfterSaleNo, freeze.AfterSaleNo)
	}
	if freeze.UserID != payload.UserID || freeze.OrderNo != payload.OrderNo || freeze.OrderID != payload.OrderID {
		t.Fatalf("freeze must carry the event's ids, got %+v", freeze)
	}
	// 键两侧的空白在这里清掉，订单域发来的东西不保证干净。
	if len(freeze.EntryKeys) != 1 || freeze.EntryKeys[0] != "order:x:bonus:c-1" {
		t.Fatalf("want the trimmed entry key, got %#v", freeze.EntryKeys)
	}
	// 文案归账户域：订单域给的是起因（一张售后单），不是一句给用户看的字。
	if freeze.Reason != FreezeReasonApplied {
		t.Fatalf("want reason %q, got %q", FreezeReasonApplied, freeze.Reason)
	}
	if freeze.OccurredAt.IsZero() {
		t.Fatal("freeze must carry an occurred_at")
	}
	if len(repo.grantParams) != 0 {
		t.Fatalf("an applied event must not grant, got %d calls", len(repo.grantParams))
	}
}

func TestHandleOrderEventAcksAppliedWithoutEntryKeys(t *testing.T) {
	// 这一单没承诺福卡，或者退的是不送福卡的会员套餐。没有要冻的东西不是错误——
	// 建一条 amount=0 的冻结行反而会让「解冻时找得到」这件事变得不可靠。
	for _, keys := range [][]string{nil, {}, {"  ", ""}} {
		repo := &stubRepository{}
		payload := validAppliedPayload()
		payload.FortuneCardEntryKeys = keys

		if err := New(repo).HandleOrderEvent(context.Background(), afterSaleEvent(t, dto.EventAfterSaleApplied, payload)); err != nil {
			t.Fatalf("keys %#v must be acked, got %v", keys, err)
		}
		if len(repo.freezeParams) != 0 {
			t.Fatalf("keys %#v must not freeze, got %d calls", keys, len(repo.freezeParams))
		}
	}
}

func TestHandleOrderEventRejectsBadAfterSalePayloads(t *testing.T) {
	badUUID := validAppliedPayload()
	badUUID.UserID = "not-a-uuid"

	missingNo := validAppliedPayload()
	missingNo.AfterSaleNo = "   "

	cases := []struct {
		name    string
		event   messaging.Envelope
		wantErr error
	}{
		{
			name: "unknown field in payload",
			// 契约漂移要在联调时炸出来。order.completed 那条路上的取舍在这里同样成立。
			event:   afterSaleEvent(t, dto.EventAfterSaleApplied, map[string]any{"afterSaleNo": "AS1", "surprise": 1}),
			wantErr: ErrInvalidEvent,
		},
		{name: "user id is not a uuid", event: afterSaleEvent(t, dto.EventAfterSaleApplied, badUUID), wantErr: ErrInvalidUserID},
		// 幂等键缺了不能糊弄过去：没有它就没有「只冻一次」，重投会冻出第二行。
		{name: "after sale number is missing", event: afterSaleEvent(t, dto.EventAfterSaleApplied, missingNo), wantErr: ErrInvalidEvent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubRepository{}
			err := New(repo).HandleOrderEvent(context.Background(), tc.event)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if len(repo.freezeParams) != 0 {
				t.Fatalf("a rejected event must not freeze, got %d calls", len(repo.freezeParams))
			}
		})
	}
}

func TestHandleOrderEventDoesNothingOnApprovedReview(t *testing.T) {
	// 审核这条事件上只剩一个动作：**驳回 ⇒ 解冻**。通过是一个空分支，两件事都不做：
	//
	//	福卡  **通过不解冻**。通过了只代表「同意退」，钱还没出去——在这里解冻会让用户在
	//	      拿到退款之前先拿到能用的卡，恰好把冻结想挡的那件事放出去；收走更没道理（钱
	//	      还没退）。冻结一直保持到退款成功、追回福卡那一刻。
	//	咖啡豆 **通过也不还**。豆是支付时就已经收下的钱，还回去就是退款本身，所以它跟着
	//	      钱走：退成了才还（见 TestHandleOrderEventRecoversOnRefundSucceeded），
	//	      退款失败时也就没有「已经还了要再扣回来」这回事。
	//
	// 两者都不动的那半个分支也要有断言，否则「顺手」在 approved 里加一句还会是绿的。
	cases := []struct {
		name        string
		status      string
		wantRelease bool
	}{
		{name: "rejected releases", status: dto.AfterSaleStatusRejected, wantRelease: true},
		{name: "approved touches neither the freeze nor the beans", status: dto.AfterSaleStatusApproved},
		{name: "an unknown status is left alone", status: "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubRepository{}
			event := afterSaleEvent(t, dto.EventAfterSaleReviewed, dto.AfterSaleReviewedEventPayload{
				AfterSaleID: uuid.NewString(),
				AfterSaleNo: "AS20260914001",
				OrderID:     uuid.NewString(),
				OrderNo:     "CO20260914001",
				UserID:      uuid.NewString(),
				Status:      tc.status,
				Action:      "reject",
				Scope:       "addon",
				// 真实的 approved 事件一定带着订单域在申请时钳过的金额（order-service 在
				// amount<=0 时就拒了申请）。这里仍然填上：审核这一拍不该读它，填了才能
				// 证明「不是因为没有金额才没冲正」。
				RefundAmount: 900,
			})
			if err := New(repo).HandleOrderEvent(context.Background(), event); err != nil {
				t.Fatalf("a review event must be acked, got %v", err)
			}
			if got := len(repo.releaseParams); (got > 0) != tc.wantRelease {
				t.Fatalf("status %q: want release=%v, got %d calls", tc.status, tc.wantRelease, got)
			}
			// 三种状态都不该碰豆：冲正挂在退款成功那一拍上。
			if got := len(repo.beanReverseParams); got != 0 {
				t.Fatalf("status %q: 审核不冲正豆，got %d calls", tc.status, got)
			}
			if !tc.wantRelease {
				return
			}
			release := repo.releaseParams[0]
			if release.AfterSaleNo != "AS20260914001" {
				t.Fatalf("want the event's afterSaleNo, got %q", release.AfterSaleNo)
			}
			if release.Reason != FreezeReasonRejected {
				t.Fatalf("want reason %q, got %q", FreezeReasonRejected, release.Reason)
			}
		})
	}
}

func TestHandleOrderEventReleasesOnCancelled(t *testing.T) {
	repo := &stubRepository{}
	event := afterSaleEvent(t, dto.EventAfterSaleCancelled, dto.AfterSaleCancelledEventPayload{
		AfterSaleID: uuid.NewString(),
		AfterSaleNo: "AS20260914001",
		OrderID:     uuid.NewString(),
		OrderNo:     "CO20260914001",
		UserID:      uuid.NewString(),
		Status:      dto.AfterSaleStatusCancelled,
		Reason:      "用户改变主意",
	})

	// 撤销之后用户手上再没有任何能解开它的动作了，所以这条消息是唯一的解冻信号。
	// 它曾经不发事件（理由是「这一刻还没有任何下游动作可撤」）——冻结让那句话不再成立。
	if err := New(repo).HandleOrderEvent(context.Background(), event); err != nil {
		t.Fatalf("a cancelled event must be acked, got %v", err)
	}
	if len(repo.releaseParams) != 1 {
		t.Fatalf("want exactly one release, got %d", len(repo.releaseParams))
	}
	if repo.releaseParams[0].Reason != FreezeReasonCancelled {
		t.Fatalf("want reason %q, got %q", FreezeReasonCancelled, repo.releaseParams[0].Reason)
	}
}

// TestHandleOrderEventRecoversOnRefundSucceeded 是冻结行那两条终局里更重的一条：
// 钱退成了 ⇒ 卡不是「放回去」而是**追回**（解冻 + 冲正那几笔发放，余额真的少掉）。
//
// 与解冻放在同一个用例里对照：两个分支做的事不同（recover vs release），认错一个的
// 表现都是静默的——余额差几张，而冻结行看起来都结束了。
func TestHandleOrderEventRecoversOnRefundSucceeded(t *testing.T) {
	payload := dto.AfterSaleRefundEventPayload{
		AfterSaleID:    uuid.NewString(),
		AfterSaleNo:    "AS20260914001",
		OrderID:        uuid.NewString(),
		OrderNo:        "CO20260914001",
		UserID:         uuid.NewString(),
		RefundNo:       "RF20260914001",
		RefundAmount:   1800,
		RefundedAtUnix: time.Date(2026, 9, 14, 3, 4, 5, 0, time.UTC).Unix(),
	}
	repo := &stubRepository{}
	if err := New(repo).HandleOrderEvent(context.Background(), afterSaleEvent(t, dto.EventAfterSaleRefunded, payload)); err != nil {
		t.Fatalf("a refunded event must be acked, got %v", err)
	}
	if len(repo.recoverParams) != 1 {
		t.Fatalf("want exactly one recover, got %d", len(repo.recoverParams))
	}
	if len(repo.releaseParams) != 0 {
		t.Fatalf("退款成功不能走解冻那条路，got %d releases", len(repo.releaseParams))
	}
	recover := repo.recoverParams[0]
	if recover.AfterSaleNo != payload.AfterSaleNo {
		t.Fatalf("want afterSaleNo %q, got %q", payload.AfterSaleNo, recover.AfterSaleNo)
	}
	if recover.Reason != FreezeReasonRecovered {
		t.Fatalf("want reason %q, got %q", FreezeReasonRecovered, recover.Reason)
	}
	// 用钱的时刻而不是收到消息的那一刻：补投旧事件时流水上的顺序必须还是那几天。
	if got := recover.OccurredAt.Unix(); got != payload.RefundedAtUnix {
		t.Fatalf("occurredAt = %d, want %d（没用事件里的时刻）", got, payload.RefundedAtUnix)
	}
	// 退款单号只是「这一笔由谁退的」的证据，不进引用位——流水按订单号归集。
	if recover.Title != titleReverseRecover {
		t.Fatalf("want title %q, got %q", titleReverseRecover, recover.Title)
	}

	// 同一拍上还要把这一单扣掉的咖啡豆还回去——**这是豆唯一的冲正时点**：钱退成了，
	// 出资那部分也该回到用户账上；钱没退成时它一分不动（见下一个用例）。
	if len(repo.beanReverseParams) != 1 {
		t.Fatalf("want exactly one bean reversal, got %d", len(repo.beanReverseParams))
	}
	reverse := repo.beanReverseParams[0]
	// 金额照抄事件里的 refundAmount，键由订单与售后单派生——两者都由事件给，
	// 账户域不自己算（口径属于订单域）。
	if reverse.Amount != payload.RefundAmount {
		t.Fatalf("want the event's refundAmount %d, got %d", payload.RefundAmount, reverse.Amount)
	}
	if reverse.AfterSaleNo != payload.AfterSaleNo {
		t.Fatalf("want the event's afterSaleNo, got %q", reverse.AfterSaleNo)
	}
	if reverse.OrderID != payload.OrderID {
		t.Fatalf("want the event's orderId, got %q", reverse.OrderID)
	}
	// 文案说清是哪一拍冲的：「审核通过」与「退款成功」是两个时刻，退款失败的那一单
	// 只有前者没有后者，事后看流水要能一眼分出这笔豆是跟着什么回来的。
	if want := "售后单 " + payload.AfterSaleNo + " 退款成功"; reverse.Remark != want {
		t.Fatalf("want remark %q, got %q", want, reverse.Remark)
	}
	// 同一拍上的两处账变必须是同一个业务时刻，否则补投旧事件时它们的先后是乱的。
	if got := reverse.OccurredAt.Unix(); got != payload.RefundedAtUnix {
		t.Fatalf("豆冲正的 occurredAt = %d, want %d（与福卡那几笔用同一个时刻）", got, payload.RefundedAtUnix)
	}
}

// TestHandleOrderEventDoesNotReverseBeansWhenTheRefundFails 是上一个用例的对面：钱没出去，
// 豆就必须原封不动。
//
// 它同时钉住了「豆不再挂在审核通过上」这件事的收益——退款失败时没有任何要撤销的动作，
// 因为没有发生过：不还豆、不需要再扣回来。
func TestHandleOrderEventDoesNotReverseBeansWhenTheRefundFails(t *testing.T) {
	repo := &stubRepository{}
	payload := dto.AfterSaleRefundEventPayload{
		AfterSaleID:    uuid.NewString(),
		AfterSaleNo:    "AS20260914002",
		OrderID:        uuid.NewString(),
		OrderNo:        "CO20260914002",
		UserID:         uuid.NewString(),
		RefundNo:       "RF20260914002",
		RefundAmount:   1800,
		FailureCode:    "ACQ.SYSTEM_ERROR",
		FailureMessage: "渠道超时",
	}
	if err := New(repo).HandleOrderEvent(context.Background(), afterSaleEvent(t, dto.EventAfterSaleRefundFailed, payload)); err != nil {
		t.Fatalf("a refund_failed event must be acked, got %v", err)
	}
	if got := len(repo.beanReverseParams); got != 0 {
		t.Fatalf("退款失败不还豆，got %d reversals", got)
	}
}

// TestHandleOrderEventUsesServiceClockWhenRefundTimeIsMissing：失败事件里没有成功的时刻
// （order-service 那边 unixOrZero 把它压成 0），追回这条路上不该出现 0。
func TestHandleOrderEventUsesServiceClockWhenRefundTimeIsMissing(t *testing.T) {
	before := time.Now().UTC()
	repo := &stubRepository{}
	payload := dto.AfterSaleRefundEventPayload{
		AfterSaleNo: "AS20260914001",
		OrderID:     uuid.NewString(),
		OrderNo:     "CO20260914001",
		UserID:      uuid.NewString(),
		// 金额与订单 ID 必须给：追回那一步不读它们，但同一拍上的豆冲正要（金额是订单域
		// 钳过的数，缺了它就是欠退；订单 ID 是那笔扣减的定位键）。这一条只问时钟，
		// 别的字段照真实形状填。
		RefundAmount: 1800,
	}
	if err := New(repo).HandleOrderEvent(context.Background(), afterSaleEvent(t, dto.EventAfterSaleRefunded, payload)); err != nil {
		t.Fatalf("a refunded event without a timestamp must be acked, got %v", err)
	}
	if len(repo.recoverParams) != 1 {
		t.Fatalf("want exactly one recover, got %d", len(repo.recoverParams))
	}
	if got := repo.recoverParams[0].OccurredAt; got.Before(before.Add(-time.Minute)) || got.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("occurredAt = %v, 没有退回服务时钟", got)
	}
	// 豆那一笔也走同一个兜底（仓储那层原本自己取 NOW()，事件这条路要的是同一个时刻）。
	if len(repo.beanReverseParams) != 1 {
		t.Fatalf("want exactly one bean reversal, got %d", len(repo.beanReverseParams))
	}
	if got := repo.beanReverseParams[0].OccurredAt; got.Before(before.Add(-time.Minute)) || got.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("豆冲正的 occurredAt = %v, 没有退回服务时钟", got)
	}
}

// TestHandleOrderEventReleasesOnRefundFailed 盯的是那个「不修就永远补不回来」的洞：
// 退款失败后**必须**解冻。
//
// 失败之后那张售后单是终态，用户要重新申请；而在途冻结已经把可用吃光，重开的那张冻结行
// 只冻得到 0 张——不在这里解开，第二次退款成功时一张卡都追不回来。所以这条事件不是
// 收尾工作，是下一个回合的前提。
func TestHandleOrderEventReleasesOnRefundFailed(t *testing.T) {
	repo := &stubRepository{}
	event := afterSaleEvent(t, dto.EventAfterSaleRefundFailed, dto.AfterSaleRefundEventPayload{
		AfterSaleID:    uuid.NewString(),
		AfterSaleNo:    "AS20260914001",
		OrderID:        uuid.NewString(),
		OrderNo:        "CO20260914001",
		UserID:         uuid.NewString(),
		RefundNo:       "RF20260914001",
		RefundAmount:   1800,
		FailureCode:    "ACQ.TRADE_NOT_EXIST",
		FailureMessage: "原交易不存在",
	})

	if err := New(repo).HandleOrderEvent(context.Background(), event); err != nil {
		t.Fatalf("a refund_failed event must be acked, got %v", err)
	}
	if len(repo.releaseParams) != 1 {
		t.Fatalf("want exactly one release, got %d", len(repo.releaseParams))
	}
	if len(repo.recoverParams) != 0 {
		t.Fatalf("退款失败不能追回，got %d recovers", len(repo.recoverParams))
	}
	if repo.releaseParams[0].Reason != FreezeReasonRefundFailed {
		t.Fatalf("want reason %q, got %q", FreezeReasonRefundFailed, repo.releaseParams[0].Reason)
	}
}

func TestHandleOrderEventRejectsRefundEventsWithoutAfterSaleNo(t *testing.T) {
	// 两条新事件共用同一个载荷，所以缺幂等键这一条也要一起钉：没有它就没有「只追一次」，
	// 重投会冲第二遍（冲正有唯一索引兜着，但冻结行结不掉）。
	for _, eventType := range []string{dto.EventAfterSaleRefunded, dto.EventAfterSaleRefundFailed} {
		t.Run(eventType, func(t *testing.T) {
			repo := &stubRepository{}
			event := afterSaleEvent(t, eventType, dto.AfterSaleRefundEventPayload{
				AfterSaleNo: "  ",
				UserID:      uuid.NewString(),
			})
			if err := New(repo).HandleOrderEvent(context.Background(), event); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("want %v, got %v", ErrInvalidEvent, err)
			}
			if len(repo.recoverParams) != 0 || len(repo.releaseParams) != 0 {
				t.Fatalf("a rejected event must not touch the freeze, got %d/%d",
					len(repo.recoverParams), len(repo.releaseParams))
			}
		})
	}
}

func TestHandleOrderEventRejectsCancelledWithoutAfterSaleNo(t *testing.T) {
	repo := &stubRepository{}
	event := afterSaleEvent(t, dto.EventAfterSaleCancelled, dto.AfterSaleCancelledEventPayload{
		AfterSaleNo: "  ",
		UserID:      uuid.NewString(),
	})
	if err := New(repo).HandleOrderEvent(context.Background(), event); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("want %v, got %v", ErrInvalidEvent, err)
	}
	if len(repo.releaseParams) != 0 {
		t.Fatalf("a rejected event must not release, got %d calls", len(repo.releaseParams))
	}
}

func TestHandleOrderEventDoesNotMixUpTheBranches(t *testing.T) {
	// 四条路各管各的：一条 order.completed 不该碰冻结，一条售后事件也不该发放。
	repo := &stubRepository{}
	if err := New(repo).HandleOrderEvent(context.Background(), completedEvent(t, validPayload())); err != nil {
		t.Fatalf("completed must be acked, got %v", err)
	}
	if len(repo.freezeParams) != 0 || len(repo.releaseParams) != 0 {
		t.Fatalf("a completed event must not touch freezes, got %d/%d", len(repo.freezeParams), len(repo.releaseParams))
	}
}
