package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
	"google.golang.org/grpc"
)

// ErrFortuneCardServiceUnavailable：账户域没答上来（连不上、内部错误、应答是空的）。
//
// 只有这一种结论。这里没有「被拒绝」那一档：预览是**只读**的，它不会拒绝谁——冻不上
// 几张是一个数，不是一次失败。所以调用方要分开的两件事是「拿到了这个数」与
// 「没拿到」：没拿到时**不能**当成「那就放行」，那会让「卡已经抽掉了还想退钱」这条规则
// 在账户域抖动时静默失效。
var ErrFortuneCardServiceUnavailable = errors.New("fortune card service is unavailable")

// FortuneCardFreezeQuote 是「现在把这批发放冻起来冻得上几张」的答案。
//
// 两个字段是两个不同的事实，调用方必须分开看：
//
//	Granted   这几笔发放还挂着多少张。**0 表示发放还没落库**（先申请退款再完成是支持的
//	          情形），不是「卡被用掉了」。
//	Freezable 此刻真的冻得上的张数：min(Granted, 账户可用)。
//
// 合成一个字段就没法把「还没发」与「发过但已经抽光」分开——两种情况下冻得上的都是 0，
// 而前者该放行、后者该拒。
type FortuneCardFreezeQuote struct {
	Granted   int64
	Freezable int64
}

// FreezeQuoteInput 是问这一句要交出去的事实。
//
// EntryKeys 是订单域按退款范围拆好的发放幂等键（见 repository.fortuneCardFreezeKeys）——
// 「哪张卡是哪一行送的」这个规则只有订单域有，账户域只按键算数。
type FreezeQuoteInput struct {
	UserID    string
	EntryKeys []string
}

// FortuneCardQuoter 问账户域「这一单赠送的福卡现在还冻得上吗」。
//
// 它存在的理由是一条业务规则：一单赠送的福卡一张都没被用过，才允许申请退款。判据只能是
// 这个数——福卡的流水是一口池子，抽奖扣的那一笔从不指向它消耗的是哪一次发放，所以库里
// 没有「这一单那几张还在不在」的直接答案。
type FortuneCardQuoter struct {
	cards   accountv1.FortuneCardServiceClient
	token   string
	timeout time.Duration
}

// NewFortuneCardQuoter 复用调用方那条连接：main 只拨一次，多个调用点共享同一个
// *grpc.ClientConn。
func NewFortuneCardQuoter(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*FortuneCardQuoter, error) {
	if conn == nil {
		return nil, errors.New("fortune card service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("fortune card service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("fortune card service timeout must be positive")
	}
	return &FortuneCardQuoter{
		cards:   accountv1.NewFortuneCardServiceClient(conn),
		token:   token,
		timeout: timeout,
	}, nil
}

// FreezeQuote 问一次预览。带的是**服务令牌**：这一次调用代表订单域去问一个账户事实，
// 不代表某个用户——用户是谁由调用方给（申请退款的那条路上是 token 解出来的本人）。
func (q *FortuneCardQuoter) FreezeQuote(ctx context.Context, in FreezeQuoteInput) (*FortuneCardFreezeQuote, error) {
	if q == nil || q.cards == nil {
		return nil, errors.New("fortune card quoter is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, q.token), q.timeout)
	defer cancel()

	resp, err := q.cards.PreviewFortuneCardFreeze(ctx, &accountv1.PreviewFortuneCardFreezeRequest{
		UserId:    in.UserID,
		EntryKeys: in.EntryKeys,
	})
	if err != nil {
		// 状态码在这里不细分：调用方对这几种的回答是同一个——不受理这次申请，并且
		// **不是**用一句「福卡已使用」去不受理（那是另一个结论，只有拿到数才敢下）。
		return nil, ErrFortuneCardServiceUnavailable
	}
	if resp == nil {
		// 应答是空的：当成没问到，而不是把两个 0 当成「冻不上」。前者会让调用方拒绝申请
		// 并说「稍后再试」，后者会说「你的卡已经用过了」——一句冤枉用户的话。
		return nil, ErrFortuneCardServiceUnavailable
	}
	return &FortuneCardFreezeQuote{Granted: resp.GetGranted(), Freezable: resp.GetFreezable()}, nil
}
