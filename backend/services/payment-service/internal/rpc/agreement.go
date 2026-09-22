package rpc

import (
	"context"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/service"
	paymentv1 "github.com/panda-dev/panda-v2/contracts/proto/payment/v1"
)

// 本文件是协议那四个 RPC：CreateAgreement / QueryAgreement / TerminateAgreement /
// ChargeAgreement。
//
// # 四条的分工
//
// 前两条（发起签约、回渠道核一份协议）是**协议自己**的事，后两条（解约、扣款）是这一刀加的
// ——它们改的是协议上的状态与协议上某一期的账，不是同一张表，但都从这份协议出发。
//
// # 「谁有权解约」的答案
//
// TerminateAgreement 只认服务令牌（auth.RequireService），所以有权调它的只有服务，不是某个
// 后台页面：用户在小程序里点「关闭自动续费」走的是 membership-service，运营在后台取消订阅
// 走的也是它（见计划 §二.4——那边**必须先调这里**再改本地 flag，顺序反了就是「用户以为关了、
// 微信下个月照扣」）。后台直连本服务解约这条路今天不存在，真要做时该在 admin 那棵树上单开一个
// 方法，而不是把这个内部契约放开。

// ListAgreementCharges 读一份协议下的全部期次（后台「订阅详情」的续费明细）。
//
// # 分档
//
//	InvalidArgument  协议号为空。
//	NotFound         我们库里没有这份协议。**与「这份协议还没有任何期次」是两件事**：
//	                 后者回一个空列表。调用方（membership-service）手里那个协议号抄自订阅行，
//	                 对不上任何一份协议时说明那行数据坏了，而它对这个结论的处置与空列表
//	                 完全不同（见 service.ListAgreementCharges 的说明）。
//	Internal         我们自己的故障。
//
// 只认服务令牌：这是内部服务之间的读，后台读支付数据走的是它自己那棵树。
func (s *PaymentService) ListAgreementCharges(ctx context.Context, req *paymentv1.ListAgreementChargesRequest) (*paymentv1.ListAgreementChargesResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	charges, err := s.payments.ListAgreementCharges(ctx, req.GetAgreementNo())
	if err != nil {
		return nil, serviceError(err)
	}
	resp := &paymentv1.ListAgreementChargesResponse{
		Charges: make([]*paymentv1.AgreementCharge, 0, len(charges)),
	}
	for _, charge := range charges {
		resp.Charges = append(resp.Charges, &paymentv1.AgreementCharge{
			BizPeriod:             charge.BizPeriod,
			Amount:                charge.Amount,
			Status:                charge.Status,
			AttemptCount:          int32(charge.AttemptCount),
			NextRetryAt:           formatTime(charge.NextRetryAt),
			ProviderTransactionId: charge.ProviderTransactionID,
			FailureCode:           charge.FailureCode,
			FailureMessage:        charge.FailureMessage,
			ChargedAt:             formatTime(charge.ChargedAt),
			CreatedAt:             formatTime(&charge.CreatedAt),
		})
	}
	return resp, nil
}

// formatTime 把可空的时间格式化成 proto 约定的 RFC3339（UTC）字符串，nil 读成空串。
//
// 空串而不是零值时间：`0001-01-01T00:00:00Z` 编出来与一个真的时刻长得一样，而调用方要
// 按它显示「还没扣成」，界面上就会出现一个公元元年的日期。proto 里用字符串而不是
// google.protobuf.Timestamp 是本仓的既定约定（见 agreement.proto 的说明）。
func formatTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// CreateAgreement 是 membership-service 发起一次委托代扣签约时走的那条路。
//
// **签约不在这条路上收尾**：它返回的是给客户端的跳转参数，用户拿它在渠道那边确认，确认之后
// 渠道推通知（见 controller 的 agreement-notify）或由调用方回来查（QueryAgreement）。
// 所以这里返回的 status 恒为 pending，调用方不该把它当成「签好了」。
//
// 与 CreatePayment 一样，**渠道拒绝不是这个 RPC 的错误**（今天也没有这个类别：纯签约服务端
// 只算一个签名，失败只会是「密钥没配」或「模板 id 没给」，两者都该是 error）。真正的 error
// 只有两类：请求不合法，以及调用方给了一个不会签约的支付方式。
func (s *PaymentService) CreateAgreement(ctx context.Context, req *paymentv1.CreateAgreementRequest) (*paymentv1.CreateAgreementResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.payments.CreateAgreement(ctx, service.CreateAgreementRequest{
		UserID:          req.GetUserId(),
		PaymentMethod:   req.GetPaymentMethod(),
		PlanCode:        req.GetPlanCode(),
		ProviderPlanID:  req.GetProviderPlanId(),
		WalletOpenID:    req.GetWalletOpenId(),
		Subject:         req.GetSubject(),
		MaxChargeAmount: req.GetMaxChargeAmount(),
		RequestID:       req.GetRequestId(),
		// 只进日志与 payment_provider_calls，不参与幂等哈希——与 CreatePayment 同一条：
		// trace 每条请求都不同，算进去会让同一个 request_id 的每次重试都变成一次冲突。
		TraceID: audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		return nil, serviceError(err)
	}
	return &paymentv1.CreateAgreementResponse{
		AgreementNo: result.AgreementNo,
		AgreementId: result.AgreementID,
		Status:      result.Status,
		Action:      result.Action,
		PayParams:   result.PayParams,
	}, nil
}

// QueryAgreement 是「回渠道核一份协议」的那条路：后台的「同步」按钮与签约之后的「确认」
// 走的都是它。
//
// # 它是写，不是读
//
// 名字像查询，做的事是**纠错**：渠道说这份协议没了而本库还记着生效中时，纠正只能发生在这
// 一次调用里，返回的 status 是**纠正之后**的本库状态。所以它在 gRPC 面上不是幂等的读——
// 两个并发的同步会串行改同一行（判定在仓储的锁内）。
//
// 请求里**只有协议号**：签约模板 id 与渠道协议号都冻在 payment-service 自己的库里，让调用方
// 传它们等于给它一个传错的机会，而传错的后果是拿错模板去问、得到「查无此约」、然后把一份
// 有效的协议在本地判死（见 AgreementQuery.PlanID 的注释）。
//
// 结果不明时返回 Unavailable 而**不是**一个「没签」的结论：把一次超时读成「已解约」会让下
// 个月扣不到款、会员静默断掉。
func (s *PaymentService) QueryAgreement(ctx context.Context, req *paymentv1.QueryAgreementRequest) (*paymentv1.QueryAgreementResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.payments.QueryAgreement(ctx, service.QueryAgreementRequest{
		AgreementNo: req.GetAgreementNo(),
		RequestID:   req.GetRequestId(),
		TraceID:     audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		return nil, serviceError(err)
	}
	return &paymentv1.QueryAgreementResponse{
		AgreementNo:   result.AgreementNo,
		Status:        result.Status,
		ContractNo:    result.ContractNo,
		ProviderState: result.ProviderState,
		Changed:       result.Changed,
	}, nil
}

// ChargeAgreement 是 membership-service 在订阅到期时发起一期扣款走的那条路。
//
// # 它返回的是「这一期现在怎样」，不是「扣到钱了没有」
//
// 渠道同步回的 SUCCESS 只说明请求被收下了，所以 status 多半是 charging。**调用方不该拿这个
// 返回值去续会员**：续费的凭据是渠道推回来的那条通知（payment.agreement.charge_succeeded，
// 见 dto 的注释），不是这里。这条 RPC 的返回值只有两个用处——知道这一期"已经在路上了、
// 别再发"（charging），以及「渠道当场拒了」时的那句原因（failed）。
//
// # 它是幂等的，幂等键不是 request_id
//
// 同一份协议的同一个 biz_period 只可能扣一次：库上 UNIQUE (agreement_id, biz_period) 是那把
// 锁，而重试落在同一行上（attempt_count 加一）。调用方超时后重发同一个期次是安全的——包括
// 那次发出去的请求其实已经受理、通知还在路上的情况（那时重复调用会看到 charging 并原样返回）。
func (s *PaymentService) ChargeAgreement(ctx context.Context, req *paymentv1.ChargeAgreementRequest) (*paymentv1.ChargeAgreementResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.payments.ChargeAgreement(ctx, service.ChargeAgreementRequest{
		AgreementNo: req.GetAgreementNo(),
		BizPeriod:   req.GetBizPeriod(),
		Amount:      req.GetAmount(),
		Subject:     req.GetSubject(),
		// **不参与幂等判定**（幂等键是 (agreement_id, biz_period)），只进两条流水——排查时
		// 拿它与调用方的日志对时间线。
		RequestID: req.GetRequestId(),
		TraceID:   audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		return nil, serviceError(err)
	}
	return &paymentv1.ChargeAgreementResponse{
		AgreementNo:           result.AgreementNo,
		BizPeriod:             result.BizPeriod,
		Status:                result.Status,
		ProviderTransactionId: result.ProviderTransactionID,
		FailureCode:           result.FailureCode,
		FailureMessage:        result.FailureMessage,
	}, nil
}

// TerminateAgreement 是解约那条路：用户在小程序里点「关闭自动续费」、运营在后台取消订阅，
// 两条最终都走到这里。
//
// # 渠道拒绝不是 error
//
// 「合同已不存在」「已经解约过了」这类答复走 failure_code 回给调用方（proto 里那两个字段的
// 注释写着「它是业务结论不是 error」）。调用方拿到非空的 failure_code 时该做的是**什么都不改**
// ——那份协议在渠道那边已经是它要的样子了，或者渠道给了一个要人来看的理由；两种都不该被一次
// 自动重试糊过去。
//
// 只有「没能问出结论」（超时、报文读不懂）才返回 error（Unavailable），那时本地一个字段都
// 没改，调用方重试是安全的。
func (s *PaymentService) TerminateAgreement(ctx context.Context, req *paymentv1.TerminateAgreementRequest) (*paymentv1.TerminateAgreementResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	result, err := s.payments.TerminateAgreement(ctx, service.TerminateAgreementRequest{
		AgreementNo: req.GetAgreementNo(),
		Reason:      req.GetReason(),
		RequestID:   req.GetRequestId(),
		TraceID:     audit.TraceIDFromContext(ctx),
	})
	if err != nil {
		return nil, serviceError(err)
	}
	return &paymentv1.TerminateAgreementResponse{
		AgreementNo:    result.AgreementNo,
		Status:         result.Status,
		FailureCode:    result.FailureCode,
		FailureMessage: result.FailureMessage,
	}, nil
}
