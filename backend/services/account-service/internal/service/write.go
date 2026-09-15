package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// DeductRequest 是一次扣减。Amount 是正数（要扣掉几张）。
//
// RequestID 是幂等号：调用方重试必须拿回同一笔而不是扣第二次，所以它进 entry_key 的
// 唯一索引。没有它，一次超时重试就等于白白扣掉用户一张福卡。
type DeductRequest struct {
	UserID        string
	Amount        int64
	RequestID     string
	Title         string
	ReferenceType string
	ReferenceID   string
	ReferenceNo   string
	Remark        string
}

// Deduct 扣减福卡。
//
// 余额不足是 ErrInsufficientFortuneCards，那是「用户没钱」不是「服务坏了」——由 rpc 层
// 翻成 FailedPrecondition，调用方应当把它变成给用户看的一句话。
func (s *AccountService) Deduct(ctx context.Context, req DeductRequest) (repository.EntryResult, error) {
	userID := strings.TrimSpace(req.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return repository.EntryResult{}, fmt.Errorf("%w: %q", ErrInvalidUserID, req.UserID)
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return repository.EntryResult{}, ErrInvalidRequestID
	}
	if req.Amount <= 0 {
		return repository.EntryResult{}, fmt.Errorf("%w: %d", ErrInvalidAmount, req.Amount)
	}
	// 文案由本服务拥有，调用方给的是起因；调用方知道得更细时（将来的「兑换奖品」）
	// 可以自己给一行，空着就用默认的这一行。
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = titleDraw
	}
	referenceType := strings.TrimSpace(req.ReferenceType)
	if referenceType == "" {
		referenceType = model.ReferenceTypeDraw
	}
	return s.repository.Deduct(ctx, repository.DeductParams{
		UserID:        userID,
		Amount:        req.Amount,
		Title:         title,
		ReferenceType: referenceType,
		ReferenceID:   strings.TrimSpace(req.ReferenceID),
		ReferenceNo:   strings.TrimSpace(req.ReferenceNo),
		EntryKey:      model.DrawKey(requestID),
		Remark:        strings.TrimSpace(req.Remark),
		OccurredAt:    s.now(),
	})
}

// ReverseRequest 是一次冲正。幂等号由 entry_id 派生，调用方不需要另给一个。
type ReverseRequest struct {
	EntryID string
	Title   string
	Remark  string
}

// Reverse 冲正一笔流水，把张数还回去（退款追回福卡）。
//
// 幂等：同一 entry_id 重放回放已有那笔。冲正会把余额扣成负数时拒绝——「已经抽过奖的
// 福卡追不回来」是业务规则，不是故障（见 repository.ErrReverseUncovered）。
func (s *AccountService) Reverse(ctx context.Context, req ReverseRequest) (repository.EntryResult, error) {
	entryID := strings.TrimSpace(req.EntryID)
	if _, err := uuid.Parse(entryID); err != nil {
		return repository.EntryResult{}, fmt.Errorf("%w: %q", ErrInvalidEntryID, req.EntryID)
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = titleReverse
	}
	return s.repository.Reverse(ctx, repository.ReverseParams{
		EntryID:    entryID,
		Title:      title,
		Remark:     strings.TrimSpace(req.Remark),
		OccurredAt: s.now(),
	})
}
