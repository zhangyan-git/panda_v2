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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 支付服务在协议这条路上的四种答复。**签约、查约、扣款、解约四条路共用这一组**——它们问的
// 是同一个域，四种答复的语义在四条路上也一致。分成四类而不是一个 error，理由与 order-service
// 的 mapPaymentError 逐字相同：**调用方该做的动作不同**。
//
//	ErrAgreementRejected      改请求或换部署，重发一模一样的一次没用
//	ErrAgreementNotFound      渠道和我们都认为这份协议没了 —— 是一次结论
//	ErrAgreementConflict      同一个幂等号的上一笔还在跑，换一把钥匙或者等一会儿
//	ErrPaymentUnavailable     没问到，等一会儿再试
//
// 前三个是「有结论」，最后一个是「没结论」。把没结论的当成结论是这条链路上最贵的一种错：
// 一次超时被读成「没签成」会让用户以为要重新点一遍，而他手里那份协议其实已经建好了。
var (
	// ErrAgreementRejected：支付服务不接受这次请求（请求不合法；这条支付方式压根不是代扣那
	// 一路；这份协议上没有渠道模板 id；或者**这份协议今天不能扣**——还没生效、已经解约、active
	// 却没有渠道协议号）。**它多半是我们自己的配置或时序问题**（支付方式没配齐、套餐的签约模板
	// id 没填、在协议生效之前就发起了扣款），不是用户填错了什么。
	//
	// 扣款那条路上它是 FailedPrecondition 而不是可重试的故障：重试多少遍，渠道那边都是一句
	// 合同不存在。
	ErrAgreementRejected = errors.New("payment service rejected the agreement request")
	// ErrAgreementNotFound：这份协议在支付服务那边不存在。后台「同步」时它是一次结论——
	// 本地记着的那个协议号在库里查无此约，说明这一行是本服务自己写坏的。
	ErrAgreementNotFound = errors.New("payment agreement not found")
	// ErrAgreementConflict：同一个幂等号的上一笔还在跑，或者那把钥匙被换过请求体。
	ErrAgreementConflict = errors.New("agreement request conflicts with an earlier attempt")
	// ErrPaymentUnavailable：支付服务没答上来（连不上、内部错误、超时、应答是空的）。
	//
	// **签约那条路上的超时尤其要当心**：支付服务可能已经算好签名、甚至已经落了一行协议，而
	// 我们这边只看到一条超时。所以它不承诺「什么都没发生」——调用方该做的是让用户重试同一次
	// 点击（requestId 相同，支付侧按幂等键回放同一份协议），而不是换一个说法报错。
	ErrPaymentUnavailable = errors.New("payment service is unavailable")
)

// 支付方式 code，**跨服务值引用**：它是 payment-service 目录里的一个常量
// （catalog.CodeWechatPapay），不是本服务能自己解释的东西。
//
// 为什么由本服务写死而不是问支付服务要：签约这件事在会员域只有一条路——微信委托代扣，而
// 「哪几个 code 能签约」是支付侧的目录事实。写死一个常量意味着目录改了名这里要跟着改；换来
// 的是签约不需要先做一次目录查询，而那次查询失败的时候我们连「该用哪个方式」都不知道。
//
// 它同时是**这个功能的前提**：V2 的签约走微信直连，换成任何别的通道（银联代扣之类）都要同时
// 改这里和 payment-service 的目录。
const paymentMethodWechatPapay = "wechat_papay"

// AgreementClient 是协议这条路上本服务对支付服务的全部四个动作：发起签约、回渠道核一份协议、
// 发起一期扣款、解一份协议。
//
// 带的是**服务令牌**（platform/auth.WithServiceToken）：这一次调用代表会员域去告诉支付域一个
// 事实（「这个人要签这份套餐」），不代表某个用户。用户的身份在这之前已经用过了——「签的是谁」
// 由令牌解出来的 userID 决定，openid 来自身份域（见 WalletIdentityReader），两者都不是客户端
// 在请求体里能声称的。
type AgreementClient struct {
	payments paymentv1.PaymentServiceClient
	token    string
	timeout  time.Duration
}

// NewAgreementClient 复用调用方那条连接：main 只拨一次，两个调用点共享同一个 *grpc.ClientConn。
func NewAgreementClient(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*AgreementClient, error) {
	if conn == nil {
		return nil, errors.New("payment service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("payment service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("payment service timeout must be positive")
	}
	return &AgreementClient{payments: paymentv1.NewPaymentServiceClient(conn), token: token, timeout: timeout}, nil
}

// Create 发起一次签约，拿回给客户端的跳转参数。
//
// **它不等到签约完成**：返回的 status 恒为 pending——用户拿跳转参数在微信里点完「同意」，
// 渠道推通知回来（或由调用方回来查）才算签成。把 pending 当签成是这条链路上最容易犯的错，
// 用户会被告知「已开通」而他根本没点。
func (c *AgreementClient) Create(ctx context.Context, in dto.CreateAgreementParams) (*dto.AgreementSigningResult, error) {
	if c == nil || c.payments == nil {
		return nil, errors.New("agreement client is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.payments.CreateAgreement(ctx, &paymentv1.CreateAgreementRequest{
		UserId:          in.UserID,
		PaymentMethod:   paymentMethodWechatPapay,
		PlanCode:        in.PlanCode,
		ProviderPlanId:  in.ProviderPlanID,
		WalletOpenId:    in.WalletOpenID,
		Subject:         in.Subject,
		MaxChargeAmount: in.MaxChargeAmount,
		RequestId:       in.RequestID,
		// trace 不在这里：它跟着 ctx 上的 metadata 走，支付侧从 ctx 取（见 audit.TraceIDFromContext）。
	})
	if err != nil {
		return nil, mapAgreementError(err)
	}
	if resp == nil || resp.GetAgreementId() == "" || resp.GetAgreementNo() == "" {
		// 应答里没有协议号或协议 id：这不可能是一次正常回答。两个号缺一个就写不进订阅
		// （agreement_id 与 contract_code 分别是它们的落点），当成「没问到」而不是把一条
		// 半截的订阅交给用户——他会跳去微信签一份我们库里认不出的协议。
		return nil, fmt.Errorf("%w: payment service returned an empty agreement", ErrPaymentUnavailable)
	}
	payParams := resp.GetPayParams()
	if payParams == nil {
		// gRPC 的 map 字段分不出「空」与「没有」，到这一侧一律是 nil，序列化出去是 JSON 的
		// null。而客户端拿到的必须是一个能直接读的对象（与 order-service 那条同一条理由）。
		payParams = map[string]string{}
	}
	return &dto.AgreementSigningResult{
		AgreementID: resp.GetAgreementId(),
		AgreementNo: resp.GetAgreementNo(),
		Status:      resp.GetStatus(),
		Action:      resp.GetAction(),
		PayParams:   payParams,
	}, nil
}

// Query 回渠道核一份协议，返回**纠正之后**的状态。
//
// 名字像查询，做的事是写：渠道说这份协议没了而支付库还记着生效中时，纠正就发生在那一次调用里
// （见 payment-service 的 rpc/agreement.go）。所以两个并发的「同步」不是两次读，是两次写。
//
// 结果不明时回 ErrPaymentUnavailable 而**不是一个「没签」的结论**：把一次超时读成「已解约」
// 会让下个月扣不到款、会员静默断掉。
func (c *AgreementClient) Query(ctx context.Context, agreementNo, requestID string) (*dto.AgreementState, error) {
	if c == nil || c.payments == nil {
		return nil, errors.New("agreement client is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.payments.QueryAgreement(ctx, &paymentv1.QueryAgreementRequest{
		AgreementNo: agreementNo,
		RequestId:   requestID,
	})
	if err != nil {
		return nil, mapAgreementError(err)
	}
	if resp == nil {
		return nil, fmt.Errorf("%w: payment service returned an empty agreement state", ErrPaymentUnavailable)
	}
	return &dto.AgreementState{
		AgreementNo:   resp.GetAgreementNo(),
		Status:        resp.GetStatus(),
		ContractNo:    resp.GetContractNo(),
		ProviderState: resp.GetProviderState(),
		Changed:       resp.GetChanged(),
	}, nil
}

// Charge 发起一期代扣。
//
// # 它返回的不是「扣到钱了没有」
//
// 渠道同步回的受理只说明请求被收下了（Status=charging），钱到没到在渠道推回来的通知里。所以
// 调用方拿到 charging 时该做的是**什么都不做**——续费的凭据是
// dto.EventAgreementChargeSucceeded 那条事件，不是这个响应。把受理当扣成会让会员在钱还没到
// 账时就被续上一个月。
//
// # 重发同一期是安全的
//
// 支付侧的幂等键是 (agreement_no, biz_period)，重发落在同一行上（那边记 attempt_count 加一）。
// 所以调用方超时之后**必须**原样重发同一个期次，而不是换一个——换一个就是在渠道那边开出第二
// 笔订单。
func (c *AgreementClient) Charge(ctx context.Context, in dto.ChargeAgreementParams) (*dto.AgreementChargeResult, error) {
	if c == nil || c.payments == nil {
		return nil, errors.New("agreement client is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.payments.ChargeAgreement(ctx, &paymentv1.ChargeAgreementRequest{
		AgreementNo: in.AgreementNo,
		BizPeriod:   in.BizPeriod,
		Amount:      in.Amount,
		Subject:     in.Subject,
		RequestId:   in.RequestID,
	})
	if err != nil {
		return nil, mapAgreementError(err)
	}
	if resp == nil {
		return nil, fmt.Errorf("%w: payment service returned an empty charge result", ErrPaymentUnavailable)
	}
	return &dto.AgreementChargeResult{
		AgreementNo:           resp.GetAgreementNo(),
		BizPeriod:             resp.GetBizPeriod(),
		Status:                resp.GetStatus(),
		ProviderTransactionID: resp.GetProviderTransactionId(),
		FailureCode:           resp.GetFailureCode(),
		FailureMessage:        resp.GetFailureMessage(),
	}, nil
}

// Terminate 解一份协议：**我们主动去撤**用户在渠道那边的授权。
//
// # 失败分两类，必须分开对待
//
//   - 渠道**明确拒绝**（「合同已不存在」这类）走结果的 FailureCode 回来，**不是 error**。那份
//     协议在渠道那边已经是它要的样子了，或者渠道给了一个要人来看的理由——两种都不该被一次自动
//     重试糊过去。
//   - 我们**没问出结论**（超时、报文读不懂）才是 error。那时支付侧一个字段都没改，重发是安全的。
//
// 这个区别是「关自动续费要先解约、成功后才改本地 flag」那条顺序的全部依据：把一次「没问到」
// 当成「解约成功」，用户会看到「自动续费：已关闭」而微信下个月照扣。
func (c *AgreementClient) Terminate(ctx context.Context, in dto.TerminateAgreementParams) (*dto.AgreementTermination, error) {
	if c == nil || c.payments == nil {
		return nil, errors.New("agreement client is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
	defer cancel()

	resp, err := c.payments.TerminateAgreement(ctx, &paymentv1.TerminateAgreementRequest{
		AgreementNo: in.AgreementNo,
		Reason:      in.Reason,
		RequestId:   in.RequestID,
	})
	if err != nil {
		return nil, mapAgreementError(err)
	}
	if resp == nil {
		return nil, fmt.Errorf("%w: payment service returned an empty termination result", ErrPaymentUnavailable)
	}
	return &dto.AgreementTermination{
		AgreementNo:    resp.GetAgreementNo(),
		Status:         resp.GetStatus(),
		FailureCode:    resp.GetFailureCode(),
		FailureMessage: resp.GetFailureMessage(),
	}, nil
}

// mapAgreementError 把 gRPC 状态码翻成上面那四个结论。
//
// 与 payment-service 的 serviceError 一一对应——那边怎么分的，这里就怎么收，中间不重新解释
// 一遍。**只有「有结论」的那三档带回状态消息**：那些句子是支付侧写好的固定说法，不是原始
// 错误文本；「没结论」那一档只留一个我们知道是什么的结论，它说的话对调用方没有可执行的信息。
func mapAgreementError(err error) error {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrAgreementRejected, status.Convert(err).Message())
	case codes.NotFound:
		return ErrAgreementNotFound
	case codes.Aborted, codes.AlreadyExists:
		return fmt.Errorf("%w: %s", ErrAgreementConflict, status.Convert(err).Message())
	case codes.Unimplemented:
		// 「这一档对面压根没有」——今天只可能是版本错配（调用方比支付服务新）。它长得像故障，
		// 就该长得像故障：归进 ErrAgreementRejected 会让人去翻套餐配置，而真正该做的是对齐部署。
		return fmt.Errorf("%w: the payment service does not implement this call", ErrPaymentUnavailable)
	default:
		// Unavailable / DeadlineExceeded / Internal 都在这里。DeadlineExceeded 是**我们自己**
		// 那条 5 秒的线：支付服务可能已经把协议建好了，而我们已经把这次调用放弃了。
		return ErrPaymentUnavailable
	}
}
