package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// BeanAdminService 是咖啡豆的后台写路径：人工调整余额。
//
// 单独一个类型、而不是挂在 AccountService 上，有两个理由：
//   - 它持的是**另一个**仓储（带审计记录器的那一个，见 repository.BeanAdminRepository），
//     读路径不该为了一个用不到的 recorder 多传一个参数；
//   - 它的规则与扣减那一路相反：金额**带符号**（充错了要能把豆调回来），幂等号撞车要回
//     409 而不是回放（点它的是人）。
type BeanAdminService struct {
	beans BeanAdminRepository
	// now 与 AccountService 同一个用途：把「用哪个时刻」这件事交给测试也能决定，
	// 而不是散落的 time.Now()。
	now func() time.Time
}

// BeanAdminRepository 是后台调整需要的持久化面。
//
// 在服务层定义（而不是直接要 *repository.BeanAdminRepository），与 Repository 同一条理由：
// 让「金额能不能为 0」「幂等号必填」这几条与 SQL 无关的规则能用一根桩测到。
type BeanAdminRepository interface {
	AdjustBeans(context.Context, repository.BeanAdjustParams) (repository.EntryResult, error)
}

func NewBeanAdmin(beans BeanAdminRepository) *BeanAdminService {
	return &BeanAdminService{beans: beans, now: func() time.Time { return time.Now().UTC() }}
}

// BeanAdjustRequest 是一次人工调整。
//
// Amount **带符号**、单位分：充值为正、把充错的豆调回来为负。RequestID 是幂等号，必填；
// 它与「重发一次后台请求」的区别见 repository.ErrBeanDuplicateRequest。
type BeanAdjustRequest struct {
	UserID    string
	Amount    int64
	RequestID string
	Remark    string
	// Operator 是操作人的管理员用户 ID（令牌里的 UserID）。它同时落进流水的 operator_id
	// 与审计的 actor_id，两处因此说的是同一个人。
	Operator string
}

// AdjustBeans 调整一个用户的咖啡豆余额，返回调整后的余额。
//
// 与扣减那一路相反：这里**允许负数**（纠错），所以校验只挡 0——0 是「手滑点了一下保存」，
// 不是一次调整。余额不足（把余额扣成负数）由仓储在行锁里判，回 ErrInsufficientCoffeeBeans。
func (s *BeanAdminService) AdjustBeans(ctx context.Context, req BeanAdjustRequest) (repository.EntryResult, error) {
	userID := strings.TrimSpace(req.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return repository.EntryResult{}, fmt.Errorf("%w: %q", ErrInvalidUserID, req.UserID)
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return repository.EntryResult{}, ErrInvalidRequestID
	}
	if req.Amount == 0 {
		return repository.EntryResult{}, repository.ErrBeanAmountZero
	}
	return s.beans.AdjustBeans(ctx, repository.BeanAdjustParams{
		UserID:     userID,
		Amount:     req.Amount,
		RequestID:  requestID,
		Remark:     req.Remark,
		Operator:   strings.TrimSpace(req.Operator),
		OccurredAt: s.now(),
	})
}
