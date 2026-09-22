package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 扣款那条路上「请求不合法」的几个错误。
var (
	// ErrBizPeriodRequired：没给期次。
	//
	// 它**不是**一个可选的备注：期次是这一期扣款的幂等键（库上 UNIQUE (agreement_id,
	// biz_period)），缺了它就没有任何东西挡得住同一期被扣两次。
	ErrBizPeriodRequired = errors.New("bizPeriod is required")
	// ErrChargeAmountNotPositive：扣款金额不是正数。库上还有一条 CHECK 兜着，这里挡是为了让
	// 调用方拿到一句能读的话而不是一次 23514。
	ErrChargeAmountNotPositive = errors.New("amount must be positive")
)

// 扣款那条路上「当前做不了这件事」的错误。
var (
	// ErrAgreementNotChargable：这份协议不能扣款（还没生效，或者已经解约）。
	//
	// 与「查无此约」（repository.ErrAgreementNotFound）分开：那一条说的是「你给错了号」，
	// 这一条说的是「这份协议在，但它今天不该被扣」。两者的处置完全不同——后者要业务方去看
	// 那份协议为什么没生效，而不是去纠正号。
	ErrAgreementNotChargable = errors.New("the agreement is not chargeable")
	// ErrAgreementContractMissing：协议上没记渠道侧的协议号，解不了约。
	//
	// 解约在渠道那边认的是渠道签发的那份凭证（微信的 contract_id），我们自己的协议号它不认。
	// 拿不到它时**绝不能**拿 agreement_no 去凑：那要么被渠道拒掉（白跑一趟），要么——如果
	// 渠道恰好也接受 contract_code——把我们这边一个猜的值写进渠道的账上。
	ErrAgreementContractMissing = errors.New("the agreement does not carry a provider contract id")
)

// ChargeAgreementRequest 是一次代扣的输入。
type ChargeAgreementRequest struct {
	// AgreementNo 是我们自己的协议号（payment_agreements.agreement_no）。
	AgreementNo string
	// BizPeriod 是期次，**由业务方给**（membership-service 从订阅的 next_charge_at 派生）。
	//
	// 它必须在同一期的两次重试之间**算出来一样**：这是库级幂等键起作用的前提，也是
	// 「重试复用同一行、不新开一行」的前提（见 model.PaymentAgreementCharge.BizPeriod）。
	BizPeriod string
	// Amount 是扣款金额，单位分。本服务不对着会员套餐复核（与发起支付同一条规矩：总额由
	// 业务方权威给出）。
	Amount  int64
	Subject string
	// RequestID 只进渠道调用流水与状态流水（排查时拿它与调用方的日志对时间线），**不参与
	// 幂等判定**——这条路的幂等键是 (agreement_id, biz_period) 与 out_trade_no，不是这个。
	RequestID string
	TraceID   string
}

// ChargeAgreementResult 是一次代扣的结果。字段与 ChargeAgreementResponse 一一对应。
type ChargeAgreementResult struct {
	AgreementNo string
	BizPeriod   string
	Status      string
	// ChargeNo 是这一期在渠道那边的商户单号（payment_agreement_charges.out_trade_no）。
	// 它不在 proto 里（调用方不需要拿它做任何判断），但排查时它是**唯一**能拿去问微信的单号，
	// 所以留在结果里。
	ChargeNo string
	// ProviderTransactionID 是渠道侧的流水号。**受理即返回，而受理不等于扣到钱**。
	ProviderTransactionID string
	FailureCode           string
	FailureMessage        string
	// NextRetryAt 是这一期下一次可以再试的时间；nil 表示不再重试（已试满）。
	NextRetryAt *time.Time
}

// ChargeAgreement 对一份生效中的协议发起一期扣款。
//
// # 幂等靠那一行，不靠调用方
//
// 同一期重复调过来（worker 重跑、多副本同时扫到、调用方超时后重试）**不会**扣两次第二次：
// CreateOrFindCharge 拿到的是同一行，而这一行已经带着上一次的 out_trade_no 与状态。所有
// 「要不要碰渠道」的判断都在那条行上做（见下面那张状态分流表），调用方不需要记住任何东西
// ——这正是老系统缺的那一块，它靠一个进程内的 map 锁，多副本下等于没有（计划 §六.3）。
//
// # 受理不是结论
//
// 渠道回 result_code=SUCCESS 只说明它**收下了**这个请求。真正到没到账由渠道推到 notify_url 的
// 那条通知说，所以这里最多把这一期推到 charging（见 model.ChargeStatusCharging）；succeeded
// 只可能由 SettleChargeNotification 写。老系统把 634 行那个 transaction_id 当成了扣款成功，
// 于是在余额不足的风控单上照样给用户续了会员。
//
// # 结果不明时按「可能已经扣了」处理
//
// 请求发出去没等到应答（或连发出去没有都不确定）时，钱**可能已经动了**。这时的正确处置是把
// 这一期留在 charging 上等通知，而**不是**记成失败：记失败会让那条随后到达的通知撞上
// applyChargeTarget 的「只有 pending/charging 能推进」而被丢掉，于是钱扣了、账上却什么都没有。
// 唯一的例外是适配器明确标了「一个字节都没发出去」（NOT_SENT）——那一次确实没发生，可以重试。
func (s *PaymentService) ChargeAgreement(ctx context.Context, in ChargeAgreementRequest) (*ChargeAgreementResult, error) {
	if err := validateChargeAgreement(in); err != nil {
		return nil, err
	}

	agreement, err := s.repository.FindAgreementByNo(ctx, in.AgreementNo)
	if err != nil {
		return nil, err
	}
	if agreement.Status != model.AgreementStatusActive {
		// 一份还没生效（用户在微信那边还没点确认）或已经解约的协议，扣款请求发出去**必然**
		// 被渠道拒（微信回 CONTRACT_NOT_EXIST 一类）。让它在入口断，比让它变成一次渠道调用
		// 与一条没意义的失败流水好——后者会让「连续失败」的计数凭空涨一格，而用户什么都没干。
		return nil, fmt.Errorf("%w: agreement %s is %s", ErrAgreementNotChargable,
			agreement.AgreementNo, agreement.Status)
	}
	route, keeper, secrets, err := s.resolveAgreementRoute(agreement.PaymentMethod)
	if err != nil {
		return nil, err
	}

	charge, _, err := s.repository.CreateOrFindCharge(ctx, repository.CreateOrFindChargeParams{
		AgreementID: agreement.ID,
		AgreementNo: agreement.AgreementNo,
		BizPeriod:   strings.TrimSpace(in.BizPeriod),
		Amount:      in.Amount,
		// 每次调用都现算一个号，而**只有新建那一行会用它**：已有那一行保留自己那个值。
		// 现算而不是先去读一次，是为了不在建行那条路上多一次往返——算错（真的新建了却拿到
		// 旧号）的可能是零，因为仓储只在 INSERT 赢了的时候才用这个值。
		OutTradeNo: chargeNo(s.now()),
		RequestID:  in.RequestID,
	})
	if err != nil {
		return nil, err
	}

	// 状态分流：**下面每一条都原样回现状、不碰渠道**。
	//
	// 这张表是这一刀的核心，逐条都有具体的事故对着：
	//
	//	succeeded  这一期已经扣到了。再发一次就是重复扣款。
	//	charging   上一次的受理还在路上，等通知。再发一次会让微信开出第二笔单（同号幂等只在
	//	           渠道那边成立，而我们这边会因为收到两条通知而算不清这一期到底怎样）。
	//	cancelled  协议在扣款途中被解约了，这一期作废（终态）。
	//	skipped    业务方说过这一期不用扣（如签约生效前已经退订）。
	//	failed     分两种：**退避中**（next_retry_at 还没到）原样回；**试满了**
	//	           （attempt_count >= ChargeMaxAttempts）原样回。只有既没到期又没试满才重试。
	//	pending    刚建出来、还没问过渠道——这是唯一一条「第一次」的路。
	switch charge.Status {
	case model.ChargeStatusSucceeded, model.ChargeStatusCharging,
		model.ChargeStatusCancelled, model.ChargeStatusSkipped:
		return chargeAgreementResult(charge), nil
	case model.ChargeStatusFailed:
		if charge.AttemptCount >= model.ChargeMaxAttempts || !chargeRetryDue(charge, s.now()) {
			return chargeAgreementResult(charge), nil
		}
	case model.ChargeStatusPending:
		// 落到这里就是第一次。
	default:
		// 词表外的状态是**我们自己的编码错误**（changing 那条路只写得到上面六个值）。报错而不是
		// 当成「不用扣」：后者的后果是这一期安静地永远不被扣。
		return nil, fmt.Errorf("unknown charge status %q on %s", charge.Status, charge.OutTradeNo)
	}

	return s.chargeAgreement(ctx, charge, agreement, route, keeper, secrets, in)
}

// chargeAgreement 是真正出网那一段：发起扣款、落流水、把结果写回那一行。
//
// 抽出来是为了让上面那张分流表读起来就是一张表——出网调用与四次落库挤在同一个函数里时，
// 「哪些状态不碰渠道」这件事会淹在一堆错误处理里。
func (s *PaymentService) chargeAgreement(ctx context.Context, charge *model.PaymentAgreementCharge,
	agreement *model.PaymentAgreement, route paymentRoute, keeper provider.AgreementKeeper,
	secrets provider.Credentials, in ChargeAgreementRequest) (*ChargeAgreementResult, error) {

	started := time.Now()
	charged, err := keeper.Charge(ctx, provider.AgreementChargeRequest{
		AgreementNo: agreement.AgreementNo,
		// 扣款认的是**渠道侧的协议号**，不是我们自己的（见 provider.AgreementChargeRequest）。
		ProviderContractID: agreement.ContractNo,
		// **必须是这一行上存着的那个号**，不是现算一个（见 model.PaymentAgreementCharge.OutTradeNo）。
		OutTradeNo: charge.OutTradeNo,
		Subject:    chargeSubject(in.Subject, agreement.Subject),
		Amount:     charge.Amount,
		NotifyURL:  s.chargeNotifyURL(route.Channel.Code),
		Method:     route.providerMethod(),
		Secrets:    secrets,
	})
	// 出网调用先留痕，再管状态（与发起支付、查约两条路同一个次序）：即使下面那一步失败，
	// 这一次询问也已经真的发生过了。
	s.writeProviderCall(ctx, providerCallRecord{
		TraceID:     in.TraceID,
		Provider:    route.Provider(),
		Operation:   model.CallOperationAgreementCharge,
		AgreementNo: agreement.AgreementNo,
		RequestID:   in.RequestID,
		Summary: map[string]any{
			"agreementNo": agreement.AgreementNo,
			"bizPeriod":   charge.BizPeriod,
			"outTradeNo":  charge.OutTradeNo,
			"amount":      charge.Amount,
			"attempt":     charge.AttemptCount + 1,
		},
		ResponseSummary: charged.ResponseSummary,
		HTTPStatus:      charged.HTTPStatus,
		ProviderCode:    charged.FailureCode,
		ProviderMessage: charged.FailureMessage,
		Result:          charged.Result,
		Attempts:        charged.Attempts,
		DurationMS:      int(time.Since(started).Milliseconds()),
		Err:             err,
	})

	attempt := repository.ChargeAttemptParams{
		ChargeID:              charge.ID,
		ProviderTransactionID: charged.ProviderTransactionID,
		RequestID:             in.RequestID,
		TraceID:               in.TraceID,
	}
	switch {
	case err == nil && charged.Result == provider.ResultSuccess:
		// 受理了。**只推到 charging**，钱到没到等通知（见函数注释）。
		attempt.Status = model.ChargeStatusCharging

	case chargeAttemptFailed(charged.Result, charged.FailureCode):
		// 渠道明确拒了（余额不足、风控、参数错），或者请求一个字节都没发出去。两种都确定
		// 「钱没有动」，所以可以记失败并排下一次。
		attempt.Status = model.ChargeStatusFailed
		attempt.FailureCode = charged.FailureCode
		attempt.FailureMessage = failureReason(charged.Result, charged.FailureCode, err)
		// 退避按**这一次之后**的尝试次数算，所以传的是 +1（MarkChargeAttempt 会把
		// attempt_count 加上一，两者必须对齐，否则退避会晚一次才到期）。
		attempt.NextRetryAt = model.ChargeNextRetryAt(s.now(), charge.AttemptCount+1)

	default:
		// 发出去没等到应答，或者报文读不懂。钱**可能已经动了**——留在 charging 上等通知。
		// 这一期若因此永远等不到通知，它停在 charging 上等人去微信商户平台核（查单兜底是
		// 下一刀，见计划 §五）。
		attempt.Status = model.ChargeStatusCharging
	}

	updated, err := s.repository.MarkChargeAttempt(ctx, attempt)
	if err != nil {
		return nil, err
	}
	return chargeAgreementResult(updated), nil
}

// TerminateAgreementRequest 是一次解约的输入。
type TerminateAgreementRequest struct {
	AgreementNo string
	Reason      string
	RequestID   string
	TraceID     string
}

// TerminateAgreementResult 是一次解约的结果。字段与 TerminateAgreementResponse 一一对应。
type TerminateAgreementResult struct {
	AgreementNo string
	Status      string
	// FailureCode / FailureMessage 只在**渠道明确拒绝**时非空，它们是业务结论不是 error
	// （见 proto 里 TerminateAgreementResponse 的注释）：重试没用，要处理的是渠道给的理由。
	FailureCode    string
	FailureMessage string
}

// TerminateAgreement 解一份协议：先撤渠道那边的授权，再把本地状态收掉。
//
// # 顺序不能反
//
// 渠道调用成功之后才动本地。反过来的话，一次失败（网络抖一下）会留下「本地已解约、微信那边
// 还挂着」——而微信下个月照样扣钱，用户在小程序里看到的是「自动续费：已关闭」。这正是这一刀
// 要消灭的那个状态（见计划 §二.4）。
//
// # 渠道拒绝不返回 error
//
// 「这份合同已经不存在了」「已经解约过了」这类答复是**结论**：它们说明渠道那边已经是我们要的
// 样子了，重试不会有别的结果。所以它们走 FailureCode 回给调用方，由调用方决定怎么办
// （membership-service 的做法是**什么都不改**、把话原样报给用户——见计划 §二.4）。
// 只有「我们没能问出结论」（超时、报文读不懂）才返回 error。
func (s *PaymentService) TerminateAgreement(ctx context.Context, in TerminateAgreementRequest) (*TerminateAgreementResult, error) {
	if strings.TrimSpace(in.AgreementNo) == "" {
		return nil, ErrAgreementNoRequired
	}

	agreement, err := s.repository.FindAgreementByNo(ctx, in.AgreementNo)
	if err != nil {
		return nil, err
	}
	if agreement.Status == model.AgreementStatusTerminated {
		// 已经是终态：本地不用改，渠道那边上一刀也已经撤过了。**不再调渠道**——重复解约会被
		// 渠道拒（合同不存在），而那会变成一个回给用户的失败，描述的是我们已经知道的现状。
		return &TerminateAgreementResult{
			AgreementNo: agreement.AgreementNo,
			Status:      agreement.Status,
		}, nil
	}

	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = "用户关闭自动续费"
	}

	if strings.TrimSpace(agreement.ContractNo) == "" {
		// 渠道那边没有可撤的凭证：这份协议还停在 pending（用户从来没在微信里点过确认）。
		// 本地收掉即可——去调解约只会拿到一句「合同不存在」，而那句话在这里不是错误，是噪音。
		return s.settleTerminated(ctx, agreement, reason, in, "", "")
	}

	route, keeper, secrets, err := s.resolveAgreementRoute(agreement.PaymentMethod)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	terminated, err := keeper.TerminateAgreement(ctx, provider.AgreementTerminate{
		AgreementNo:        agreement.AgreementNo,
		ProviderContractID: agreement.ContractNo,
		Reason:             reason,
		Method:             route.providerMethod(),
		Secrets:            secrets,
	})
	s.writeProviderCall(ctx, providerCallRecord{
		TraceID:     in.TraceID,
		Provider:    route.Provider(),
		Operation:   model.CallOperationAgreementTerminate,
		AgreementNo: agreement.AgreementNo,
		RequestID:   in.RequestID,
		Summary: map[string]any{
			"agreementNo": agreement.AgreementNo,
			"reason":      reason,
		},
		ResponseSummary: terminated.ResponseSummary,
		HTTPStatus:      terminated.HTTPStatus,
		ProviderCode:    terminated.FailureCode,
		ProviderMessage: terminated.FailureMessage,
		Result:          terminated.Result,
		Attempts:        terminated.Attempts,
		DurationMS:      int(time.Since(started).Milliseconds()),
		Err:             err,
	})

	if err == nil && terminated.Result == provider.ResultSuccess {
		// 本地这一句 reason 与发给渠道的那句是**同一个**（见 provider.AgreementTerminate.Reason
		// 「解约这件事在渠道账单上留下的唯一一句人话」）：两处各写一句的话，用户看到的与渠道
		// 账单上的迟早对不上。
		return s.settleTerminated(ctx, agreement, reason, in, "", "")
	}

	// 渠道明确拒绝：业务结论，本地不改。
	if chargeAttemptFailed(terminated.Result, terminated.FailureCode) {
		return &TerminateAgreementResult{
			AgreementNo:    agreement.AgreementNo,
			Status:         agreement.Status,
			FailureCode:    terminated.FailureCode,
			FailureMessage: failureReason(terminated.Result, terminated.FailureCode, err),
		}, nil
	}

	// 结果不明：**什么都不改**并报错。把一次解约超时当成「已解约」是本服务里最贵的一种误判
	// ——用户以为关了，微信那边还在扣（与查约那条路同一条判据）。
	return nil, fmt.Errorf("%w: terminating agreement %s: %s",
		ErrProviderResultUncertain, agreement.AgreementNo,
		failureReason(terminated.Result, terminated.FailureCode, err))
}

// settleTerminated 把本地协议收成 terminated，并复用查约那条路的判定（SettleAgreement）。
//
// 复用而不是另写一遍：解约是终态、终态不复活、重复解约不重写——这套判据只有一份，两处各写
// 一遍就一定会有一处漏掉（签约通知那条路已经复用同一份了，见 applyAgreementTarget 的注释）。
func (s *PaymentService) settleTerminated(ctx context.Context, agreement *model.PaymentAgreement,
	reason string, in TerminateAgreementRequest, failureCode, failureMessage string) (*TerminateAgreementResult, error) {

	updated, _, err := s.repository.SettleAgreement(ctx, repository.SettleAgreementParams{
		AgreementNo: agreement.AgreementNo,
		Target:      model.AgreementStatusTerminated,
		Reason:      reason,
		RequestID:   in.RequestID,
	})
	if err != nil {
		return nil, err
	}
	return &TerminateAgreementResult{
		AgreementNo:    updated.AgreementNo,
		Status:         updated.Status,
		FailureCode:    failureCode,
		FailureMessage: failureMessage,
	}, nil
}

// chargeAttemptFailed 判断一次扣款尝试是不是**确定没动钱**的失败。
//
// 两条：渠道明确回绝（ResultFailed——适配器只在报文被收下且被拒时给这个值），以及请求一个
// 字节都没发出去（NOT_SENT）。**超时与「结果不明」不在此列**：钱可能已经动了，那种情况必须
// 留在 charging 上等通知（见 ChargeAgreement 的注释）。
func chargeAttemptFailed(result provider.Result, failureCode string) bool {
	return result == provider.ResultFailed || (result == provider.ResultUnknown && failureCode == "NOT_SENT")
}

// chargeRetryDue 判断一次失败的尝试是不是已经过了退避期。
//
// next_retry_at 为空有两种来源，都在这里判对：**从来没有排过**（列是 NULL）与**排不上**
// （尝试数已经试满，见 model.ChargeNextRetryAt 返回 nil）。后者由调用方先判掉（试满的判据
// 在前面），所以落到这里的 NULL 只可能是前者——按「该试了」处理是对的。
func chargeRetryDue(charge *model.PaymentAgreementCharge, now time.Time) bool {
	return charge.NextRetryAt == nil || !charge.NextRetryAt.After(now)
}

// chargeSubject 是账单上那句话（微信的 body）。
//
// 调用方给了就用调用方的，没给就退回签约时那句（如「会员自动续费」）——**不能是空串**：
// 微信对 body 是必填，空着会被拒，而那句拒的话（「body 为空」）与「这一期扣不了」长得一样。
func chargeSubject(requested, fallback string) string {
	if subject := strings.TrimSpace(requested); subject != "" {
		return subject
	}
	if subject := strings.TrimSpace(fallback); subject != "" {
		return subject
	}
	return "会员自动续费"
}

// chargeAgreementResult 把那一行翻成给调用方的结果。分流表里六条「原样回」的路都用它，
// 所以「回什么」这件事只有一份。
func chargeAgreementResult(charge *model.PaymentAgreementCharge) *ChargeAgreementResult {
	return &ChargeAgreementResult{
		AgreementNo:           charge.AgreementNo,
		BizPeriod:             charge.BizPeriod,
		Status:                charge.Status,
		ChargeNo:              charge.OutTradeNo,
		ProviderTransactionID: charge.ProviderTransactionID,
		FailureCode:           charge.FailureCode,
		FailureMessage:        charge.FailureMessage,
		NextRetryAt:           charge.NextRetryAt,
	}
}

// validateChargeAgreement 是发起代扣的第一道。
func validateChargeAgreement(in ChargeAgreementRequest) error {
	switch {
	case strings.TrimSpace(in.AgreementNo) == "":
		return ErrAgreementNoRequired
	case strings.TrimSpace(in.BizPeriod) == "":
		return ErrBizPeriodRequired
	case in.Amount <= 0:
		return ErrAmountNotPositive
	}
	return nil
}

// chargeNotifyURL 拼出这个渠道的**扣款结果**通知地址。
//
// 与协议通知（agreement-notify）分开两条 URL，因为这个 project 里最贵的一次事故就是这两条
// 撞在一起：老系统的续费扣款与签约回调共用一条路由，回调按 out_trade_no 去查订阅表，而那个
// 号从来没写进那张表 —— 永远查不到 → 回 FAIL → 微信永久重推（见迁移 013 的文件头）。
//
// 三条 URL 各一个方法而不是加参数，理由与 agreementNotifyURL 同：controller 的路径常量是
// 硬编码的第二份（它已经 import service，反过来引就是环）。
func (s *PaymentService) chargeNotifyURL(channelCode string) string {
	if s.notifyBaseURL == "" {
		return ""
	}
	return s.notifyBaseURL + "/v1/payments/agreement-charge-notify/" + channelCode
}

// chargeNo 生成一期扣款的商户单号（payment_agreement_charges.out_trade_no）。
//
// 形状照 paymentNo（PAY + 时间戳 + 三位毫秒 + 六位随机），把前缀换成 CHG：26 个字符，而微信
// 对 out_trade_no 的上限是 32。**不带协议号与期次**——微信只要求全局唯一，而把业务语义编进
// 单号会让「换个期次算法」变成一次历史数据的迁移（见迁移 013 的注释）。
func chargeNo(now time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		// 与 paymentNo / agreementNo 同一条理由：crypto/rand 在 darwin/linux 上不会失败，真失败了
		// 也绝不退回固定值——那会让同一毫秒内算出来的单号完全由时间戳决定，撞号从概率问题变成
		// 必然问题，而这里撞号的后果是**两期扣款共用一个号**（库上那条唯一索引会把它拦下来，
		// 但那时已经有一次扣款发起不出去了）。
		panic(fmt.Sprintf("payment-service: read random for charge number: %v", err))
	}
	tail := uint32(random[0])<<16 | uint32(random[1])<<8 | uint32(random[2])
	return fmt.Sprintf("CHG%s%03d%06d", now.Format("20060102150405"), now.UnixNano()%1000, tail%1000000)
}
