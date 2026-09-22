package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
	"google.golang.org/grpc"
)

// ErrPaymentNotFound：按支付单号查支付域查无此单。它与 ErrAgreementNotFound 一样是**一次结论**
// ——调用方手里那个号来自订单域（订单上的 payment_no），对不上说明那条链上有一行数据坏了。
//
// 单开一个哨兵而不是复用 ErrAgreementNotFound：两者说的是**不同的东西不存在**（一份代扣协议 vs
// 一张支付单），而丢掉这个区别的代价是排查时看着「协议不存在」去找一个协议号——那句话在这条路上
// 一个字都不沾（这里根本没有协议号参与）。
var ErrPaymentNotFound = errors.New("payment not found")

// ChargeReader 是支付域在**后台订阅详情**这条路上的两个纯读。
//
// # 它为什么与 AgreementClient 分开一个类型
//
// AgreementClient 那四个动作全是**写**（连名字像读的 Query 也是写：它按渠道的原话改支付库那一行），
// 而这两个是名副其实的读。分开之后，「这条链路上哪些调用会改别的域的状态」在目录上一眼可见
// ——这正是 dto 那一层分成两个文件的同一条理由。
//
// # 它与 AgreementClient 仍共用一条连接与同一个令牌
//
// 两个类型都只是同一个 *grpc.ClientConn 上的两个 stub 视图（见 main 的装配）：分开的是**能力**，
// 不是连接。带的是服务令牌（platform/auth.WithServiceToken）——它读的是别人（用户）的数据，
// 不是调用者自己的，所以不能带用户令牌。
//
// # 失败一律由调用方降级，本层不吞错
//
// 这里照常把四档结论回出去（见 mapReadError），降级那一件事发生在服务层（见 subscriptionDetail）：
// 一个客户端替调用方决定「这个错不重要」会让它永远看不见真正的故障。
type ChargeReader struct {
	payments paymentv1.PaymentServiceClient
	token    string
	timeout  time.Duration
}

// NewChargeReader 复用调用方那条连接（与 NewAgreementClient 同一个 *grpc.ClientConn）。
// 三个参数都拒空，理由同那一处。
func NewChargeReader(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*ChargeReader, error) {
	if conn == nil {
		return nil, errors.New("payment service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("payment service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("payment service timeout must be positive")
	}
	return &ChargeReader{payments: paymentv1.NewPaymentServiceClient(conn), token: token, timeout: timeout}, nil
}

// ListAgreementCharges 读一份协议下的全部期次，支付域那边按期次升序给（那正是后台「续费明细」
// 要的顺序）。
//
// # 三种「没有期次」必须分清楚
//
//	协议号为空        调用方的请求坏了（本层直接回 ErrAgreementRejected，不打支付域）
//	查无此约          ErrAgreementNotFound —— 订阅行上的协议号在支付域不存在，那是本服务写坏的
//	有约束、没期次    空列表 —— 正常：还没到第一个扣款日
//
// 把后两者合成一个空列表就是这一层最容易犯的错：一次「协议号压根不存在」会被显示成「暂无续费
// 记录」，而真相是这一行数据有问题、要有人去看。
func (c *ChargeReader) ListAgreementCharges(ctx context.Context, agreementNo string) ([]dto.AgreementCharge, error) {
	if c == nil || c.payments == nil {
		return nil, errors.New("charge reader is not configured")
	}
	trimmed := strings.TrimSpace(agreementNo)
	if trimmed == "" {
		// 请求不合法，不打支付域：那边对空协议号也是 InvalidArgument，但这多一次跨服务往返换来的
		// 是一模一样的结论（见 payment-service 的分档注释）。
		return nil, fmt.Errorf("%w: agreement number is required", ErrAgreementRejected)
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.payments.ListAgreementCharges(ctx, &paymentv1.ListAgreementChargesRequest{AgreementNo: trimmed})
	if err != nil {
		return nil, c.mapReadError(err)
	}
	if resp == nil {
		return nil, fmt.Errorf("%w: payment service returned an empty charge list", ErrPaymentUnavailable)
	}
	// **回非 nil 的空切片**：nil 序列化成 JSON 的 null，而前端那一块要的是「一个空的数组」——
	// 它照着数组写渲染，null 会把它打回错误分支。
	charges := make([]dto.AgreementCharge, 0, len(resp.GetCharges()))
	for _, charge := range resp.GetCharges() {
		if charge == nil {
			continue
		}
		charges = append(charges, dto.AgreementCharge{
			BizPeriod:             charge.GetBizPeriod(),
			Amount:                charge.GetAmount(),
			Status:                charge.GetStatus(),
			AttemptCount:          charge.GetAttemptCount(),
			NextRetryAt:           charge.GetNextRetryAt(),
			ChargedAt:             charge.GetChargedAt(),
			CreatedAt:             charge.GetCreatedAt(),
			ProviderTransactionID: charge.GetProviderTransactionId(),
			FailureCode:           charge.GetFailureCode(),
			FailureMessage:        charge.GetFailureMessage(),
		})
	}
	return charges, nil
}

// GetPayment 读一张支付单的摘要。后台要的是里面那个渠道流水号（首月那一笔的微信流水）。
//
// 查无此单回 ErrPaymentNotFound、**不是一个空的摘要**：调用方拿到的那个号来自订单域，对不上说明
// 那条链上有一行数据坏了（与 OrderClient.GetOrder 同一条取舍）。而支付单号为空在这里是**不合法
// 的请求**而不是「没有这个时刻」——它不是可选项，是调用方拼出来的。
func (c *ChargeReader) GetPayment(ctx context.Context, paymentNo string) (*dto.PaymentSummary, error) {
	if c == nil || c.payments == nil {
		return nil, errors.New("charge reader is not configured")
	}
	trimmed := strings.TrimSpace(paymentNo)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: payment number is required", ErrAgreementRejected)
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.payments.GetPayment(ctx, &paymentv1.GetPaymentRequest{PaymentNo: trimmed})
	if err != nil {
		mapped := c.mapReadError(err)
		// NotFound 在两条读上指的是两件不同的东西（一份协议 / 一张支付单），而共用的那张映射表
		// 只能给一个名字。这里把它换成这条路上的那一个——**只在这一处换**，映射表本身不动：
		// 两个哨兵的区别只对调用方有意义，对状态码没有影响。
		if errors.Is(mapped, ErrAgreementNotFound) {
			return nil, ErrPaymentNotFound
		}
		return nil, mapped
	}
	if resp == nil || strings.TrimSpace(resp.GetPaymentNo()) == "" {
		// 「答了一张没单号的支付单」与「什么都没答」在这里的处置相同：都是没问到。回一个单号为空
		// 的摘要会让调用方以为查到了，然后拿着一片空白去显示。
		return nil, fmt.Errorf("%w: payment service returned an empty payment", ErrPaymentUnavailable)
	}
	return &dto.PaymentSummary{
		PaymentNo:             resp.GetPaymentNo(),
		Status:                resp.GetStatus(),
		Amount:                resp.GetAmount(),
		PaymentMethod:         resp.GetPaymentMethod(),
		ProviderTransactionID: resp.GetProviderTransactionId(),
		// 三个时刻照原样搬，不在这一层解成时间（见 dto.PaymentSummary 的说明）。
		PaidAt:  resp.GetPaidAt(),
		OrderNo: resp.GetOrderNo(),
	}, nil
}

// mapReadError 把 gRPC 状态码翻成结论。判据与 mapAgreementError 逐字相同（**调用方接下来该做
// 什么**），四个哨兵也复用同一组：这两个方法读的是同一个域，四种答复的语义在读写两侧一致。
//
// 唯一的差别在 NotFound 那一档之外没有差别——协议不存在与支付单不存在分别是两个哨兵，由两个
// 方法各自的那个语义决定，所以这里不替它选，只在 Unimplemented 那一档保留「对面没有这个调用」
// 的说法（版本错配，与 mapAgreementError 同一处理）。
func (c *ChargeReader) mapReadError(err error) error {
	return mapAgreementError(err)
}
