package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 退款这条路上的四种结论。分法与发起支付那几个（见 payment.go）**刻意同形**，因为调用方
// 该做的事完全一样；分开命名是因为「支付服务拒绝了这笔退款」与「支付服务拒绝了这次支付」
// 在日志与后台提示里必须分得清，共用一组名字会让一次退款失败看起来像一次支付失败。
//
//	ErrRefundRejected            这笔钱现在退不了（不可退状态、超过可退余额、渠道不支持）。
//	                             改请求或者人工处理，重发一模一样的一次没用。
//	ErrRefundConflict            上一笔退款还在推进（并发路径抢先了）。稍后重试同一张售后单。
//	ErrRefundUncertain           **得不出结论**：渠道超时或结果不明，或者我们这条调用的
//	                             deadline 先到了。退款单不会因此变 failed——它停在 pending 或
//	                             processing，重发同一个售后单号会接着走。
//	ErrRefundServiceUnavailable  支付服务没答上来。是故障，不是业务结论。
var (
	// ErrRefundRejected：支付服务不接受这次退款。它对应 gRPC 的 InvalidArgument /
	// NotFound / FailedPrecondition 三档——**支付单还没收妥、金额超过可退余额、这个渠道
	// 不支持退款**都走这里，它们对调用方是同一句话：这笔钱现在退不了。
	ErrRefundRejected = errors.New("payment service rejected the refund")
	// ErrRefundConflict：这张退款单正被另一条路径推进（Aborted），或者那把售后单号被换过
	// 请求体（AlreadyExists）。
	ErrRefundConflict = errors.New("refund request conflicts with an earlier attempt")
	// ErrRefundUncertain：**得不出结论**。退款单还在支付侧手上（pending/processing），重发
	// 同一个售后单号是安全的，而且正是这条路上唯一该做的事。
	ErrRefundUncertain = errors.New("refund result is uncertain")
	// ErrRefundServiceUnavailable：支付服务没答上来（连不上、内部错误、应答里没有退款单号）。
	ErrRefundServiceUnavailable = errors.New("payment service is unavailable")
)

// CreateRefundInput 是发起一次退款要交给支付服务的事实。
//
// 比 CreatePaymentInput 短得多，因为**钱的来路已经记在支付域那边了**：payments 上那张单
// 带着 order_no 与 user_id，退款单要的那两栏从它取（见 proto 里 CreateRefundRequest 的
// 说明）。这里多出来的只有「退哪一张售后单、退多少」，而它们都是 order 库的事实。
type CreateRefundInput struct {
	// AfterSaleNo 是售后单号，值引用，**同时是幂等键**：payment_refunds.after_sale_no
	// 整表唯一，一张售后单最多落一张退款单。重发同号拿回的是同一张，不会再退一次钱。
	AfterSaleNo string
	// PaymentNo 是要退的那张支付单（orders.payment_no）。必须是已收妥的那一笔。
	PaymentNo string
	// Amount 是退款总额，单位为分，**由订单权威给出**（售后单上算好的 refund_amount）。
	// 支付服务只校验它不超过那张支付单的可退余额，不读订单库。
	Amount int64
	Reason string
	// OrderLineID 是 order_lines.id，值引用；整单退为空串。
	//
	// 用 string 而不是 *string：proto 那边是不可空字段，而「整单退」在这里的表示就是空串
	// （支付侧按 NULLIF 落成 NULL）。多一层指针只会让每个调用点都要判一次 nil。
	OrderLineID string
}

// RefundOutcome 是支付服务对一次发起退款的答复。
//
// status 取值 pending / processing / succeeded / failed（见 proto 的 CreateRefundResponse）。
// **pending 与 processing 都不是失败**：它们表示这笔退款单已经建好了，只是渠道还没给结论，
// 支付侧的退款查询任务在跟。调用方该做的是把售后单推到 refunding 然后等事件。
type RefundOutcome struct {
	RefundNo         string
	Status           string
	ProviderRefundID string
	FailureCode      string
	FailureMessage   string
}

// CreateRefund 为一笔已收妥的支付发起退款。
//
// 它长在 PaymentCreator 上而不是另起一个类型：**同一个下游服务、同一条连接、同一枚服务令牌**
// （见 main 里那一次 Dial）。另起一个类型就要再写一遍构造与校验，而那两处唯一会分叉的地方
// 只是错误文案。
//
// 令牌是服务令牌：这一次调用代表订单域去告诉支付域「这张售后单已经批了，把钱退回去」，
// 不代表某个用户——同意退款的那个人是后台的审核人，他的身份已经写进 order 库的审核流水与
// 平台审计里了（见 repository.ReviewAfterSale）。
func (p *PaymentCreator) CreateRefund(ctx context.Context, in CreateRefundInput) (*RefundOutcome, error) {
	if p == nil || p.payments == nil {
		return nil, errors.New("payment creator is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, p.token), p.timeout)
	defer cancel()
	resp, err := p.payments.CreateRefund(ctx, &paymentv1.CreateRefundRequest{
		AfterSaleNo: in.AfterSaleNo,
		PaymentNo:   strings.TrimSpace(in.PaymentNo),
		Amount:      in.Amount,
		Reason:      in.Reason,
		OrderLineId: in.OrderLineID,
		// request_id / trace_id 不在契约里：支付侧从 gRPC 元数据里的 trace 取（见
		// payment-service/internal/rpc/refund.go）。多一个字段就多一处能对不上的地方。
	})
	if err != nil {
		return nil, mapRefundError(err)
	}
	if resp == nil || resp.GetRefundNo() == "" {
		// 应答里没有退款单号，这不可能是一次正常回答。当成「没问到」而不是把一张空单交给
		// 售后流程：售后单会被推进 refunding 而 refund_no 是空的，那之后就再也对不上账了
		// （见 dto.RefundEventPayload 里为什么用售后单号而不是退款单号做主键）。
		return nil, fmt.Errorf("%w: payment service returned an empty refund", ErrRefundServiceUnavailable)
	}
	return &RefundOutcome{
		RefundNo:         resp.GetRefundNo(),
		Status:           resp.GetStatus(),
		ProviderRefundID: resp.GetProviderRefundId(),
		FailureCode:      resp.GetFailureCode(),
		FailureMessage:   resp.GetFailureMessage(),
	}, nil
}

// mapRefundError 把 gRPC 状态码翻成上面那四个结论。
//
// 与 mapPaymentError 同一张分档表、同一条理由（判据是「调用方该做什么」），但**Unimplemented
// 那一档的注释在这里更硬**：退款这条 RPC 是后加的，一个还没升级的支付服务会回 Unimplemented，
// 而那必须是一次 503 故障、不是「这笔钱退了不」。归进 Rejected 的话，客服看到的是「这笔退款
// 被支付服务拒绝」，而正确结论是「部署没对齐」。
func mapRefundError(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrRefundRejected, status.Convert(err).Message())
	case codes.Aborted, codes.AlreadyExists:
		return fmt.Errorf("%w: %s", ErrRefundConflict, status.Convert(err).Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		// DeadlineExceeded 是我们自己那条线。它也归「没结论」：支付侧可能已经建好退款单、
		// 正在问渠道，而我们已经把这次调用放弃了。重发同一个售后单号会命中幂等拿回那一张。
		return ErrRefundUncertain
	default:
		return ErrRefundServiceUnavailable
	}
}
