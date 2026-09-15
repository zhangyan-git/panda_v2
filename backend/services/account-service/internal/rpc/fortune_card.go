// Package rpc 实现 account-service 的 gRPC 面。包名用 rpc 而不是 grpc，
// 以免遮蔽 google.golang.org/grpc。
//
// 这一层是**薄的**：验服务令牌、把 proto 消息翻成业务入参、把业务错误翻成状态码，别的
// 什么都不做。它持的是同一个 *service.AccountService，与 HTTP 查询那条路共用一套规则
// ——两条路各写一份规则是「同一个动作在不同入口行为不同」的来源。
package rpc

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FortuneCardService 是福卡账户对内的 gRPC 面（方案 5.6 的 account-service 里福卡那一半）。
//
// 三个方法今天都还没有调用方——抽奖服务没建。它们是这个服务的对外契约，不是死代码：
// 抽奖的扣减、退款的追回会走这里，所以集成测试直接拿 gRPC 客户端打这三个，而不是等
// 有了调用方再补。另一半（咖啡豆）在 coffee_bean.go，那一半有真实调用方
// （payment-service 的纯豆出资），形状刻意与之同形。
type FortuneCardService struct {
	accountv1.UnimplementedFortuneCardServiceServer

	accounts *service.AccountService
}

func NewFortuneCardService(accounts *service.AccountService) *FortuneCardService {
	return &FortuneCardService{accounts: accounts}
}

// GetFortuneCardBalance 读一个用户的余额。
//
// 没有账户行就是 0，不是 NotFound：账户行是第一次发放时懒创建的，抽奖在扣减之前先看
// 一眼，拿到 0 才好说那句「福卡余额不足」。
func (s *FortuneCardService) GetFortuneCardBalance(ctx context.Context, req *accountv1.GetFortuneCardBalanceRequest) (*accountv1.GetFortuneCardBalanceResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	balance, err := s.accounts.Balance(ctx, req.GetUserId())
	if err != nil {
		return nil, fortuneCardError(err)
	}
	return &accountv1.GetFortuneCardBalanceResponse{Balance: balance}, nil
}

// DeductFortuneCards 扣减福卡：用户每参与一次抽奖扣 1 张。
//
// 重放（同一个 request_id）回放已有那笔，replayed 为真——调用方超时重试时会看到它，
// 那次重试不会扣第二次。余额不足是 FailedPrecondition：调用方应当把它翻成给用户看的
// 一句话，而不是当成服务故障去重试。
func (s *FortuneCardService) DeductFortuneCards(ctx context.Context, req *accountv1.DeductFortuneCardsRequest) (*accountv1.DeductFortuneCardsResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.accounts.Deduct(ctx, service.DeductRequest{
		UserID:        req.GetUserId(),
		Amount:        req.GetAmount(),
		RequestID:     req.GetRequestId(),
		Title:         req.GetTitle(),
		ReferenceType: req.GetReferenceType(),
		ReferenceID:   req.GetReferenceId(),
		ReferenceNo:   req.GetReferenceNo(),
		Remark:        req.GetRemark(),
	})
	if err != nil {
		return nil, fortuneCardError(err)
	}
	return &accountv1.DeductFortuneCardsResponse{
		BalanceAfter: result.BalanceAfter,
		EntryId:      result.EntryID,
		Replayed:     result.Replayed,
	}, nil
}

// ReverseFortuneCardEntry 冲正一笔已有的流水，把张数还回去（退款追回福卡）。
//
// 幂等：同一 entry_id 重放回放已有那笔。冲正会把余额扣成负数（那笔发放已经被抽奖用掉）
// 时回 FailedPrecondition——那是业务规则，不是故障。
func (s *FortuneCardService) ReverseFortuneCardEntry(ctx context.Context, req *accountv1.ReverseFortuneCardEntryRequest) (*accountv1.ReverseFortuneCardEntryResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.accounts.Reverse(ctx, service.ReverseRequest{
		EntryID: req.GetEntryId(),
		Title:   req.GetTitle(),
		Remark:  req.GetRemark(),
	})
	if err != nil {
		return nil, fortuneCardError(err)
	}
	return &accountv1.ReverseFortuneCardEntryResponse{
		EntryId:      result.EntryID,
		BalanceAfter: result.BalanceAfter,
		Replayed:     result.Replayed,
	}, nil
}

// fortuneCardError 把业务错误翻成 gRPC 状态码。集中在一处，三个方法才能给出同一套回答。
//
// 「本服务认识的业务结果」与「服务端故障」必须分开：前者调用方知道该说什么（余额不足、
// 这笔已经冲过），后者只能重试或告警。混成 Internal 会让一次正常的余额不足变成一次告警。
func fortuneCardError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidUserID),
		errors.Is(err, service.ErrInvalidEntryID),
		errors.Is(err, service.ErrInvalidAmount),
		errors.Is(err, service.ErrInvalidRequestID):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, repository.ErrInsufficientFortuneCards),
		errors.Is(err, repository.ErrReverseUncovered),
		errors.Is(err, repository.ErrNotReversible):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, repository.ErrEntryNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, repository.ErrEntryKeyConflict):
		// 幂等键撞到了别人的流水：调用方把号发重了，重试也解决不了。
		return status.Error(codes.AlreadyExists, err.Error())
	default:
		return status.Error(codes.Internal, "fortune card operation failed")
	}
}
