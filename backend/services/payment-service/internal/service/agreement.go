package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 签约那条路上「请求不合法」的几个错误。它们的处置与发起支付完全一样（gRPC InvalidArgument、
// 控制器 400），所以共用同一个 ValidationErrors 清单，见 service.go。
var (
	// ErrProviderPlanIDRequired：调用方没给渠道侧的签约模板 id。
	//
	// 它是**必填**而不是可空：签约模板 id 在渠道那边决定「这份协议每月扣多少」，缺了它签出来
	// 的东西在渠道那边不存在。老系统把这件事留给「会员等级配了没有」的一句话（
	// subscription_service.go 的「该会员等级未配置签约模板ID」），结果是错误发生在用户点了
	// 「开通连续包月」之后；这里让它在入口就断，调用方能自己发现套餐配漏了。
	ErrProviderPlanIDRequired = errors.New("providerPlanId is required")
	// ErrWalletOpenIDRequired：没给用户在渠道里的身份。
	//
	// 签约是用户与渠道之间的事，协议上写的就是「谁的账户可以被扣」。拿不到 openid 时**绝不能
	// 往下走**：合同里的 contract_display_account 会空着，用户签的是一份看不出授权给谁的东西。
	ErrWalletOpenIDRequired = errors.New("walletOpenId is required")
	// ErrMaxChargeAmountInvalid：扣款上限是负数。0 是合法的（未约定上限）。
	ErrMaxChargeAmountInvalid = errors.New("maxChargeAmount must not be negative")
	// ErrAgreementNoRequired：查约/解约没给协议号。
	ErrAgreementNoRequired = errors.New("agreementNo is required")
)

// 签约那条路上「当前做不了这件事」的错误（gRPC FailedPrecondition）。
var (
	// ErrAgreementUnsupported：这条支付方式对应的协议族不会签约。
	//
	// 与 ErrProviderOperationUnsupported 分开：那条说的是「这族协议没有这个能力」（换渠道才行），
	// 这条说的是「这个支付方式压根不是代扣那一路」（银联 H5、咖啡豆都走到这里）。调用方拿到
	// 它该做的是别拿这条方式签约，而不是去改配置。
	ErrAgreementUnsupported = errors.New("this payment method cannot sign agreements")
	// ErrAgreementPlanMissing：协议上没记渠道侧的签约模板 id，查不了约。
	//
	// 只可能出现在两种情况下：签约那次调用方没给（今天被校验挡在门外），或者库里的行是别处
	// 写进去的。查约**必须**带 plan_id（微信按 (plan_id, contract_code) 认这一份协议），
	// 所以这时宁可不问，也不拿一个猜的模板去问——问错模板会得到「查无此约」，而那条结论会把
	// 一份有效的协议在本地判死。
	ErrAgreementPlanMissing = errors.New("the agreement does not carry a provider plan id")
)

// CreateAgreementRequest 是一次发起签约的输入。
type CreateAgreementRequest struct {
	// UserID 是签约的用户，值引用。
	UserID string
	// PaymentMethod 是支付方式的 code（catalog 里的常量，如 `wechat_papay`）。
	PaymentMethod string
	// PlanCode 是业务侧的套餐标识（如会员套餐代码），本服务不解释它的含义。
	PlanCode string
	// ProviderPlanID 是渠道侧的签约模板 id。
	ProviderPlanID string
	// WalletOpenID 是用户在渠道里的身份（微信 openid），签约要素。
	WalletOpenID string
	Subject      string
	// MaxChargeAmount 是单次扣款上限，单位为分；0 表示未约定。
	MaxChargeAmount int64
	RequestID       string
	TraceID         string
}

// CreateAgreementResult 是发起签约的结果，也是**三个地方共用的同一个形状**：
// gRPC 响应、写进幂等表供回放的快照、以及（将来）控制器 JSON 响应。
//
// 与 CreateResult 同一条理由：回放的字节必须与首次响应一致，用两个结构各组装一遍是一个
// 必然漂移的写法。
type CreateAgreementResult struct {
	AgreementNo string `json:"agreementNo"`
	AgreementID string `json:"agreementId"`
	Status      string `json:"status"`
	// Action 是给客户端的（jump_miniapp）——客户端只按它决定怎么把用户送到签约页。
	Action    string            `json:"action"`
	PayParams map[string]string `json:"payParams"`
}

// CreateAgreement 发起一次委托代扣签约。
//
// # 一个协议号，两个身份
//
// agreementNo 由这里生成，**并且就是交给渠道的 contract_code**。老系统有两个号（自己那个
// ObjectID 与 contract_code），而扣款回传的 out_trade_no 与回调查询用的号又各说各话，于是
// 续费回调永远查不到订阅、渠道无限重推。这里
// 只留一个：渠道推回来的通知按它认这份协议，我们查约、解约、扣款也按它。
//
// # 三段里只有一段
//
// 渠道流程那边是「抢幂等键 / 出网调用 / 收口」三段事务，因为中间那次第三方往返要几秒，
// 不该让 PG 事务开着锁等。签约不是：**服务端只算一个签名，不发任何请求**（用户在客户端跳到
// 微信的签约页完成授权），所以这里是一次事务——签名在事务外算（纯计算、不发请求），
// 幂等记录、协议行、调用流水一起提交。
//
// 重放那条路上签名会被重算一次然后丢掉（幂等表里存的是第一次那份参数）——这是刻意的：
// 把幂等检查提到签名之前，就得先提交一个 processing 的幂等行，于是多出一个「抢了键、还没
// 建协议」的中间态，而那个中间态崩溃后留下的东西没有任何东西会去收拾它。
func (s *PaymentService) CreateAgreement(ctx context.Context, in CreateAgreementRequest) (*CreateAgreementResult, error) {
	if err := validateCreateAgreement(in); err != nil {
		return nil, err
	}

	route, keeper, secrets, err := s.resolveAgreementRoute(in.PaymentMethod)
	if err != nil {
		return nil, err
	}

	agreementNo := s.newAgreementNo(s.now())
	signResult, err := keeper.Sign(ctx, provider.AgreementSignRequest{
		AgreementNo: agreementNo,
		// 我们自己的协议号与交给渠道的 contract_code 是同一个值（见上面「一个协议号，两个
		// 身份」），两处传同一个变量而不是各传一次——写两次就会有一次被改成别的。
		ContractCode: agreementNo,
		PlanID:       in.ProviderPlanID,
		UserID:       in.UserID,
		WalletOpenID: in.WalletOpenID,
		NotifyURL:    s.agreementNotifyURL(route.Channel.Code),
		Method:       route.providerMethod(),
		Secrets:      secrets,
	})
	if err != nil {
		// **这一步不落渠道调用流水**：签名没算出来意味着我们对渠道什么也没做（连报文都没有），
		// 而没有协议行可挂的流水只会让人去找一份不存在的协议。这条错误原样回给调用方，它在
		// 日志里；真要说清楚的是配置（密钥没配）或调用方（套餐配漏了）。
		return nil, err
	}

	result := &CreateAgreementResult{
		AgreementNo: agreementNo,
		Status:      model.AgreementStatusPending,
		Action:      string(route.Method.Action),
		PayParams:   signResult.PayParams,
	}
	agreement, replay, hit, err := s.repository.CreateAgreement(ctx, repository.CreateAgreementParams{
		AgreementNo:     agreementNo,
		UserID:          in.UserID,
		Provider:        route.Provider(),
		Method:          route.Method.Code,
		Subject:         strings.TrimSpace(in.Subject),
		PlanCode:        strings.TrimSpace(in.PlanCode),
		MaxChargeAmount: in.MaxChargeAmount,
		Metadata: map[string]any{
			model.AgreementMetaProviderPlanID: in.ProviderPlanID,
		},
		RequestID:   in.RequestID,
		RequestHash: createAgreementRequestHash(in),
		TraceID:     in.TraceID,
		CallSummary: map[string]any{
			"agreementNo":     agreementNo,
			"userId":          in.UserID,
			"planCode":        in.PlanCode,
			"providerPlanId":  in.ProviderPlanID,
			"maxChargeAmount": in.MaxChargeAmount,
			"channel":         route.Channel.Code,
			"paymentMethod":   route.Method.Code,
			"action":          string(route.Method.Action),
			// 参数的值不进流水（含签名，是一次性凭据），只记它给了几个键——与发起支付那边
			// 记 payParamKeys 同一条规矩。
			"payParamKeys": sortedKeys(signResult.PayParams),
		},
		CallResponseSummary: signResult.ResponseSummary,
		Response:            result,
	})
	if err != nil {
		return nil, err
	}
	if hit {
		if len(replay) == 0 {
			// 幂等行在、快照是空的：只可能是一行被标成 succeeded 却没写 response 的脏数据。
			// 报错让人来查，好过在这里猜一组签约参数给客户端——那会让用户在微信那边签下一份
			// 我们这边没有记录的协议。
			return nil, fmt.Errorf("idempotent create agreement %s has no recorded response", in.RequestID)
		}
		var replayed CreateAgreementResult
		if err := json.Unmarshal(replay, &replayed); err != nil {
			return nil, fmt.Errorf("decode replayed create agreement response: %w", err)
		}
		return &replayed, nil
	}
	result.AgreementID = agreement.ID
	return result, nil
}

// QueryAgreementRequest 是回渠道核一份协议的输入。
type QueryAgreementRequest struct {
	AgreementNo string
	// RequestID 只进渠道调用流水（排查时拿它与调用方的日志对时间线），不参与任何判定。
	RequestID string
	TraceID   string
}

// QueryAgreementResult 是一次查约的结论。
type QueryAgreementResult struct {
	AgreementNo string
	// Status 是**纠正之后**的本库状态。调用方按它决定业务动作。
	Status string
	// ContractNo 是渠道侧的协议号（微信的 contract_id）。
	ContractNo string
	// ProviderState 是渠道给的原始状态词，不做翻译。排查时先看它：本库状态是我们的判断，
	// 这一个才是渠道的原话。
	ProviderState string
	// Changed 表示这次核查改了本库状态。
	Changed bool
}

// QueryAgreement 回渠道核一份协议，并把结论落回本库。
//
// 它是**写**不是读：渠道说这份协议没了而本库还记着生效中时，纠正只能发生在这里——后台那个
// 「同步」按钮与签约之后的「确认」走的都是它，两条路共用这一套判定，不各写一份。
//
// 结果不明时（超时、报文读不懂）**什么都不改**并报错：把一次查约超时读成「已解约」会把一份
// 有效的协议在本地判死，而判死的后果是用户下个月扣不到款、会员静默断掉。
func (s *PaymentService) QueryAgreement(ctx context.Context, in QueryAgreementRequest) (*QueryAgreementResult, error) {
	if strings.TrimSpace(in.AgreementNo) == "" {
		return nil, ErrAgreementNoRequired
	}
	agreement, err := s.repository.FindAgreementByNo(ctx, in.AgreementNo)
	if err != nil {
		return nil, err
	}
	route, keeper, secrets, err := s.resolveAgreementRoute(agreement.PaymentMethod)
	if err != nil {
		return nil, err
	}
	planID, err := agreementPlanID(agreement)
	if err != nil {
		return nil, err
	}

	queried, err := keeper.QueryAgreement(ctx, provider.AgreementQuery{
		AgreementNo:  agreement.AgreementNo,
		PlanID:       planID,
		ContractCode: agreement.AgreementNo,
		Method:       route.providerMethod(),
		Secrets:      secrets,
	})
	// 出网调用先留痕，再管状态（与发起支付那条路同一个次序）：流水记的是「我们对渠道问过
	// 什么」，即使下面那一步失败，这一次询问也已经真的发生过了。
	s.writeProviderCall(ctx, providerCallRecord{
		TraceID:     in.TraceID,
		Provider:    route.Provider(),
		Operation:   model.CallOperationQuery,
		AgreementNo: agreement.AgreementNo,
		RequestID:   in.RequestID,
		Summary: map[string]any{
			"agreementNo": agreement.AgreementNo,
			"planCode":    agreement.PlanCode,
			"localStatus": agreement.Status,
		},
		ResponseSummary: queried.ResponseSummary,
		HTTPStatus:      queried.HTTPStatus,
		ProviderCode:    queried.FailureCode,
		ProviderMessage: queried.FailureMessage,
		Result:          queried.Result,
		Attempts:        queried.Attempts,
		Err:             err,
	})

	if err != nil || queried.Result != provider.ResultSuccess {
		// 渠道明确拒绝（SIGN_ERROR、SYSTEMERROR、参数错）与超时归成一类：**都没有给出这份
		// 协议的确定状态**，而这条路上唯一安全的处置是「什么都不改、稍后再问」。它们分开记账
		// （Result 与 failure_code 都在流水里），但给调用方的答复是同一句。
		return nil, fmt.Errorf("%w: querying agreement %s: %s",
			ErrProviderResultUncertain, agreement.AgreementNo, failureReason(queried.Result, queried.FailureCode, err))
	}

	target, err := agreementTarget(queried.State)
	if err != nil {
		return nil, err
	}
	updated, changed, err := s.repository.SettleAgreement(ctx, repository.SettleAgreementParams{
		AgreementNo: agreement.AgreementNo,
		Target:      target,
		ContractNo:  queried.ProviderContractID,
		Reason:      agreementQueryReason(queried.State),
		RequestID:   in.RequestID,
	})
	if err != nil {
		return nil, err
	}
	return &QueryAgreementResult{
		AgreementNo:   updated.AgreementNo,
		Status:        updated.Status,
		ContractNo:    updated.ContractNo,
		ProviderState: string(queried.State),
		Changed:       changed,
	}, nil
}

// resolveAgreementRoute 是签约两条路共用的开头：把支付方式解析成「渠道 + 会签约的适配器 +
// 凭据」。
//
// 抽出来是因为三段判定都必须**同时**发生，而分开写必然有一处会漏：方式在不在目录里、这条
// 方式有没有渠道、它的适配器会不会签约、凭据读不读得到。漏掉最后一条的表现是「签名里少一个
// 字段」，那在渠道那边是一句 SIGN_ERROR，看不出是没配密钥。
func (s *PaymentService) resolveAgreementRoute(paymentMethod string) (paymentRoute, provider.AgreementKeeper, provider.Credentials, error) {
	route, err := s.resolveMethod(paymentMethod)
	if err != nil {
		return paymentRoute{}, nil, nil, err
	}
	if route.Channel == nil {
		// 账户出资（咖啡豆）是唯一没有渠道的 action，它当然不会签约。
		return paymentRoute{}, nil, nil, fmt.Errorf("%w: method %s has no channel", ErrAgreementUnsupported, route.Method.Code)
	}
	adapter, err := s.providers.Lookup(route.Channel.Provider)
	if err != nil {
		return paymentRoute{}, nil, nil, err
	}
	// 探可选接口（见 provider.AgreementKeeper）：探不到就是「这条渠道不支持协议」，而且**必须
	// 在写任何东西之前**断掉——先建协议再发现签不了，会留下一条永远停在 pending 的记录，
	// 用户那边却什么都没发生。
	keeper, ok := adapter.(provider.AgreementKeeper)
	if !ok {
		return paymentRoute{}, nil, nil, fmt.Errorf("%w: channel %s", ErrAgreementUnsupported, route.Channel.Code)
	}
	secrets := resolveSecrets(s.resolveSecret, route.Channel, adapter, route.providerMethod())
	return route, keeper, secrets, nil
}

// agreementPlanID 从协议的 metadata 里取出渠道侧的签约模板 id。
func agreementPlanID(agreement *model.PaymentAgreement) (string, error) {
	var metadata map[string]any
	if len(agreement.Metadata) > 0 {
		if err := json.Unmarshal(agreement.Metadata, &metadata); err != nil {
			return "", fmt.Errorf("%w: agreement %s has unreadable metadata: %v",
				ErrAgreementPlanMissing, agreement.AgreementNo, err)
		}
	}
	planID, _ := metadata[model.AgreementMetaProviderPlanID].(string)
	if strings.TrimSpace(planID) == "" {
		return "", fmt.Errorf("%w: agreement %s", ErrAgreementPlanMissing, agreement.AgreementNo)
	}
	return planID, nil
}

// agreementTarget 把渠道无关的协议状态翻成本库的协议状态。
//
// 映射是**总函数**：三个渠道状态都有落点，没有 default 里的「先按原样处理」。渠道那边多出
// 第四种状态时（协议这件事上迟早会有：暂停、过期），这条 switch 会立刻报错，而不是安静地
// 把一份状态不明的协议留在本地。
func agreementTarget(state provider.AgreementState) (string, error) {
	switch state {
	case provider.AgreementSigned:
		// 「用户已确认」与「这份协议能扣款了」今天是一件事（见 model.AgreementStatusActive），
		// 所以落 active 而不是 signed：未来那条扫待扣款的查询认的是 active。
		return model.AgreementStatusActive, nil
	case provider.AgreementTerminated:
		return model.AgreementStatusTerminated, nil
	case provider.AgreementPending:
		// 渠道说还在进行中：本地什么都不用改（本来就是 pending）。
		return model.AgreementStatusPending, nil
	default:
		return "", fmt.Errorf("%w: unknown provider agreement state %q", ErrProviderResultUncertain, state)
	}
}

// agreementQueryReason 是写进状态流水与解约原因的那句话。排查时先看它。
//
// 它**不区分「用户主动解约」与「查无此约」**：渠道对这两件事回同一组错误码，我们也不去猜
// （猜错的后果是给用户推一条「你的自动续费已关闭」，而他从没开过，见 provider.AgreementState）。
func agreementQueryReason(state provider.AgreementState) string {
	switch state {
	case provider.AgreementSigned:
		return "provider confirms the agreement is active"
	case provider.AgreementTerminated:
		return "provider reports the agreement is no longer valid"
	default:
		return "provider reports the agreement is still in progress"
	}
}

// failureReason 把一次失败的渠道调用拼成一句能进日志的话。
func failureReason(result provider.Result, failureCode string, err error) string {
	if err != nil {
		if failureCode != "" {
			return fmt.Sprintf("%s: %v", failureCode, err)
		}
		return err.Error()
	}
	return fmt.Sprintf("result=%s code=%s", result, failureCode)
}

// validateCreateAgreement 是发起签约的第一道。
func validateCreateAgreement(in CreateAgreementRequest) error {
	switch {
	case strings.TrimSpace(in.UserID) == "":
		return ErrUserIDRequired
	case uuid.Validate(strings.TrimSpace(in.UserID)) != nil:
		// 用户 id 是 uuid（落到 payment_agreements.user_id 那个 UUID 列上）。不在这里挡，
		// 它会以一次「invalid input syntax for type uuid」的数据库错误冒出去。
		return ErrUserIDInvalid
	case strings.TrimSpace(in.PaymentMethod) == "":
		return ErrMethodRequired
	case strings.TrimSpace(in.RequestID) == "":
		return ErrRequestIDRequired
	case strings.TrimSpace(in.ProviderPlanID) == "":
		return ErrProviderPlanIDRequired
	case strings.TrimSpace(in.WalletOpenID) == "":
		return ErrWalletOpenIDRequired
	case in.MaxChargeAmount < 0:
		return ErrMaxChargeAmountInvalid
	}
	return nil
}

// agreementNotifyURL 拼出这个渠道的**协议**通知地址。
//
// 与 notifyURL（支付回调）分两个方法而不是加一个参数：两条路的 URL 段不同
// （callback / agreement-notify），而回调那条路的 controller 常量与它是硬编码的第二份
// （见 notifyURL 的注释，同样的取舍：controller 已经 import service，反过来引就是环）。
func (s *PaymentService) agreementNotifyURL(channelCode string) string {
	if s.notifyBaseURL == "" {
		return ""
	}
	return s.notifyBaseURL + "/v1/payments/agreement-notify/" + channelCode
}

// createAgreementRequestHash 是签约幂等键上那条「同一个 key 换了请求体」判定用的哈希。
//
// subject 与 traceID 不算进去，理由与 createRequestHash 逐字相同：前者是展示文案（改一个字
// 不该让一次重试变成冲突），后者每条请求都不同（算进去会让每次重试都变成冲突）。
// maxChargeAmount 算进去——它是签约要素，改了它就不是同一次签约。
func createAgreementRequestHash(in CreateAgreementRequest) string {
	return repository.RequestHash(struct {
		UserID          string
		PaymentMethod   string
		PlanCode        string
		ProviderPlanID  string
		WalletOpenID    string
		MaxChargeAmount int64
	}{
		UserID:          in.UserID,
		PaymentMethod:   in.PaymentMethod,
		PlanCode:        in.PlanCode,
		ProviderPlanID:  in.ProviderPlanID,
		WalletOpenID:    in.WalletOpenID,
		MaxChargeAmount: in.MaxChargeAmount,
	})
}

// agreementNo 生成我们自己的协议号（见 CreateAgreement 里「一个协议号，两个身份」）。
//
// 形状与 paymentNo / refundNo 一致：前缀 + 秒级时间戳 + 随机后缀。**长度是有约束的**：
// 这个号会被当作渠道侧的 contract_code 发出去，而渠道对它有字符集与长度上限（微信是 32 位
// 以内的字母数字）。23 位留足了余量，而前缀与时间戳让人一眼能读出「这是哪一天建的协议」。
func agreementNo(now time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		// 与 paymentNo 同一条理由：crypto/rand 在 darwin/linux 上不会失败，真失败了也绝不退回
		// 固定值——那会让同一秒内建出来的协议号完全由时间戳决定，撞号从概率问题变成必然问题。
		panic(fmt.Sprintf("payment-service: read random for agreement number: %v", err))
	}
	tail := uint32(random[0])<<16 | uint32(random[1])<<8 | uint32(random[2])
	return fmt.Sprintf("AGR%s%06d", now.Format("20060102150405"), tail%1000000)
}
