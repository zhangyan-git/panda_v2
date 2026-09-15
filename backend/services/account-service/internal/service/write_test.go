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
