// Package client 持有 lottery-service 的出网 gRPC 客户端。
//
// 今天只有一个方向：account-service 的福卡账户。**lottery-service 是 fortune_card.proto
// 的第一个真实调用方**——那份契约写好之后一直只有实现方没有调用方（account-service 自己
// 的集成测试直接调 service 层），所以「契约是不是照着真实需要写的」这件事到今天才第一次
// 被验证。
//
// 边界照方案 §5.7：**Lottery 只拥有抽奖和奖品数据；福卡余额由 Account 负责。** 本服务
// 没有余额列，也不打算有——参与时扣几张、还剩几张，只有账户域说了算。这里存下来的
// fortune_entry_id 是一个**值引用**，用来在事后反查那一笔账变。
//
// 结构上这份是 payment-service/internal/client/account.go 的副本：幂等号怎么传、哪个 gRPC
// 码翻成业务结论、服务令牌走 metadata 而不是请求字段，全仓只有一种语义，换服务时不该
// 重新理解一遍。**差别只在方法名与业务结论的名字上。**
package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	accountv1 "github.com/panda-dev/panda-v2/contracts/proto/account/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrInsufficientFortuneCards：用户的福卡不够参与这一次抽奖。
//
// 它**不是故障**，是「本服务认识的业务结果」：调用方把它翻成一次说明白的失败
// （参与记录 status='failed' / failure_code='insufficient_fortune_cards'），小程序给用户
// 看一句「福卡不够了」。把它当成普通错误会变成一次 5xx，而用户看到的会是「系统繁忙」。
//
// 判据在传输层（哪个 gRPC 码算余额不足），结论名在业务层——service 那边有一个同名的别名
// （service.ErrInsufficientFortuneCards），这样调用方读业务代码时不必知道 gRPC。
var ErrInsufficientFortuneCards = errors.New("fortune card balance is insufficient")

// ErrInvalidDeductRequest：账户域说这次扣减的**参数**不对（负数张数、空 user_id）。
//
// 与余额不足分开：那是用户的事，这是本服务自己的 bug。判据同样是传输层的
// codes.InvalidArgument——把它留在传输层，业务层就只能看见一个普通错误，于是会走「重试」
// 那条路，而一条必然失败的请求重试一万次还是同样的结果。
var ErrInvalidDeductRequest = errors.New("fortune card deduction request was rejected as invalid")

// ErrReverseRefused：账户域拒绝冲正这一笔流水。
//
// 契约里写明了原因：冲正会把余额扣成负数时拒绝，也就是「这张卡已经被别处花掉了」。
// 对抽奖来说这是一条**必须响的警情**——用户的卡在我们这里被扣了、我们没给成参与、还退不
// 回去。它不能等同于「冲正成功」，也不能被吞掉。
var ErrReverseRefused = errors.New("fortune card entry cannot be reversed")

// DeductRequest 是一次参与抽奖的扣减请求。
//
// RequestID 由调用方给，**lottery 侧就是参与记录的 ID**：账户域拿它派生 entry_key，所以
// 同一条参与重试多少次都只扣一笔。这是「崩在第二段与第三段之间」那条路能安全重跑的全部
// 依据——修复 worker 用同一个 request_id 重跑，拿回的是同一笔流水而不是扣第二张卡。
type DeductRequest struct {
	UserID    string
	Amount    int64
	RequestID string
	// Title 是流水上给用户看的那一行文案（「参与抽奖」）。
	Title string
	// 起因对象的三个值引用。ReferenceID 是参与记录 ID，ReferenceNo 是期次号
	// （「LA1B2C3D-0003」），两侧人工对账时靠它们互相对上号。
	ReferenceType string
	ReferenceID   string
	ReferenceNo   string
	Remark        string
}

// DeductResult 是账户域回的三个值。
type DeductResult struct {
	// EntryID 是 fortune_card_entries.id。它落进 lottery_participations.fortune_entry_id，
	// 补偿的冲正与将来的对账都按它反查这笔账变。
	EntryID string
	// BalanceAfter 是这一笔之后的余额。重放时给的是**当时那笔**的余额，不是此刻的。
	BalanceAfter int64
	// Replayed 为 true 表示账户域之前就扣过同一个 request_id 了（我们超时重试，或者上一次
	// 的响应丢了）。**这不是错误**：卡只扣了一张，参与可以照常确认。
	Replayed bool
}

// ReverseResult 是一次冲正的结果。
type ReverseResult struct {
	// EntryID 是**冲正流水**的 ID（不是被冲正的那一笔），落进 reverse_entry_id。
	EntryID      string
	BalanceAfter int64
	Replayed     bool
}

// FortuneCardClient 是 account-service 福卡账户的客户端。
type FortuneCardClient struct {
	cards accountv1.FortuneCardServiceClient
	token string
}

// token 是服务令牌：这几条 RPC 用 auth.WithServiceToken 走 metadata，**不放请求字段**——
// 放进字段就等于允许调用方替别人声明身份（与 payment-service 的 CoffeeBeanClient 同一条
// 理由）。
func NewFortuneCardClient(conn grpc.ClientConnInterface, token string) *FortuneCardClient {
	return &FortuneCardClient{cards: accountv1.NewFortuneCardServiceClient(conn), token: token}
}

// Deduct 扣减福卡。
//
// 只有「余额不足」被翻译成业务结论，其余一律保持普通错误：账户服务不可达、超时、内部报错
// 都是真故障，而且**不能被当成拒绝**——那会把一条其实已经扣了卡的参与标成 failed，用户
// 白丢一张卡。service 那边对它们的处置是「参与停在 pending，交给修复 worker 用同一个
// request_id 重跑」。
//
// InvalidArgument 翻成 ErrInvalidDeductRequest：它意味着我们传错了参数（负数张数、空
// user_id），那是本服务自己的 bug，不是用户的错；service 会把它标成 failed/invalid_request
// 并大声记日志，而不会拿它去重试。
func (c *FortuneCardClient) Deduct(ctx context.Context, req DeductRequest) (DeductResult, error) {
	if c == nil || c.cards == nil {
		return DeductResult{}, errors.New("fortune card client is not configured")
	}
	resp, err := c.cards.DeductFortuneCards(auth.WithServiceToken(ctx, c.token), &accountv1.DeductFortuneCardsRequest{
		UserId:        req.UserID,
		Amount:        req.Amount,
		RequestId:     req.RequestID,
		Title:         req.Title,
		ReferenceType: req.ReferenceType,
		ReferenceId:   req.ReferenceID,
		ReferenceNo:   req.ReferenceNo,
		Remark:        req.Remark,
	})
	if err != nil {
		switch status.Code(err) {
		case codes.FailedPrecondition:
			return DeductResult{}, fmt.Errorf("%w: user %s", ErrInsufficientFortuneCards, req.UserID)
		case codes.InvalidArgument:
			return DeductResult{}, fmt.Errorf("%w: %s", ErrInvalidDeductRequest, status.Convert(err).Message())
		}
		return DeductResult{}, fmt.Errorf("deduct fortune cards for participation %s: %w", req.RequestID, err)
	}
	return DeductResult{
		EntryID:      resp.GetEntryId(),
		BalanceAfter: resp.GetBalanceAfter(),
		Replayed:     resp.GetReplayed(),
	}, nil
}

// Reverse 冲正一笔流水，把卡还回去。
//
// 只用在**补偿路径**上：参与在扣卡通路上被开奖抢先了，用户什么也没得到，卡原路退回。
// 冲正的幂等键由账户域从 entry_id 派生（`reverse:{entryId}`），所以补偿中途崩了重跑也
// 只会退一次。
//
// FailedPrecondition 在这里的含义与 Deduct 那边**不一样**：不是「余额不足」，是「这一笔
// 退不了」（退回去会把余额扣成负数，也就是那张卡已经被别处花掉了）。翻成
// ErrReverseRefused，调用方要把它当成警情而不是当成退成功。
func (c *FortuneCardClient) Reverse(ctx context.Context, entryID, title, remark string) (ReverseResult, error) {
	if c == nil || c.cards == nil {
		return ReverseResult{}, errors.New("fortune card client is not configured")
	}
	resp, err := c.cards.ReverseFortuneCardEntry(auth.WithServiceToken(ctx, c.token),
		&accountv1.ReverseFortuneCardEntryRequest{EntryId: entryID, Title: title, Remark: remark})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return ReverseResult{}, fmt.Errorf("%w: entry %s", ErrReverseRefused, entryID)
		}
		return ReverseResult{}, fmt.Errorf("reverse fortune card entry %s: %w", entryID, err)
	}
	return ReverseResult{
		EntryID:      resp.GetEntryId(),
		BalanceAfter: resp.GetBalanceAfter(),
		Replayed:     resp.GetReplayed(),
	}, nil
}

// Balance 读一个用户的福卡余额，只给抽奖中心显示用（「我还有几张卡」）。
//
// 它**不参与任何判定**：能不能参与由 Deduct 说了算，先看一眼余额只是为了在余额不够时给出
// 一句人话，而不是拿一次失败的扣减去说同一件事。没有账户行就是 0（账户行是第一次发放时
// 懒创建的），契约里写着。
//
// 读失败**不是**「余额为 0」——调用方（service）必须把这两种情况分开，否则抽奖中心会在
// 账户域抖动时对用户说「你没有卡了」。
func (c *FortuneCardClient) Balance(ctx context.Context, userID string) (int64, error) {
	if c == nil || c.cards == nil {
		return 0, errors.New("fortune card client is not configured")
	}
	resp, err := c.cards.GetFortuneCardBalance(auth.WithServiceToken(ctx, c.token),
		&accountv1.GetFortuneCardBalanceRequest{UserId: userID})
	if err != nil {
		return 0, fmt.Errorf("read fortune card balance for user %s: %w", userID, err)
	}
	return resp.GetBalance(), nil
}
