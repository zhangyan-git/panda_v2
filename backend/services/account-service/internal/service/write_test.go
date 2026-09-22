package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

func TestDeductRejectsBadRequests(t *testing.T) {
	userID := uuid.NewString()
	cases := []struct {
		name    string
		req     DeductRequest
		wantErr error
	}{
		{
			name:    "user id is not a uuid",
			req:     DeductRequest{UserID: "nope", Amount: 1, RequestID: "r-1"},
			wantErr: ErrInvalidUserID,
		},
		{
			// 没有幂等号就没有「只扣一次」这回事：一次超时重试等于白扣用户一张。
			// 宁可拒绝这一次调用，也不能收下一个无法重放的扣减。
			name:    "request id is missing",
			req:     DeductRequest{UserID: userID, Amount: 1},
			wantErr: ErrInvalidRequestID,
		},
		{
			name:    "amount is not positive",
			req:     DeductRequest{UserID: userID, Amount: 0, RequestID: "r-1"},
			wantErr: ErrInvalidAmount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubRepository{}
			if _, err := New(repo).Deduct(context.Background(), tc.req); !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if len(repo.deductParams) != 0 {
				t.Fatalf("a rejected deduct must not reach the repository, got %d calls", len(repo.deductParams))
			}
		})
	}
}

func TestDeductDefaultsTitleAndReference(t *testing.T) {
	repo := &stubRepository{}
	userID := uuid.NewString()

	if _, err := New(repo).Deduct(context.Background(), DeductRequest{
		UserID:    userID,
		Amount:    1,
		RequestID: "participation-1",
	}); err != nil {
		t.Fatalf("deduct: %v", err)
	}
	params := repo.deductParams[0]
	// 文案属于账户域（见 event.go 的说明）：调用方没给就用默认的那一行，而不是留空。
	// 明细页上一条没有字的流水等于一条查不清的流水。
	if params.Title != titleDraw {
		t.Fatalf("want the default title %q, got %q", titleDraw, params.Title)
	}
	if params.ReferenceType != model.ReferenceTypeDraw {
		t.Fatalf("want reference type %q, got %q", model.ReferenceTypeDraw, params.ReferenceType)
	}
	// 幂等键由 request_id 派生而不是调用方直接给：扣减的幂等单位是「这一次抽奖」，
	// 号由它自己带，服务侧只管拼。
	if params.EntryKey != model.DrawKey("participation-1") {
		t.Fatalf("want entry key %q, got %q", model.DrawKey("participation-1"), params.EntryKey)
	}
	// 张数带符号是 repository 的事（流水上扣减记负数），service 交出去的仍是「扣几张」。
	if params.Amount != 1 {
		t.Fatalf("want amount 1, got %d", params.Amount)
	}
}

// TestPreviewFreezeRejectsNonUUIDUser 与 Deduct / Reverse 同一条口径：拿不到一个能对上账户
// 行的用户 id 就不去问仓储。预览是**只读**的，这里拒掉不会造成任何副作用，但一个
// `WHERE user_id = 'nope'::uuid` 会以 SQL 报错的形式回给订单域，而那是一次 500。
func TestPreviewFreezeRejectsNonUUIDUser(t *testing.T) {
	repo := &stubRepository{}
	if _, _, err := New(repo).PreviewFreeze(context.Background(), "nope", []string{"order:x:base"}); !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("want %v, got %v", ErrInvalidUserID, err)
	}
	if len(repo.previewParams) != 0 {
		t.Fatalf("a rejected preview must not reach the repository, got %d calls", len(repo.previewParams))
	}
}

// TestPreviewFreezePassesBothNumbersThrough 钉住这个方法是**纯转发**：判定在订单域那侧
// （见 PreviewFreeze 的说明），本服务只给两个事实。所以两个数必须一个不减一个不并地交回去，
// 尤其是 freezable < granted 这一档——它正是「这片池子里已经有卡被抽走了」的那个信号，
// 在这里被「修正」成 granted 的话，那条业务规则就永远看不出卡被用过。
func TestPreviewFreezePassesBothNumbersThrough(t *testing.T) {
	repo := &stubRepository{previewGranted: 3, previewFreezable: 1}
	userID := uuid.NewString()

	granted, freezable, err := New(repo).PreviewFreeze(context.Background(), " "+userID+" ", []string{" order:x:base ", ""})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if granted != 3 || freezable != 1 {
		t.Fatalf("granted/freezable = %d/%d, want 3/1", granted, freezable)
	}
	params := repo.previewParams[0]
	if params.UserID != userID {
		t.Fatalf("user id = %q, 没有去空白", params.UserID)
	}
	// 键与 Freeze 同一个去空白规则（trimKeys）：空串是调用方那边拼键拼漏了，不是一笔发放，
	// 带进 SQL 只会让 `entry_key = ''` 恒不命中，静默少算一张。
	if len(params.EntryKeys) != 1 || params.EntryKeys[0] != "order:x:base" {
		t.Fatalf("entry keys = %v", params.EntryKeys)
	}
}

// TestPreviewFreezeForwardsRepositoryError 确认读账户失败不会被翻译成「冻不上几张」。
// 这两个 0 在订单域眼里是「发放还没落库」——一次抖动会被读成一条放行的理由。
func TestPreviewFreezeForwardsRepositoryError(t *testing.T) {
	boom := errors.New("boom")
	repo := &stubRepository{previewErr: boom}
	if _, _, err := New(repo).PreviewFreeze(context.Background(), uuid.NewString(), []string{"order:x:base"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want 原样透出的仓储错误", err)
	}
}

func TestReverseRejectsNonUUIDEntryID(t *testing.T) {
	repo := &stubRepository{}
	if _, err := New(repo).Reverse(context.Background(), ReverseRequest{EntryID: "  "}); !errors.Is(err, ErrInvalidEntryID) {
		t.Fatalf("want %v, got %v", ErrInvalidEntryID, err)
	}
	if len(repo.reverseParams) != 0 {
		t.Fatalf("a rejected reverse must not reach the repository, got %d calls", len(repo.reverseParams))
	}
}

func TestReverseDefaultsTitle(t *testing.T) {
	repo := &stubRepository{}
	entryID := uuid.NewString()

	if _, err := New(repo).Reverse(context.Background(), ReverseRequest{EntryID: entryID}); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if got := repo.reverseParams[0].Title; got != titleReverse {
		t.Fatalf("want the default title %q, got %q", titleReverse, got)
	}
	// 冲正不需要调用方另给幂等号：幂等单位就是「冲这一笔」，由 entry_id 派生。
	if repo.reverseParams[0].EntryID != entryID {
		t.Fatalf("want entry id %s, got %s", entryID, repo.reverseParams[0].EntryID)
	}
}
