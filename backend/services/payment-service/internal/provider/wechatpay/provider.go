package wechatpay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

// Provider 实现 provider.Provider（Name / Create / Verify / Ack），并额外实现
// provider.SecretSlotter、provider.AgreementKeeper 与 provider.AgreementNotifier。
//
// # 这一族只做协议，不做收款
//
// 它进目录的理由与别的渠道不同：微信直连委托代扣在 **payment-service** 里是签约通道，
// 不是收单通道。会员的付款仍然走银联商务（V2 的下单链路），而这条通道管的是「以后每个月
// 自动扣款」这件事的**授权**——签约、查约、解约，以及扣款本身。
//
// 所以 Create 与 Verify 在这里都是**明确拒绝**：它们服务的是「支付聚合」那条路，而这条通道
// 上没有支付单。这不是偷懒的桩，是一句必须能被读到的边界声明——真有人拿 catalog 里的
// `wechat_papay` 去发起支付时，他要得到的是「这条通道不做收款」，而不是一张永远等不到回调的
// 支付单。
//
// 它**不带任何状态**：协议声明每次调用从 method.ChannelConfig 现解析（配置是一张常量表，
// 解析很便宜），而客户端的装配只看配置，跟着请求走。
type Provider struct{}

// New 构造适配器。
func New() *Provider { return &Provider{} }

// Name 实现 provider.Provider。
func (*Provider) Name() string { return Name }

// SecretSlots 实现 provider.SecretSlotter：这一族**一把**凭据（APIv2 密钥）。
//
// 签名与验签用的是同一把：微信 APIv2 的回调用同一套算法、同一把密钥签。所以这里报一个槽，
// 而不是「出站一把、入站一把」那种两把的形状。
//
// 商户证书与私钥**不在这里**：它们是文件路径，不是密钥槽——槽是给「一把字符串密钥」的，
// 而证书是商户平台下发的 PEM 文件（见 Protocol.certFile）。
//
// 解析失败时返回空切片而不是报错，理由与 ums 逐字相同：这个方法没有错误可返回，而它唯一的
// 调用方（装配处）拿到空切片会退回渠道行的 secret_ref。配置坏掉这件事会在紧随其后的 Sign /
// Query 里以一条完整得多的错误爆出来。
func (*Provider) SecretSlots(method provider.Method) []string {
	protocol, err := Parse(method.ChannelConfig)
	if err != nil {
		return nil
	}
	return []string{protocol.secretRef}
}

// RecordableHeaders 实现 provider.HeaderRecorder：**一个头都不留**。
//
// 微信 APIv2 的回调把签名放在**报文体**里（一个 `sign` 字段），请求头里没有任何协议凭证
// ——没有时间戳头、没有随机串头、没有签名头。所以这一族的白名单是空的，不是因为「不方便」，
// 而是因为那份报文里确实没有可留的头。
func (*Provider) RecordableHeaders() []string { return nil }

// Ack 实现 provider.Provider：按 APIv2 的约定应答一次回调。
//
// 成功 `<xml><return_code>SUCCESS</return_code></xml>`，失败 `<xml><return_code>FAIL</return_code></xml>`
// ——微信只认这两个词，而且**失败也要回 200**：那是它的重推信号。回非 2xx 在微信那边是
// 「网络错误」，同样会重推，但会把它记成一次投递失败（渠道侧的错误率），而实际上是我们
// 主动拒了这条通知。
//
// 应答体里**不写失败原因**：把内部原因回给渠道既没用又是信息泄漏（见 provider.Ack 的注释）。
// 这条与老系统有个细微差别：老系统把 err.Error() 拼进了 return_msg（subscription_handler.go:130），
// 那些串里有我们自己的单号与错误路径。这里不回。
func (*Provider) Ack(_ provider.Method, accepted bool) provider.Ack {
	body := `<xml><return_code>SUCCESS</return_code></xml>`
	if !accepted {
		body = `<xml><return_code>FAIL</return_code></xml>`
	}
	return provider.Ack{
		Status:      http.StatusOK,
		ContentType: "application/xml",
		Body:        []byte(body),
	}
}

// 「这条通道不做收款」的两个拒绝。
//
// 它们分开是因为**读的人不同**：Create 那条说的是「你来错地方了，收款走银联」，
// Verify 那条说的是「这份报文不是支付回调，签约通知有它自己的入口」。
var (
	// ErrNotAPaymentChannel：这条通道只签协议，不建支付单。
	ErrNotAPaymentChannel = errors.New("wechat pay direct-connect is an agreement channel, not a payment channel")
	// ErrNotAPaymentNotification：这条通道上没有支付回调，只有签约通知。
	ErrNotAPaymentNotification = errors.New("wechat pay direct-connect has no payment notification, only agreement notifications")
)

// Create 实现 provider.Provider：**拒绝**。
//
// 契约要求它必须在 err 非 nil 时也填 Result（见 provider.Provider.Create 的注释），所以这里
// 回一个 ResultFailed——调用方对「适配器明确拒绝」的处置就是把这次发起记成失败，而不是
// 记成「结果不明、等回调」。
func (p *Provider) Create(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	return provider.CreateResult{
		Result:         provider.ResultFailed,
		FailureCode:    "NOT_A_PAYMENT_CHANNEL",
		FailureMessage: ErrNotAPaymentChannel.Error(),
		ResponseSummary: map[string]any{
			"channelCode":   req.Method.ChannelCode,
			"paymentMethod": req.Method.Code,
		},
	}, fmt.Errorf("%w: %s", ErrNotAPaymentChannel, req.Method.ChannelCode)
}

// Verify 实现 provider.Provider：**拒绝**。
//
// 微信往我们这儿推两类报文：协议变更通知（带 change_type）与扣款结果通知（带 result_code）。
// 它们各有自己的入口——前者 `/v1/payments/agreement-notify/{渠道码}`（改的是协议），后者
// `/v1/payments/agreement-charge-notify/{渠道码}`（改的是某一期扣款）。两条都**不是支付聚合
// 的一条出资**：payment_agreement_charges 有自己的状态，而 payments.order_no 那列是 NOT NULL
// 的订单号（见迁移 001），代扣没有订单可挂。
//
// 所以这一族在这条路上没有能返回的 provider.Notification（它要求一个 payment_no），拒绝是
// 诚实的答案。把报文送到这里来是一个配置错误（notify_url 配错了地址），而该说的话是「这条
// 报文不是支付结果」，不是一句语焉不详的「验签失败」。
func (p *Provider) Verify(_ context.Context, req provider.NotificationRequest) (provider.Notification, error) {
	return provider.Notification{}, fmt.Errorf("%w: %s", ErrNotAPaymentNotification, req.ChannelCode)
}

// VerifyAgreementNotification 实现 provider.AgreementNotifier：认一份协议变更通知。
//
// 它就是上面那条拒绝的**正面**：签约通知有自己的入口（`/v1/payments/agreement-notify/{渠道码}`），
// 报文解析与验签在 notify.go 里，这里只做这件事该在适配器层做的那一步——**把凭据取出来**。
//
// 取不到密钥就拒绝（与 Sign / QueryAgreement 同一条规矩）：这里返回的
// provider.ErrSecretNotConfigured 会在 service 那边被记成一条「签名没验成」的通知，
// 而不是「报文不对」——那正是事实，而且它的排查方向（配置）完全不同。
//
// 摘要用 responseSummary 的白名单，它已经盖住了协议通知里的那几个字段（contract_id /
// contract_code / contract_state / return_code / result_code / err_code）：白名单存在
// 就是为了「不整份抄进流水」，而这份报文里其余字段对我们没有意义。
func (p *Provider) VerifyAgreementNotification(_ context.Context, req provider.NotificationRequest) (provider.AgreementNotification, error) {
	var notification provider.AgreementNotification

	protocol, err := Parse(req.Method.ChannelConfig)
	if err != nil {
		return notification, err
	}
	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		return notification, fmt.Errorf("%w: slot %q for channel %q",
			provider.ErrSecretNotConfigured, protocol.secretRef, req.ChannelCode)
	}

	notify, params, err := ParseAgreementNotify(req.Body, secret)
	if err != nil {
		return notification, err
	}
	return provider.AgreementNotification{
		AgreementNo:     notify.ContractCode,
		ContractNo:      notify.ProviderContractID,
		State:           notify.State(),
		ProviderState:   string(notify.ProviderState),
		ResponseSummary: responseSummary(params),
	}, nil
}

// VerifyAgreementChargeNotification 实现 provider.AgreementChargeNotifier：认一份扣款结果通知。
//
// 与 VerifyAgreementNotification 逐字同构（解析 + 验签在 notify.go，这里只把凭据取出来），
// 差别只在翻出来的结构：那一份说的是协议状态，这一份说的是某一期扣到了没有。
//
// 摘要同样走 responseSummary 的白名单。**total_fee 会在里面**——它在白名单上（那是我们发过去
// 的数，渠道原样回显），而这一条通知的金额正是调用方要拿去与本地那一期对的东西。
func (p *Provider) VerifyAgreementChargeNotification(_ context.Context, req provider.NotificationRequest) (provider.AgreementChargeNotification, error) {
	var notification provider.AgreementChargeNotification

	protocol, err := Parse(req.Method.ChannelConfig)
	if err != nil {
		return notification, err
	}
	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		return notification, fmt.Errorf("%w: slot %q for channel %q",
			provider.ErrSecretNotConfigured, protocol.secretRef, req.ChannelCode)
	}

	notify, params, err := ParseChargeNotify(req.Body, secret)
	if err != nil {
		return notification, err
	}
	return provider.AgreementChargeNotification{
		OutTradeNo:            notify.OutTradeNo,
		ProviderTransactionID: notify.ProviderTransactionID,
		ProviderContractID:    notify.ProviderContractID,
		Amount:                notify.Amount,
		Result:                notify.Result,
		FailureCode:           notify.FailureCode,
		FailureMessage:        notify.FailureMessage,
		ResponseSummary:       responseSummary(params),
	}, nil
}

// ============================================================
// provider.AgreementKeeper
// ============================================================

// Sign 实现 provider.AgreementKeeper：算出一份纯签约的参数，**不发任何请求**。
//
// 返回的 PayParams 就是客户端要带去微信签约小程序的那 8 个字段 + sign，另外附上跳转目标
// （SignMiniProgramAppID / SignMiniProgramPath）。老系统把这两样分开返回（subscription_service
// 的 SignParams 结构体），这边合并进扁平表，因为 provider.CreateResult.PayParams 就是那个形状
// ——签约与支付在客户端那边是同一件事的两个形态（都是「拿一堆参数调起微信」）。
func (p *Provider) Sign(_ context.Context, req provider.AgreementSignRequest) (provider.AgreementSignResult, error) {
	result := provider.AgreementSignResult{ContractCode: req.ContractCode}

	protocol, err := Parse(req.Method.ChannelConfig)
	if err != nil {
		return result, err
	}
	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		// 与出站调用同一条规矩：**取不到密钥就拒绝**，绝不降级成空签名。
		return result, fmt.Errorf("%w: slot %q for channel %q",
			provider.ErrSecretNotConfigured, protocol.secretRef, req.Method.ChannelCode)
	}

	signed, err := NewClient(protocol).SignParams(SignRequest{
		PlanID:                 req.PlanID,
		ContractCode:           req.ContractCode,
		ContractDisplayAccount: req.WalletOpenID,
		NotifyURL:              req.NotifyURL,
	}, secret)
	if err != nil {
		// 算不出签名是**我们自己的问题**（密钥没配、套餐没配），所以它是一条 err 而不是
		// 一个「渠道拒了」的结果——调用方据此回 5xx/503，不去动协议状态。
		result.FailureCode = "SIGN_FAILED"
		result.FailureMessage = err.Error()
		return result, err
	}

	payParams := make(map[string]string, len(signed.Params)+3)
	for key, value := range signed.Params {
		payParams[key] = value
	}
	payParams["request_serial"] = signed.RequestSerial
	// 跳转目标：微信官方的签约小程序（不是我们自己的）。appid 来自配置（见 Protocol），
	// 不再写死在包里。
	payParams["mini_program_appid"] = protocol.signMiniProgramAppID
	payParams["mini_program_path"] = SignMiniProgramPath
	result.PayParams = payParams

	// 摘要里**不放 sign**：签名原文与签名值都不进流水（见 provider.CreateResult.ResponseSummary）。
	// 留的是能定位这一份协议的那几个值。
	result.ResponseSummary = map[string]any{
		"planId":       signed.Params["plan_id"],
		"contractCode": signed.Params["contract_code"],
		"timestamp":    signed.Params["timestamp"],
	}
	return result, nil
}

// QueryAgreement 实现 provider.AgreementKeeper：回渠道问一份协议的状态。
//
// 三个「等于没签」的错误码与「-25 进行中」的处置在 Client.QueryContract 里，这里只把它的
// 结论翻成渠道无关的 provider.AgreementState。
func (p *Provider) QueryAgreement(ctx context.Context, req provider.AgreementQuery) (provider.AgreementQueryResult, error) {
	result := provider.AgreementQueryResult{}

	protocol, secret, err := p.protocolAndSecret(req.Method, req.Secrets)
	if err != nil {
		return result, err
	}
	queried, call, err := NewClient(protocol).QueryContract(ctx, secret, ContractQuery{
		PlanID:       req.PlanID,
		ContractCode: req.ContractCode,
	})
	fillCall(&result.Result, &result.FailureCode, &result.FailureMessage, &result.ResponseSummary,
		&result.HTTPStatus, &result.Attempts, call, err)
	result.ProviderContractID = queried.ProviderContractID
	result.TerminationMode = queried.TerminationMode
	if err != nil {
		return result, err
	}
	result.State = provider.AgreementState(queried.State)
	return result, nil
}

// TerminateAgreement 实现 provider.AgreementKeeper：解一份协议。**需要商户证书**。
func (p *Provider) TerminateAgreement(ctx context.Context, req provider.AgreementTerminate) (provider.AgreementCallResult, error) {
	result := provider.AgreementCallResult{}

	protocol, secret, err := p.protocolAndSecret(req.Method, req.Secrets)
	if err != nil {
		return result, err
	}
	call, err := NewClient(protocol).TerminateContract(ctx, secret, TerminateContract{
		ProviderContractID: req.ProviderContractID,
		Remark:             req.Reason,
	})
	fillCall(&result.Result, &result.FailureCode, &result.FailureMessage, &result.ResponseSummary,
		&result.HTTPStatus, &result.Attempts, call, err)
	return result, err
}

// Charge 实现 provider.AgreementKeeper：对一份已签约的协议发起一次扣款。**需要商户证书**。
//
// # 预扣费通知在这里发，不在服务层
//
// 微信要求扣款**之前**先给用户一条预扣费通知（`papay/pap_pay_apply`，一条单向告知，没有
// 结果回来）。它是**这一族协议的要求**，不是业务规则：换一家渠道就没有这一步，而服务层
// 不该知道「微信想提前打个招呼」。写在适配器里还有一个正确的副作用——每一次逻辑尝试
// （一次 Charge 调用）发一条，而两次尝试之间隔着 24h 以上（model.ChargeBackoff），正好是
// 那条通知「扣款前发出」的时限；同一次调用内部的 HTTP 重试不会重复发。
//
// **它失败不阻断扣款**：老系统的处置是记一条 warn 继续（subscription_service.go:630-632），
// 这里同一条——通知是给用户看的礼貌，钱该扣还得扣。成败记进这次调用的应答摘要（preNotify
// 那一段），不另开一行 payment_provider_calls（见 model.CallOperationAgreementCharge）。
//
// **受理不等于扣到钱**：微信这个接口是异步的，result_code=SUCCESS 只说明请求被收下了，
// 真正到账与否在回调里（见 Client.Charge 的注释）。
func (p *Provider) Charge(ctx context.Context, req provider.AgreementChargeRequest) (provider.AgreementChargeResult, error) {
	result := provider.AgreementChargeResult{}

	protocol, secret, err := p.protocolAndSecret(req.Method, req.Secrets)
	if err != nil {
		return result, err
	}
	client := NewClient(protocol)

	preNotify := p.preNotify(ctx, client, secret, req)

	charged, call, err := client.Charge(ctx, secret, ChargeRequest{
		ProviderContractID: req.ProviderContractID,
		OutTradeNo:         req.OutTradeNo,
		Body:               req.Subject,
		TotalFee:           req.Amount,
		NotifyURL:          req.NotifyURL,
	})
	fillCall(&result.Result, &result.FailureCode, &result.FailureMessage, &result.ResponseSummary,
		&result.HTTPStatus, &result.Attempts, call, err)
	result.ProviderTransactionID = charged.ProviderTransactionID
	if result.ResponseSummary == nil {
		result.ResponseSummary = map[string]any{}
	}
	result.ResponseSummary["preNotify"] = preNotify
	return result, err
}

// preNotify 发那条预扣费通知，并把结果压成一段能进流水的小结。
//
// 它**永不返回错误**：这条通知的失败已经被判定为不阻断扣款，把它的错误往外传就得让每一个
// 调用点都记得忽略它——那种「调用方必须记得忽略」的接口，早晚会有一个调用点把失败当成功
// 的替代品。所以失败在这里收口，只留一段摘要（成败、渠道原话、网络结果），排查时看流水。
//
// 摘要里**不放报文**：这条通知的参数里有 contract_id 与 out_trade_no，它们已经在本行别处
// 记着；响应里回显的字段照 responseSummary 的白名单挑。
func (p *Provider) preNotify(ctx context.Context, client *Client, secret string, req provider.AgreementChargeRequest) map[string]any {
	call, err := client.PreNotify(ctx, secret, PreNotifyRequest{
		ProviderContractID: req.ProviderContractID,
		OutTradeNo:         req.OutTradeNo,
		Body:               req.Subject,
		TotalFee:           req.Amount,
		// deduct_date 就是今天：我们是在「该扣的这一天」发这条通知的，与老系统逐字一致
		// （subscription_service.go:623）。微信那边它是「预计扣款日」，不是我们这边的排期。
		DeductDate: time.Now().Format("20060102"),
	})

	summary := map[string]any{"ok": err == nil}
	if err != nil {
		summary["error"] = err.Error()
	}
	if len(call.Response) > 0 {
		summary["response"] = responseSummary(call.Response)
	}
	return summary
}

// protocolAndSecret 是三个出站方法共用的开头：解析配置、取出密钥。
//
// 抽出来的理由与 ums 的 authorize 相同——分开写三遍必然有一处少判一次密钥为空，而那处的
// 表现是「签名算出来是个空串」，微信回一句 SIGNERROR，看不出是没配密钥。
func (p *Provider) protocolAndSecret(method provider.Method, secrets provider.Credentials) (*Protocol, string, error) {
	protocol, err := Parse(method.ChannelConfig)
	if err != nil {
		return nil, "", err
	}
	secret := secrets.Get(protocol.secretRef)
	if secret == "" {
		return nil, "", fmt.Errorf("%w: slot %q for channel %q",
			provider.ErrSecretNotConfigured, protocol.secretRef, method.ChannelCode)
	}
	return protocol, secret, nil
}

// fillCall 把一次出网调用的结果填进那几个与 CreateResult 同形的字段。
//
// 为什么一个函数而不是四份：查询、解约、代扣三条路的**结论分类完全相同**——「渠道说不行」
// 是 ResultFailed，「没发出去」是我们自己的问题，「发出去了没等到应答」结果不明。分开写
// 四遍的实现里，必然有一条会把 BusinessError 记成 timeout。
//
// 判据与 ums.transportFailure 逐字相同：**只有 httpx 明确标了 NotSent 的才算没发出去**。
func fillCall(result *provider.Result, failureCode, failureMessage *string, summary *map[string]any,
	httpStatus, attempts *int, call CallResult, err error) {
	*httpStatus = call.HTTPStatus
	*attempts = call.Attempts
	*summary = responseSummary(call.Response)

	switch {
	case err == nil:
		*result = provider.ResultSuccess
		return
	}
	*result = provider.ResultUnknown
	*failureMessage = err.Error()

	var business *BusinessError
	if errors.As(err, &business) {
		// 渠道收了报文、明确说不行。这是最干净的一种失败：钱没有动。
		*result = provider.ResultFailed
		*failureCode = business.FailureCode()
		return
	}
	var httpErr *httpx.Error
	if errors.As(err, &httpErr) && httpErr.NotSent {
		// 一个字节都没发出去：我们自己的问题（域名、证书、网络）。结果不明是**安全的**
		// 那一侧——调用方不会因为一次 DNS 失败去改协议状态。
		*failureCode = "NOT_SENT"
		return
	}
	// 发出去了、没等到应答，或者报文读不懂：**结果不明**。签约这条路上它的正确处置是
	// 「什么都不做、等重试」，因为一次查约超时被当成「已解约」会把一份有效的协议标死。
	*failureCode = "TIMEOUT"
}

// responseSummary 挑几个能定位这一笔的键进流水，其余一概不留。
//
// 白名单而不是黑名单：一份渠道应答里有什么字段**由渠道决定**，而我们能确定的是「这几个
// 是我们写的、无敏感信息」。整份抄进 payment_provider_calls.response_summary 的后果是把
// 渠道回显的一切（它可能把我们发过去的字段原样带回来）存进一张人人可读的表。
func responseSummary(response map[string]string) map[string]any {
	if len(response) == 0 {
		return nil
	}
	summary := make(map[string]any, len(summaryKeys))
	for _, key := range summaryKeys {
		if value, ok := response[key]; ok {
			summary[key] = value
		}
	}
	return summary
}

// summaryKeys 是允许进流水的应答字段。全部是我们自己发过去、渠道原样回显的那几个，
// 加上渠道侧的状态说明。
//
// total_fee 在名单上是因为扣款通知那条路要用它：那条报文里的金额是调用方拿去与本地那一期对
// 账的东西（见 repository.ErrChargeAmountMismatch），流水里没有它就只能去翻报文原文。
var summaryKeys = []string{
	"return_code", "return_msg", "result_code", "err_code", "err_code_des",
	"contract_id", "contract_code", "contract_state", "transaction_id", "out_trade_no",
	"total_fee",
}
