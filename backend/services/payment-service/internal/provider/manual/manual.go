// Package manual 是一个**手工/模拟渠道**的适配器：它不对接任何真实支付公司，
// 目的是让发起支付这条链路本地就能端到端真跑起来。
//
// 为什么值得先落它，而不是直接写微信：
//
//   - 真实渠道需要商户凭据（商户号、证书、API v3 密钥），这台机器上没有；没有凭据时写出来的
//     微信适配器只能验参数构造，那不是验证，是猜。
//   - 但**验签这条路必须真的验**。防重放与「验签失败绝不改支付状态」是支付服务最关键的两条
//     行为，用桩函数把 Verify 写成 `return nil` 的话，它们永远不会被验到。所以本适配器做真的
//     HMAC-SHA256 校验，密钥从渠道的 secret_ref 指的环境变量读，读不到就拒签。
//   - 真实渠道接进来时，接口不用动：写一个实现 provider.Provider 的新包 + 注册 + 插两行数据。
//
// 它的 Create 立刻返回 success（= 渠道收单了、在等结果），钱要等回调那一步才到。
package manual

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// Name 是它的注册名，与 payment_channels.provider 的值对应。
const Name = "manual"

// SignatureHeader 是回调验签用的请求头。
//
// 带厂商前缀而不是笼统的签名头：将来同时挂几家渠道时，`X-Manual-Signature` 与
// `Wechatpay-Signature` 各自有主，不会互相覆盖，也不需要「先试这家的算法再试那家」。
const SignatureHeader = "X-Manual-Signature"

// NotificationBody 是本适配器的回调报文体。
//
// 它是**适配器私有的**契约，不是我们内部的契约：接真实渠道时这份结构被替换成微信/银联的
// 报文格式，而 provider.Notification 不变，service 一行不改。这正是这层抽象要买到的东西。
type NotificationBody struct {
	// NotificationID 是渠道侧的通知唯一号。它是防重放的全部依据——落库时
	// UNIQUE (provider, notification_id) 挡住第二次。
	NotificationID string `json:"notificationId"`
	// EventType 取 succeeded 或 failed。
	EventType string `json:"eventType"`
	PaymentNo string `json:"paymentNo"`
	// Amount 单位为分。**JSON 里用数字，因为这是渠道发给我们的报文格式，不是我们的
	// 对外 JSON**（方案 13.1 的「金额走字符串」约束说的是我们回给客户端的那些）。
	Amount int64 `json:"amount"`
	// ProviderTransactionID 是建单时我们记下的渠道交易号，回调要原样带回来。
	ProviderTransactionID string `json:"providerTransactionId"`
	// PaidAtUnix 是渠道给的成交时间（Unix 秒）。用渠道时间而不是我们收到回调的时间：
	// 对账以渠道时间为准，我们晚收几小时不该让这笔钱看起来晚收了几小时。
	PaidAtUnix     int64  `json:"paidAtUnix"`
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// Provider 实现 provider.Provider。
type Provider struct{}

// New 构造适配器。它没有状态：密钥每次调用时随请求传进来（见 provider.Method.SecretRef
// 的注释——解析密钥是装配处的事，适配器是可测的纯函数）。
func New() *Provider { return &Provider{} }

// Name 实现 provider.Provider。
func (*Provider) Name() string { return Name }

// Sign 计算回调报文的签名，返回小写 hex。
//
// 导出它是**故意的**：适配器的验签算法必须有一处权威实现，让联调脚本、dev-seed 说明与
// 测试用的是同一份，而不是各自照文档抄一遍——抄错一次的表现是「验签失败」，而那时你分不清
// 是服务错了还是你的脚本错了。
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Create 向「渠道」发起一次支付。
//
// 这里立刻成功：手工渠道没有真实的收单动作，它等于「我们生成了一张待付的单，等有人来付」。
// 真正的成交发生在回调那一步，所以返回的是 provider.ResultSuccess 而不是一个 Suspense 状态。
func (p *Provider) Create(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	if strings.TrimSpace(req.PaymentNo) == "" {
		// 商户订单号是渠道侧对账的锚点，没有它这笔调用根本不该发出去。
		return provider.CreateResult{
			Result:         provider.ResultFailed,
			FailureCode:    "MISSING_PAYMENT_NO",
			FailureMessage: "payment number is required",
		}, errors.New("manual: payment number is required")
	}
	if req.Amount <= 0 {
		// 应付金额为 0 或负数的单在 payments 表的 CHECK 那关就会挂。在适配器里先拦住，
		// 是为了让「是不是我传错了金额」在日志里一眼可见，而不是变成一次数据库约束报错。
		return provider.CreateResult{
			Result:         provider.ResultFailed,
			FailureCode:    "INVALID_AMOUNT",
			FailureMessage: "amount must be positive",
		}, fmt.Errorf("manual: amount must be positive, got %d", req.Amount)
	}

	transactionID := "MANUAL-" + req.PaymentNo
	// payUrl 允许由运营在 payment_methods.params 里配（指向一个真的收银页或一个说明页）。
	// 没配就给空串：客户端据此显示「请联系店员」，联调脚本则直接打回调地址。
	payURL := strings.TrimSpace(req.Method.Params["payUrl"])

	return provider.CreateResult{
		Result:                provider.ResultSuccess,
		ProviderTransactionID: transactionID,
		PayParams: map[string]string{
			"manualPayUrl": payURL,
			"paymentNo":    req.PaymentNo,
			// 金额一律字符串：这份 map 原样回到客户端，方案 13.1 要求 JSON 里的金额
			// 不能用数字——JS 的数字精度接不住分。
			"amount": strconv.FormatInt(req.Amount, 10),
			// 渠道侧交易号，联调时回调体要原样带上它（服务会拿它跟建单时记下的比对）。
			"providerTransactionId": transactionID,
			// 回调地址交给「渠道」：联调时照它 POST，不用去翻配置。
			"notifyUrl": req.NotifyURL,
			"expiresAt": strconv.FormatInt(req.ExpiresAt.Unix(), 10),
		},
		ResponseSummary: map[string]any{
			"providerTransactionId": transactionID,
			"amount":                req.Amount,
			"mode":                  req.Method.Mode,
			"notifyUrl":             req.NotifyURL,
		},
	}, nil
}

// Verify 验签并归一化一次回调。
//
// 三步，任何一步不过都返回错误、**绝不返回一个看起来验过了的 Notification**：
//
//  1. 密钥读不到 → 拒（ErrSecretNotConfigured）。验不了签等于没有防重放，放行是错的。
//  2. 签名对不上 → 拒（ErrSignatureMismatch）。用 hmac.Equal 做定长比较，不用 ==：
//     字符串比较会在第一个不同的字节就返回，理论上能被计时侧信道利用。
//  3. 签名过了但报文缺必需字段 → 拒（ErrInvalidNotification）。签名只证明「这报文是我们
//     的密钥签的」，不证明「它是一份完整的成交通知」。
func (p *Provider) Verify(_ context.Context, req provider.NotificationRequest) (provider.Notification, error) {
	if strings.TrimSpace(req.Secret) == "" {
		// 注意这里不区分「渠道没配 secret_ref」与「配了但环境变量是空的」：对验签来说
		// 两者都得拒，而排查时看错误里的渠道码就够了。
		return provider.Notification{}, fmt.Errorf("%w: channel %q", provider.ErrSecretNotConfigured, req.ChannelCode)
	}

	got, err := hex.DecodeString(strings.TrimSpace(req.Headers.Get(SignatureHeader)))
	if err != nil {
		// 头不是合法 hex：当作签名不对，不当作「没带签名头」。两者对调用方是同一个处置。
		return provider.Notification{}, fmt.Errorf("%w: channel %q signature header is not hex", provider.ErrSignatureMismatch, req.ChannelCode)
	}
	want := hmac.New(sha256.New, []byte(req.Secret))
	want.Write(req.Body)
	if !hmac.Equal(got, want.Sum(nil)) {
		return provider.Notification{}, fmt.Errorf("%w: channel %q", provider.ErrSignatureMismatch, req.ChannelCode)
	}

	var body NotificationBody
	decoder := json.NewDecoder(strings.NewReader(string(req.Body)))
	// 不 DisallowUnknownFields：这是**对方的报文格式**，加字段是渠道的权利，不该让
	// 我们这边报错。跨服务事件（payment.succeeded）走的是另一条规矩——那是我们自己的
	// 契约，多一个字段必须炸（见 service 的 callback 测试）。
	if err := decoder.Decode(&body); err != nil {
		return provider.Notification{}, fmt.Errorf("%w: channel %q body is not json: %v", provider.ErrInvalidNotification, req.ChannelCode, err)
	}

	if strings.TrimSpace(body.NotificationID) == "" {
		return provider.Notification{}, fmt.Errorf("%w: channel %q has no notificationId", provider.ErrInvalidNotification, req.ChannelCode)
	}
	if strings.TrimSpace(body.PaymentNo) == "" {
		// 没有支付单号的回调无法落到任何一单上。签名是真的，但这说明对面拼错了报文。
		return provider.Notification{}, fmt.Errorf("%w: channel %q has no paymentNo", provider.ErrInvalidNotification, req.ChannelCode)
	}
	event := provider.NotificationEvent(body.EventType)
	switch event {
	case provider.EventSucceeded:
		if body.Amount <= 0 {
			// 成功通知必须带金额：金额对账是「这笔回调是不是真的属于这一单」的核心判据，
			// 缺了它就只能选择相信渠道——那是我们绝不该做的选择。
			return provider.Notification{}, fmt.Errorf("%w: channel %q succeeded notification has no amount", provider.ErrInvalidNotification, req.ChannelCode)
		}
	case provider.EventFailed:
		// 失败通知不要求金额（渠道可能压根没生成金额）。失败码为空也接受：有些渠道只给
		// 一个「用户取消」的信号。
	default:
		return provider.Notification{}, fmt.Errorf("%w: channel %q has unknown eventType %q", provider.ErrInvalidNotification, req.ChannelCode, body.EventType)
	}

	return provider.Notification{
		NotificationID:        strings.TrimSpace(body.NotificationID),
		EventType:             event,
		PaymentNo:             strings.TrimSpace(body.PaymentNo),
		Amount:                body.Amount,
		ProviderTransactionID: strings.TrimSpace(body.ProviderTransactionID),
		PaidAt:                paidAt(body.PaidAtUnix),
		FailureCode:           strings.TrimSpace(body.FailureCode),
		FailureMessage:        strings.TrimSpace(body.FailureMessage),
	}, nil
}

// Ack 实现 provider.Provider：手工渠道的应答约定。
//
// 拒绝时回 **400 而不是 200 带失败码**：这个渠道是我们自己写的，重投策略由我们自己定，
// 而「非 2xx 就重投」是 HTTP 上最不容易被误读的一条。真实渠道里两种写法都有（微信 v2 用
// 200 + return_code，支付宝用非 200），所以这正好是一个该由适配器决定、由接口传出去的例子。
//
// 应答体里不放拒绝原因：原因在 payment_notifications.failure_reason 里，回给渠道既没用
// 又是信息泄漏。
func (*Provider) Ack(accepted bool) provider.Ack {
	if accepted {
		return provider.Ack{
			Status:      http.StatusOK,
			ContentType: "application/json",
			Body:        []byte(`{"code":"SUCCESS","message":"ok"}`),
		}
	}
	return provider.Ack{
		Status:      http.StatusBadRequest,
		ContentType: "application/json",
		Body:        []byte(`{"code":"FAIL","message":"notification rejected"}`),
	}
}

// paidAt 把渠道给的 Unix 秒翻成时间。
//
// 0 或负数一律当作「渠道没给」，返回零值；调用方看到零值就退回用 NOW()。**不能拿 0 直接
// 建 time.Unix(0)**：那会写成 1970 年，一条 1970 年的资金流水会让对账报表永远对不平。
func paidAt(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}
