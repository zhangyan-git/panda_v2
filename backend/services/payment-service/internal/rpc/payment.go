// Package rpc 实现 payment-service 的 gRPC 面。包名用 rpc 而不是 grpc，
// 以免遮蔽 google.golang.org/grpc。
//
// 这一层是**薄的**：鉴权、把 proto 消息翻成业务入参、把业务错误翻成状态码，别的什么都不做。
// 它持的是同一个 *service.PaymentService，与 HTTP 回调那条路共用一套业务规则——两条路各写
// 一份规则是「同一个动作在不同入口行为不同」的来源。
package rpc

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PaymentService 是支付服务对内的 gRPC 面。
//
// 只实现 CreatePayment。方案里 payment 的其余内部契约（查单、关单、退款）现在都没有真实
// 调用方，一律落到嵌入的 Unimplemented 实现上回 codes.Unimplemented——这与 merchant-service、
// coffee-machine-service 的做法一致：内部契约在有调用方之前不发明一套。
//
// Unimplemented 在调用方那一侧**算故障不算业务结论**（order-service 的 mapPaymentError
// 把它归进「没结论」）：它今天实际出现只有一个原因——调用方比本服务新，喊了一个还没实现的
// 方法。那不是用户能改的事，所以不该让客户端把它读成「这个支付方式用不了」。
type PaymentService struct {
	paymentv1.UnimplementedPaymentServiceServer

	payments *service.PaymentService
}

func NewPaymentService(payments *service.PaymentService) *PaymentService {
	return &PaymentService{payments: payments}
}

// CreatePayment 是 order-service 编排发起支付时走的那条路。
//
// 金额由调用方（order-service，在锁内校验完订单之后）权威给出，本服务不读订单库、也不对着
// 订单库复核——订单事实的归属方是订单服务（方案 5.9 给支付服务的职责清单里本来就没有
// 「读订单」这一项）。
//
// **渠道拒绝不是这个 RPC 的错误**：它返回 status='failed' 与 failure_code/message，与契约里
// 那句「failed = 发起即失败（渠道拒绝、参数不全），客户端可以换方式重试」对应。返回 error
// 会让 order-service 把一次正常的业务拒绝当成服务端故障去重试。真正的 error 只有两类：
// 请求不合法，以及我们**得不出结论**（渠道没给出确定的答复，见 service.ErrProviderResultUncertain）。
func (s *PaymentService) CreatePayment(ctx context.Context, req *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.payments.CreatePayment(ctx, service.CreateRequest{
		OrderID:         req.GetOrderId(),
		OrderNo:         req.GetOrderNo(),
		UserID:          req.GetUserId(),
		Amount:          req.GetAmount(),
		PaymentMethodID: req.GetPaymentMethodId(),
		Subject:         req.GetSubject(),
		RequestID:       req.GetRequestId(),
		WalletOpenID:    req.GetWalletOpenId(),
		Attach:          req.GetAttach(),
		// 只进日志与 payment_provider_calls，不参与幂等哈希——trace 每条请求都不同，
		// 把它算进哈希会让同一个 request_id 的每次重试都变成一次「换了请求体」的冲突。
		TraceID: audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		return nil, createError(err)
	}
	return &paymentv1.CreatePaymentResponse{
		PaymentNo:      result.PaymentNo,
		Status:         result.Status,
		Action:         result.Action,
		PayParams:      result.PayParams,
		ExpiresAtUnix:  result.ExpiresAtUnix,
		FailureCode:    result.FailureCode,
		FailureMessage: result.FailureMessage,
	}, nil
}

// createError 把业务错误翻成 gRPC 状态码。
//
// 判据是「调用方该做什么」，不是「错误有多严重」：
//
//	InvalidArgument         请求本身不合法，改了才能成
//	NotFound                引用的支付方式不存在（对方传错了 id）
//	FailedPrecondition      存在但当前不可用（运营停用了、这个 action 起不了支付）——
//	                        重试多少次都一样，得先改配置
//	Aborted                 同一个幂等键的上一笔还在跑，退避重试
//	AlreadyExists           幂等键被换过请求体，重试也没用
//	Unavailable             我们得不出结论（渠道超时/结果不明），稍后可能成
//	Internal                我们自己的故障（渠道没注册、密钥读不到、数据库报错）
//
// **状态消息不回传原始错误文本**：里面的表名、SQL 片段、渠道配置不该跨服务边界跑。
// 排查要用的信息在服务端日志里，调用方拿到的是一句分得清该怎么办的话。
func createError(err error) error {
	switch {
	case service.IsValidationError(err):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, service.ErrPaymentMethodNotFound), errors.Is(err, service.ErrPaymentNotFound):
		return status.Error(codes.NotFound, "payment method not found")
	case errors.Is(err, service.ErrPaymentMethodInactive),
		errors.Is(err, service.ErrMethodChannelMissing),
		errors.Is(err, service.ErrUnauthorizedAction):
		return status.Error(codes.FailedPrecondition, "payment method is not available")
	case errors.Is(err, service.ErrIdempotencyInProgress):
		return status.Error(codes.Aborted, "the same request is still being processed")
	case errors.Is(err, service.ErrIdempotencyConflict):
		return status.Error(codes.AlreadyExists, "the idempotency key was used with a different request")
	case errors.Is(err, service.ErrProviderResultUncertain):
		return status.Error(codes.Unavailable, "payment provider did not return a definite result")
	case errors.Is(err, service.ErrPaymentNotPending):
		// 并发下另一条路径已经把这单推进走了（渠道回调快过我们的第二段事务）。
		// 调用方重发会命中幂等回放拿到那张单，所以是可重试的。
		return status.Error(codes.Aborted, "the payment was already advanced by another path")
	default:
		return status.Error(codes.Internal, "payment service error")
	}
}
