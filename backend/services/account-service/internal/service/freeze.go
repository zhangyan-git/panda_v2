package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/repository"
)

// FreezeReasonApplied / FreezeReasonRejected / FreezeReasonCancelled / FreezeReasonRefundFailed
// / FreezeReasonRecovered 是冻结行上那句原因。
//
// 文案归账户域（与流水上的 title 同一个理由）：说的是「为什么这张卡现在不能用」，
// 而订单域给的是起因——一张售后单，不是一句给用户看的字。
const (
	FreezeReasonApplied   = "退款申请中"
	FreezeReasonRejected  = "退款申请被驳回"
	FreezeReasonCancelled = "用户撤销了退款申请"
	// FreezeReasonRefundFailed 是钱没退成：卡原样放回去。
	//
	// 少了它，退款失败的单会把卡永远冻着——而用户可以重新申请，重开的那张冻结行因为
	// 在途冻结已经吃光可用，只冻得到 0 张，于是第二次退款成功时一张都追不回来。
	FreezeReasonRefundFailed = "退款失败"
	// FreezeReasonRecovered 是钱退成了：冻着的卡收回来。追不回来的张数由仓储追加在它后面。
	FreezeReasonRecovered = "退款成功，福卡已追回"
)

// titleReverseRecover 是追回写下的那几笔冲正流水的文案。
//
// 与 titleReverse（通用的「冲正」）分开：客服看到「冲正」只会知道有人冲了一笔，看不到
// 为什么冲；这一笔的起因永远是同一件事，就写在文案里。
const titleReverseRecover = "退款追回"

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

// PreviewFreeze 回答「现在把这批发放冻起来冻得上几张」，只算不写。
//
// 订单域在**受理退款申请之前**问这一句。业务规则是「一单赠送的福卡一张都没被用过才允许
// 申请退款」，而福卡的流水是一口池子（抽奖那一笔只记 `draw:{requestId}`，从不指向它消耗
// 的是哪一次发放），所以「这一单那几张还在不在」在库里没有直接答案。能算出来的就是这个
// 数，判据因此只能是它。
//
// 两个返回值要分开看，别合成一个：granted 为 0 是「发放还没落库」（申请早于发放是支持的
// 情形），不是「卡用掉了」；freezable 小于承诺张数才是。合成一个的话，「先申请后退款、
// 事后再完成」这条正常路径会被拒掉。判定是订单域的规则，本服务只给这两个事实。
func (s *AccountService) PreviewFreeze(ctx context.Context, userID string, entryKeys []string) (granted, freezable int64, err error) {
	userID = strings.TrimSpace(userID)
	if _, err := uuid.Parse(userID); err != nil {
		return 0, 0, ErrInvalidUserID
	}

	return s.repository.PreviewFreezeAfterSale(ctx, repository.PreviewFreezeParams{
		UserID:    userID,
		EntryKeys: trimKeys(entryKeys),
	})
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

// Recover 追回一张售后单冻住的福卡：钱退成了，这一单送出去的卡从账上收回来。
//
// 与 Release 是**相反**的两件事，别混：解冻是把冻着的卡放回可用（余额不动），追回是把它们
// 从余额里真的扣掉。所以退款失败走 Release、退款成功走这里。
//
// 找不到冻结行、或者已经解冻过，都是**成功**——与 Release 同一条取舍：重投、乱序、
// 「驳回事件比退款成功事件晚到」都会撞上这两种情况，把它们做成错误只会让一条正常的消息
// 重试到死信，而它要表达的事早就完成了。
//
// 返回追回的张数（可能小于冻结额：申请退款之前就被抽掉的卡追不回来）。追不回来的部分
// 写在冻结行的 reason 上，调用方不需要为它做任何补偿。
func (s *AccountService) Recover(ctx context.Context, afterSaleNo string, occurredAt time.Time) (int64, error) {
	afterSaleNo = strings.TrimSpace(afterSaleNo)
	if afterSaleNo == "" {
		return 0, ErrInvalidAfterSaleNo
	}
	if occurredAt.IsZero() {
		occurredAt = s.now()
	}

	return s.repository.RecoverAfterSale(ctx, repository.RecoverParams{
		AfterSaleNo: afterSaleNo,
		Title:       titleReverseRecover,
		Remark:      "售后单 " + afterSaleNo + " 退款成功",
		Reason:      FreezeReasonRecovered,
		OccurredAt:  occurredAt,
	})
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
