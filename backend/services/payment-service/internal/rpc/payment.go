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
// 实现 CreatePayment 与 CreateRefund（见 refund.go）。方案里 payment 的其余内部契约（查单、
// 关单）现在都没有真实调用方，一律落到嵌入的 Unimplemented 实现上回 codes.Unimplemented
// ——这与 merchant-service、coffee-machine-service 的做法一致：内部契约在有调用方之前不发明一套。
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

// GetPayment 读一张支付单的摘要：调用方（membership-service）拿它把订单上的支付单号换成渠道
// 流水号，渲染后台「包月订阅」详情里的首月支付信息那一块。
//
// # 这个 RPC 存在的理由就是 provider_transaction_id 那一格
//
// 全仓只有本服务对外给得出渠道流水号：order-service 的 GetOrder 只给 payment_no（对账凭据
// 不属于订单域，见那边的说明）。所以它的形状由那个用途定——给的是几格摘要，不是一整张支付单
// （attach、expires_at、account_entry_id 那些是编排信息，这里的消费者都没有）。
//
// # 分档
//
//	InvalidArgument  支付单号为空。**这不是 NotFound**：设备单与会员续费单的订单上
//	                 payment_no 就是空的（那两条路上没有支付单），调用方拿着空值过来问，
//	                 说明它把「这条路没有支付单」误当成了「来查一下」（见 service.GetPayment）。
//	NotFound         这张支付单不在我们库里。调用方手里那个号抄自订单行，对不上说明那行
//	                 数据坏了，而不是「这单还没付」。
//	Internal         我们自己的故障。
//
// 只认服务令牌：后台读支付数据走的是它自己那棵树，不经过这里。
func (s *PaymentService) GetPayment(ctx context.Context, req *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	payment, err := s.payments.GetPayment(ctx, req.GetPaymentNo())
	if err != nil {
		return nil, serviceError(err)
	}
	return &paymentv1.GetPaymentResponse{
		PaymentNo:             payment.PaymentNo,
		Status:                payment.Status,
		Amount:                payment.Amount,
		PaymentMethod:         payment.PaymentMethod,
		ProviderTransactionId: payment.ProviderTransactionID,
		// 空串而不是零值时间：未成功的支付单 paid_at 是 NULL，编成 `0001-01-01T00:00:00Z`
		// 在界面上就是一个公元元年的日期（见 formatTime 的说明）。
		PaidAt:  formatTime(payment.PaidAt),
		OrderNo: payment.OrderNo,
	}, nil
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
		OrderID:       req.GetOrderId(),
		OrderNo:       req.GetOrderNo(),
		UserID:        req.GetUserId(),
		Amount:        req.GetAmount(),
		PaymentMethod: req.GetPaymentMethod(),
		Subject:       req.GetSubject(),
		RequestID:     req.GetRequestId(),
		WalletOpenID:  req.GetWalletOpenId(),
		Attach:        req.GetAttach(),
		// 分账的三个维度：范围两个、业务分类一个。它们只用来在建支付单那一刻命中分账规则，
		// 不进幂等哈希——同一笔订单重发时，这几个值本来就该是一样的。
		StoreID:  req.GetStoreId(),
		DeviceID: req.GetDeviceId(),
		BizType:  req.GetBizType(),
		// 只进日志与 payment_provider_calls，不参与幂等哈希——trace 每条请求都不同，
		// 把它算进哈希会让同一个 request_id 的每次重试都变成一次「换了请求体」的冲突。
		TraceID: audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		return nil, serviceError(err)
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

// serviceError 把业务错误翻成 gRPC 状态码。**发起支付与退款两条路共用它**（见 refund.go）：
// 两条路翻的是同一个 service 包的错误，分两份写只会让「同一个错误在两个入口回不同的码」。
//
// 判据是「调用方该做什么」，不是「错误有多严重」：
//
//	InvalidArgument         请求本身不合法，改了才能成
//	NotFound                目录里没有这个支付方式 code（对方传错了）
//	FailedPrecondition      这条路上当前做不了这件事（渠道没配齐、这笔钱还不该退）
//	                        ——重试多少次都一样，得先改配置或先等钱收妥
//	Aborted                 同一个幂等键的上一笔还在跑 / 另一条路已经把它推走了，退避重试
//	AlreadyExists           幂等键被换过请求体，重试也没用
//	Unavailable             我们得不出结论（渠道超时/结果不明），稍后可能成
//	Internal                我们自己的故障（渠道没注册、密钥读不到、数据库报错）
//
// **状态消息不回传原始错误文本**：里面的表名、SQL 片段、渠道配置不该跨服务边界跑。
// 排查要用的信息在服务端日志里，调用方拿到的是一句分得清该怎么办的话。
//
// 这一组里从前有两条**用中文**（渠道停用 / 历史只读）。它们随「渠道是数据」一起没了：
// 今天一条渠道要么在代码里、要么不存在，没有「运营把它停掉」这个状态，也就没有一句要让
// 用户看懂并照做的话。剩下的分支只有调用方（另一个服务）在读，英文够用。
//
// 唯一一个用户可能撞上的新形态是「这条渠道这次部署没配齐」（ErrChannelIncomplete）——
// 那是我们的部署问题，用户改不了，所以走 FailedPrecondition 而不是给一句中文。
func serviceError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// **这条调用自己没时间了**：可能是调用方给的预算用完了，也可能是本服务 gRPC 面的整调用
		// 预算用完了（见 platform/server 里那处 kgrpc.Timeout）。它不是一个业务结论，更不是
		// 我们的故障——渠道那边可能什么都没发生，也可能已经把这笔退款收下了。
		//
		// 落在 default 上会报 Internal，而调用方把它读成「支付服务没答上来」：order-service 的
		// mapRefundError 就是这样，于是「一笔可能已经退成功的钱」被说成一次服务故障，售后单停在
		// approved 却没人知道该重发。DeadlineExceeded 才是那句话的本义，也正对着调用方那条
		// 「我们这条调用的 deadline 先到了 → ErrRefundUncertain → 重发同一个售后单号」的路。
		return status.Error(codes.DeadlineExceeded, "the call ran out of time before a conclusion")
	case errors.Is(err, context.Canceled):
		// 调用方走了（连接断了，或者它自己的上层预算到了）。同样不是我们的故障，而且这条
		// 应答多半没人读——调用方已经不在等了。
		return status.Error(codes.Canceled, "the call was cancelled by the caller")
	case service.IsValidationError(err):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, service.ErrPaymentMethodNotFound), errors.Is(err, service.ErrPaymentNotFound):
		return status.Error(codes.NotFound, "payment method not found")
	case errors.Is(err, service.ErrAgreementNotFound):
		// 签约那条路的「查无此约」。与 ErrPaymentNotFound **都是 NotFound 但不是同一件事**：
		// 调用方（membership-service）要按它决定是「这张单没了」还是「这份授权没了」。
		return status.Error(codes.NotFound, "payment agreement not found")
	case errors.Is(err, service.ErrChannelIncomplete),
		errors.Is(err, service.ErrUnauthorizedAction):
		return status.Error(codes.FailedPrecondition, "payment method is not available")
	case errors.Is(err, service.ErrAgreementNotChargable), errors.Is(err, service.ErrAgreementContractMissing):
		// 代扣那条路的「这份协议今天不能扣」与「这份协议上没有渠道侧的协议号」。
		//
		// 前者是**调用方的时序问题**（协议还没生效，或已经解约）——membership-service 该做的是
		// 别在这时候发起扣款，而不是退避重试；重试多少遍，渠道那边都是一句合同不存在。
		// 后者是那行数据的毛病（active 却没有 contract_no），要人来看，同样不是重试能解的。
		return status.Error(codes.FailedPrecondition, "the agreement is not chargeable")
	case errors.Is(err, service.ErrAgreementUnsupported), errors.Is(err, service.ErrAgreementPlanMissing):
		// 两个都是「这条路上当前做不了这件事」，重试多少遍都一样：前者是这条支付方式压根不是
		// 代扣那一路（调用方该换方式），后者是这份协议上没有渠道侧的签约模板 id（要人来查
		// 那行数据是怎么写进去的）。分开与 ErrChannelIncomplete 一档，是因为它们都不该让调用方
		// 去改请求——改了也不会成。
		return status.Error(codes.FailedPrecondition, "agreements are not available for this payment method")
	case errors.Is(err, service.ErrPaymentNotRefundable):
		// 这笔钱还不该退（没收妥、或者已经失败关掉了）。它是**调用方的时序问题**，不是我们
		// 的故障：order-service 该做的是别在这时候发起退款，而不是退避重试。
		return status.Error(codes.FailedPrecondition, "payment is not in a refundable state")
	case errors.Is(err, service.ErrRefundExceedsRefundable):
		// 退的钱超过了这张单剩下的可退额。这是一种与「渠道拒绝」同级的业务结论——重试没用，
		// 要改的是金额。
		return status.Error(codes.FailedPrecondition, "refund amount exceeds the refundable amount")
	case errors.Is(err, service.ErrProviderOperationUnsupported):
		// 这条支付方式对应的协议族没有这个能力（今天不会出现：银联商务三条操作都实现了）。
		// 它是**部署/配置**问题，与「这条渠道这次没配齐」落在同一档。
		return status.Error(codes.FailedPrecondition, "provider does not support this operation")
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
	case errors.Is(err, service.ErrRefundNotAdvanceable):
		// 退款单已经被另一个入口推走了（退款查询 worker 快过调用方）。
		// 调用方重发同一个售后单号会拿回那张单，所以同样是可重试的。
		return status.Error(codes.Aborted, "the refund was already advanced by another path")
	default:
		return status.Error(codes.Internal, "payment service error")
	}
}
