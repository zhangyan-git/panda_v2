package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 支付服务拒绝或答不上来这次发起时的四种情况。它们分开存在，是因为**调用方该做的事不同**：
//
//	ErrPaymentRejected            改请求或者换一种支付方式，重发一模一样的一次没用
//	ErrPaymentConflict            带着新的幂等号重发，或者等一会儿再试同一把钥匙
//	ErrPaymentUncertain           稍后重试（这次不算失败，渠道那边可能已经有单了）
//	ErrPaymentServiceUnavailable  等一会儿再试；这是故障，不是业务结论
//
// 前两个是「有结论」，后两个是「没结论」。把没结论的当成失败告诉用户「支付失败」是这里
// 最容易犯的错：用户会以为钱没出去，于是换一种方式再付一次——而渠道那边那张预支付单
// 可能仍然有效。
var (
	// ErrPaymentRejected：支付服务不接受这次请求。可能是支付方式 ID 不是合法的 uuid、
	// 订单 ID 缺失或不是 uuid、这个方式不存在/被停用/没有关联渠道。**余额不足也走这里**
	// （咖啡豆不够时支付侧回 FailedPrecondition，那是一次「有结论」的拒绝，客户端换一种
	// 方式重试是对的）。
	ErrPaymentRejected = errors.New("payment service rejected the request")
	// ErrPaymentConflict：同一个幂等号的上一笔还在跑（Aborted），或者那把钥匙被换过
	// 请求体（AlreadyExists）。两种都是「换一把钥匙重来」，不是「这单不能付」。
	ErrPaymentConflict = errors.New("payment request conflicts with an earlier attempt")
	// ErrPaymentUncertain：**得不出结论**。渠道超时或结果不明（支付侧回 Unavailable），
	// 或者我们这次调用的 deadline 先到了。支付单不会停在 failed——它停在 created，
	// 由支付侧的超时关单收走，所以这次发起必须能被重试。
	ErrPaymentUncertain = errors.New("payment result is uncertain")
	// ErrPaymentServiceUnavailable：支付服务没答上来（连不上、内部错误、应答是空的）。
	ErrPaymentServiceUnavailable = errors.New("payment service is unavailable")
)

// CreatePaymentInput 是发起一次支付要交给支付服务的事实。
//
// subject / attach / walletOpenID 都没有：它们要么由支付侧按渠道规则拼，要么本轮没有任何
// 渠道消费得了。往一个没人读的字段里塞值，只会让人以为它被用上了。
type CreatePaymentInput struct {
	// OrderID 是 orders.id。它和 OrderNo 指的是同一张订单，之所以两个都给：账户出资
	// （咖啡豆）的幂等键由**订单 ID** 派生（账户域按 `order:{orderId}` 建键，见
	// account v1 的 coffee_bean.proto）。用订单 ID 而不是支付单号做那把键，用户重试时
	// 就算拿到了一张新的支付单，同一张订单的豆也只扣一次。
	OrderID string
	OrderNo string
	UserID  string
	// Amount 是应付总额，单位为分，**由订单权威给出**：支付服务不读订单库，它拿到的就是
	// 这个值。所以这个值必须是订单的行上算好的 payable_amount，不能是调用方传上来的数。
	Amount          int64
	PaymentMethodID string
	// RequestID 是这次发起的幂等号，原样交给支付服务做支付单的幂等键。
	RequestID string
}

// PaymentCreator 向支付服务发起一次支付。
//
// 带的是**服务令牌**（platform/auth.WithServiceToken）：这一次调用代表订单域去告诉支付域
// 一个事实，不代表某个用户。用户的身份已经在这之前用过了——归属校验发生在 service 层，
// 用的是他自己那张 access token 解出来的 userID。这里再传一次没有意义，也没有一个能装的
// 字段。
type PaymentCreator struct {
	payments paymentv1.PaymentServiceClient
	token    string
	timeout  time.Duration
}

// NewPaymentCreator 复用调用方那条连接：main 只拨一次，多个调用点共享同一个 *grpc.ClientConn。
func NewPaymentCreator(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*PaymentCreator, error) {
	if conn == nil {
		return nil, errors.New("payment service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("payment service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("payment service timeout must be positive")
	}
	return &PaymentCreator{payments: paymentv1.NewPaymentServiceClient(conn), token: token, timeout: timeout}, nil
}

// Create 发起一次支付。
//
// 返回的是 dto.PayAction 而不是这个包里自己的一个结构，与 DeviceReader.Get 的做法不同：
// Device 是一次**投影**（20 个字段里只有 6 个影响下单校验），而这里是**原样透传**——
// 那几个字段就是回到客户端的 JSON 契约本身。在这里再抄一份，等于给「少搬一个字段」留了
// 一个静默失效的口子：客户端拿不到 payParams，收银台是空的，而没有任何一层会报错。
func (p *PaymentCreator) Create(ctx context.Context, in CreatePaymentInput) (*dto.PayAction, error) {
	if p == nil || p.payments == nil {
		return nil, errors.New("payment creator is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, p.token), p.timeout)
	defer cancel()
	resp, err := p.payments.CreatePayment(ctx, &paymentv1.CreatePaymentRequest{
		OrderId:         in.OrderID,
		OrderNo:         in.OrderNo,
		UserId:          in.UserID,
		Amount:          in.Amount,
		PaymentMethodId: in.PaymentMethodID,
		RequestId:       in.RequestID,
		// subject / wallet_open_id / attach 有意留空，见 CreatePaymentInput 的注释。
	})
	if err != nil {
		return nil, mapPaymentError(err)
	}
	if resp == nil || resp.GetPaymentNo() == "" {
		// 应答里没有支付单号，这不可能是一次正常回答。当成「没问到」而不是把一个空支付单
		// 交给客户端：客户端拿它去调起支付只会得到一句渠道的报错，而我们就此丢掉了一个
		// 能查的支付单号。
		return nil, fmt.Errorf("%w: payment service returned an empty payment", ErrPaymentServiceUnavailable)
	}
	// 空的 payParams 在这里被补成空对象。gRPC 的 map 字段分不出「空」与「没有」——支付侧
	// 那边两种都收敛成同一个空 map（create.go 的 markPending 与 createAccountPayment 都
	// 显式写过 map[string]string{}），但到了这一侧 GetPayParams() 一律回 nil，序列化出去
	// 就是 JSON 的 null。而 dto.PayAction 对客户端承诺过这里**不是** null：null 与 {} 是
	// 两种读法（前者是 undefined）。账户出资就是那个正好没有渠道参数的常态——扣豆成功、
	// 客户端什么都不用做，它拿到的必须是一个能直接读的空对象。
	payParams := resp.GetPayParams()
	if payParams == nil {
		payParams = map[string]string{}
	}
	return &dto.PayAction{
		PaymentNo:      resp.GetPaymentNo(),
		Status:         resp.GetStatus(),
		Action:         resp.GetAction(),
		PayParams:      payParams,
		ExpiresAtUnix:  resp.GetExpiresAtUnix(),
		FailureCode:    resp.GetFailureCode(),
		FailureMessage: resp.GetFailureMessage(),
	}, nil
}

// mapPaymentError 把 gRPC 状态码翻成上面那四个结论。
//
// 判据是「调用方该做什么」，与支付侧 createError 的分法一一对应——那边怎么分的，这里就
// 怎么收，中间不重新解释一遍。**只有「有结论」的那两档（Rejected / Conflict）带回状态
// 消息**：那些消息是支付侧精心写成「分得清该怎么办」的固定句子（见
// payment-service/internal/rpc/payment.go），不是原始错误文本。另外两档不回传，只留一个
// 我们知道是什么的结论——它们说的话对调用方没有可执行的信息。
func mapPaymentError(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrPaymentRejected, status.Convert(err).Message())
	case codes.Aborted, codes.AlreadyExists:
		return fmt.Errorf("%w: %s", ErrPaymentConflict, status.Convert(err).Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		// DeadlineExceeded 是我们自己那条 5 秒的线。它也归「没结论」：支付服务可能已经建好了
		// 支付单正在问渠道，而我们已经把这次调用放弃了。重发会命中同一个幂等号，拿回那一张。
		return ErrPaymentUncertain
	case codes.Unimplemented:
		// 「这一档我们这边压根没有」——**不是业务结论**。它今天是版本错配：调用方比支付服务
		// 新，喊了一个对面还没实现的 RPC（见 payment-service/internal/rpc/payment.go 的包注释：
		// 只有 CreatePayment 是真实现的）。归进 ErrPaymentRejected 的话，客户端会收到
		// 「这个支付方式现在用不了，让用户换一种方式」——而用户换哪一种都一样，真正该做的是
		// 把部署对齐。回 503，让它长得像一次故障，因为它就是。
		return ErrPaymentServiceUnavailable
	default:
		return ErrPaymentServiceUnavailable
	}
}
