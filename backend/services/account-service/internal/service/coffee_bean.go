package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// 咖啡豆这一半的规则。与福卡那几份文件是同一层的两半：形状校验（UUID、正数、必填的号）
// 都在这里，仓储只认 SQL 能表达的东西。
//
// 与福卡的一处根本不同：豆是**钱**（单位分），所以这里的金额校验关心的是分，不是张数；
// 而豆在支付时就已经扣走，退款窗口里没有可保护的东西——这一半没有冻结，只有一个方向：
// 把钱还回去。

// BeanConsumeRequest 是一次纯豆出资的扣减。
//
// OrderID 是必填的业务字段而不是备注：幂等键由它派生（见 model.BeanConsumeKey），冲正也要
// 按它反查。调用方给错它，扣款就落到另一张订单的键上——那是一个比金额错更难查的错。
type BeanConsumeRequest struct {
	UserID  string
	OrderID string
	OrderNo string
	// Amount 是这张订单要用掉多少豆，正数、单位分。
	Amount int64
	// Title 与 Remark 是流水上给用户看的文案，两者都可以留空：标题留空由仓储兜成
	// 「咖啡豆支付」，备注留空就是没备注。由调用方给，是因为只有它知道这一笔的上下文
	// ——payment-service 把支付单号写在备注里，客服按流水反查时靠的就是它。
	//
	// 它们是**文案不是业务字段**：一个都不参与幂等键，也不影响金额与余额（见下面
	// ConsumeBeans 的传参注释）。所以形状校验只管 TrimSpace，不校验内容。
	Title  string
	Remark string
}

// ConsumeBeans 扣减咖啡豆：一张订单用豆全额付掉了。
//
// 余额不足是 repository.ErrInsufficientCoffeeBeans——「用户没钱」不是「服务坏了」，
// payment-service 把它翻成一次发起即失败的支付结果（客户端可以换一种方式重试）。
//
// 重放（同一张订单的第二次扣减）回放原样，所以调用方超时重发不会扣第二次。
func (s *AccountService) ConsumeBeans(ctx context.Context, req BeanConsumeRequest) (repository.EntryResult, error) {
	userID := strings.TrimSpace(req.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return repository.EntryResult{}, fmt.Errorf("%w: %q", ErrInvalidUserID, req.UserID)
	}
	orderID := strings.TrimSpace(req.OrderID)
	if _, err := uuid.Parse(orderID); err != nil {
		return repository.EntryResult{}, fmt.Errorf("%w: %q", ErrInvalidOrderID, req.OrderID)
	}
	if req.Amount <= 0 {
		return repository.EntryResult{}, fmt.Errorf("%w: %d", ErrInvalidAmount, req.Amount)
	}
	return s.repository.ConsumeBeans(ctx, repository.BeanConsumeParams{
		UserID:     userID,
		OrderID:    orderID,
		OrderNo:    strings.TrimSpace(req.OrderNo),
		Amount:     req.Amount,
		Title:      strings.TrimSpace(req.Title),
		Remark:     strings.TrimSpace(req.Remark),
		OccurredAt: s.now(),
	})
}

// BeanReverseRequest 是一次退款冲正：这一单的退款成功了，把扣掉的豆还回去。
type BeanReverseRequest struct {
	OrderID     string
	AfterSaleID string
	AfterSaleNo string
	// Amount 是这次要还回去的金额，正数、单位分。允许小于那笔扣减——部分退款（先退加购
	// 行、再退整单）是常态，所以金额由调用方按退款范围给，本服务不整单照抄。
	Amount int64
	Remark string
	// OccurredAt 是这笔账变的业务时刻。零值表示「就是现在」——gRPC 那条路（支付侧当场
	// 发起冲正）本来就是即时发生的，不传这个字段。
	//
	// 事件那条路必须传：补投一条前几天的事件时，流水上的顺序得还是那几天，与同一拍上
	// 福卡那几笔冲正（用事件的 refundedAt）对得起来。与发放、扣减同一条规矩。
	OccurredAt time.Time
}

// ReverseBeans 把一单扣掉的豆还回去，返回「本次真的冲了一笔」。
//
// 找不到那笔扣减就什么都不做（返回 false，不是错误）：绝大多数订单是渠道支付的，这条
// 路径天天会被走到——把它做成错误，会让一条完全正常的事件进重试链，一路重试到死信。
//
// 冲正额超过「这笔扣减还没冲回的部分」是错误而不是静默钳制：订单域已经按「实付 - 已退 -
// 在途」钳过一次，真撞到说明有一处算错了，欠退比错退更容易被忽略（见 repository）。
func (s *AccountService) ReverseBeans(ctx context.Context, req BeanReverseRequest) (bool, error) {
	orderID := strings.TrimSpace(req.OrderID)
	if _, err := uuid.Parse(orderID); err != nil {
		return false, fmt.Errorf("%w: %q", ErrInvalidOrderID, req.OrderID)
	}
	afterSaleNo := strings.TrimSpace(req.AfterSaleNo)
	if afterSaleNo == "" {
		return false, ErrInvalidAfterSaleNo
	}
	if req.Amount <= 0 {
		return false, fmt.Errorf("%w: %d", ErrInvalidAmount, req.Amount)
	}
	occurredAt := req.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = s.now()
	}
	return s.repository.ReverseBeans(ctx, repository.BeanReverseParams{
		OrderID:     orderID,
		AfterSaleID: strings.TrimSpace(req.AfterSaleID),
		AfterSaleNo: afterSaleNo,
		Amount:      req.Amount,
		Remark:      strings.TrimSpace(req.Remark),
		OccurredAt:  occurredAt,
	})
}

// BeanAccount 读一个用户的豆账户。
//
// 没有账户行就是 0 分：账户行是第一次调整或第一次扣减时才懒创建的，一个从没充过豆的用户
// 问自己的余额，答案是「0」这件事本身，不是「查不到这个人」（与 Account 同一条）。
func (s *AccountService) BeanAccount(ctx context.Context, userID string) (*dto.BeanAccountResponse, error) {
	userID = strings.TrimSpace(userID)
	if _, err := uuid.Parse(userID); err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidUserID, userID)
	}
	account, err := s.repository.GetBeanAccount(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &dto.BeanAccountResponse{UserID: userID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &dto.BeanAccountResponse{
		UserID:     account.UserID,
		Balance:    account.Balance,
		HasAccount: true,
		CreatedAt:  account.CreatedAt,
		UpdatedAt:  account.UpdatedAt,
	}, nil
}

// BeanEntries 分页读豆流水，返回 (当页, 总数)。
//
// 分页兜底逐条照抄 Entries：0 的 pageSize 会让 SQL 的 LIMIT 变成 0（页面永远空着），
// 超过上限的 pageSize 会把整库拉出来，两种都要在这里挡住。
func (s *AccountService) BeanEntries(ctx context.Context, q dto.BeanEntryQuery) ([]*model.CoffeeBeanEntry, int, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = defaultPageSize
	}
	if q.PageSize > dto.MaxPageSize {
		q.PageSize = dto.MaxPageSize
	}
	return s.repository.ListBeanEntries(ctx, q)
}
