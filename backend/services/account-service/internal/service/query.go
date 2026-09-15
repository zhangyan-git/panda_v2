package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// defaultPageSize 是没给 pageSize 时的每页条数。
const defaultPageSize = 20

// Balance 读一个用户的福卡余额。
//
// 没有账户行就是 0：账户行是第一次发放时懒创建的，一个从没收到过福卡的用户问自己的
// 余额，答案是「0 张」这件事本身，不是「查不到这个人」。
func (s *AccountService) Balance(ctx context.Context, userID string) (int64, error) {
	userID = strings.TrimSpace(userID)
	if _, err := uuid.Parse(userID); err != nil {
		return 0, ErrInvalidUserID
	}
	account, err := s.repository.GetAccount(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return account.Balance, nil
}

// Account 读一个用户的账户本身（客服最常问的那一句「他还有几张」）。
func (s *AccountService) Account(ctx context.Context, userID string) (*dto.AccountResponse, error) {
	userID = strings.TrimSpace(userID)
	if _, err := uuid.Parse(userID); err != nil {
		return nil, ErrInvalidUserID
	}
	account, err := s.repository.GetAccount(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		// 没有账户行不是 404：见 Balance 的说明。
		return &dto.AccountResponse{UserID: userID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &dto.AccountResponse{
		UserID:           account.UserID,
		Balance:          account.Balance,
		FrozenBalance:    account.FrozenBalance,
		AvailableBalance: account.Available(),
		CreatedAt:        account.CreatedAt,
		UpdatedAt:        account.UpdatedAt,
		HasAccount:       true,
	}, nil
}

// Entries 分页读流水，返回 (当页, 总数)。
//
// 分页参数在这里兜底而不是只靠 api.ParsePage：小程序那条路是同一个方法，而它今天不带
// 任何筛选参数。一个 0 的 pageSize 会让 SQL 的 LIMIT 变成 0（页面永远空着），
// 一个超过上限的 pageSize 会把整库拉出来——两种都要在这里挡住。
func (s *AccountService) Entries(ctx context.Context, q dto.EntryQuery) ([]*model.FortuneCardEntry, int, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = defaultPageSize
	}
	if q.PageSize > dto.MaxPageSize {
		q.PageSize = dto.MaxPageSize
	}
	return s.repository.ListEntries(ctx, q)
}
