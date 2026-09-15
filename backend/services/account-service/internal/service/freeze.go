package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// FreezeReasonApplied / FreezeReasonRejected / FreezeReasonCancelled 是冻结行上那句原因。
//
// 文案归账户域（与流水上的 title 同一个理由）：说的是「为什么这张卡现在不能用」，
// 而订单域给的是起因——一张售后单，不是一句给用户看的字。
const (
	FreezeReasonApplied   = "退款申请中"
	FreezeReasonRejected  = "退款申请被驳回"
	FreezeReasonCancelled = "用户撤销了退款申请"
)

// Freeze 冻结一批发放：用户提交退款申请，这一单的福卡从这一刻起不能拿去抽奖。
//
// 要冻哪几笔由调用方（订单域）给——它拆承诺快照、按退款范围挑行，本服务不去猜
// 「哪张卡是哪一行送的」。空列表直接返回：这一单没承诺福卡，或者退的是不送福卡的会员套餐。
//
// 幂等由 after_sale_no 提供，所以这里没有「已经冻过了」的分支——那段判断恰好会把正常的
// 重投误判成冲突（与扣减、冲正是同一个取舍）。
func (s *AccountService) Freeze(ctx context.Context, userID, afterSaleNo, orderID, orderNo string, entryKeys []string, reason string) error {
	userID = strings.TrimSpace(userID)
	if _, err := uuid.Parse(userID); err != nil {
		return ErrInvalidUserID
	}
	afterSaleNo = strings.TrimSpace(afterSaleNo)
	if afterSaleNo == "" {
		// 没有幂等键就没有「只冻一次」这回事。宁可让这条消息进死信，也不能凭空建一行
		// 解冻时找不到的冻结——那会把卡锁死到退款流程结束之后。
		return ErrInvalidAfterSaleNo
	}
	keys := trimKeys(entryKeys)
	if len(keys) == 0 {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = FreezeReasonApplied
	}

	_, err := s.repository.FreezeAfterSale(ctx, repository.FreezeParams{
		UserID:      userID,
		AfterSaleNo: afterSaleNo,
		OrderID:     strings.TrimSpace(orderID),
		OrderNo:     strings.TrimSpace(orderNo),
		EntryKeys:   keys,
		Reason:      reason,
		OccurredAt:  s.now(),
	})
	return err
}

// Release 解冻一张售后单冻住的全部福卡（申请被驳回，或者用户撤销了）。
//
// 找不到这张单的冻结行也是成功：解冻的两条来路各自独立投递、顺序不定，把「没什么可解的」
// 做成错误会让一条正常的重投一路重试到死信。
func (s *AccountService) Release(ctx context.Context, afterSaleNo, reason string) error {
	afterSaleNo = strings.TrimSpace(afterSaleNo)
	if afterSaleNo == "" {
		return ErrInvalidAfterSaleNo
	}
	if strings.TrimSpace(reason) == "" {
		reason = FreezeReasonRejected
	}

	_, err := s.repository.ReleaseAfterSale(ctx, repository.ReleaseParams{
		AfterSaleNo: afterSaleNo,
		Reason:      reason,
		OccurredAt:  s.now(),
	})
	return err
}

// Freezes 分页读冻结，返回 (当页, 总数)。分页兜底与 Entries 同一条理由，见那里的说明。
func (s *AccountService) Freezes(ctx context.Context, q dto.FreezeQuery) ([]*model.FortuneCardFreeze, int, error) {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = defaultPageSize
	}
	if q.PageSize > dto.MaxPageSize {
		q.PageSize = dto.MaxPageSize
	}
	return s.repository.ListFreezes(ctx, q)
}

// trimKeys 清掉键两侧的空白并丢掉空串。
//
// 不去重：同一批里出现两次的键意味着订单域拆重了，那是上游的 bug，而这里悄悄去重会让
// 「发放侧对这个键只认一笔」与「冻结侧以为冻了两笔」对不上。让它原样落库，在金额上现形。
func trimKeys(keys []string) []string {
	trimmed := make([]string, 0, len(keys))
	for _, key := range keys {
		if key = strings.TrimSpace(key); key != "" {
			trimmed = append(trimmed, key)
		}
	}
	return trimmed
}
