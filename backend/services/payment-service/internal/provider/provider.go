// Package provider 是渠道适配器：把「向一家渠道收一笔钱」抽象成一个接口。
//
// 这个包存在的理由是上一轮定下的那条不变量——`payment_methods.action` 是**数据**，
// 行为分派认它、不认 `code`（见 model/payment_method.go 与
// [[payment-method-action-dispatch]]）。于是：
//
//   - 同形态的新渠道 = 插一行 payment_channels + 一行 payment_methods，客户端零改动；
//   - 只有全新形态（一个上面六种 action 都装不下的交互）才需要加一个 action 取值，
//     那本来就要改客户端，所以不算额外负担；
//   - 每家渠道的实现差别被关在一个 Provider 里，不出现在 service 层。
//
// 接一家真实渠道 = 写一个实现 Provider 的包 + 在 Registry 里注册 + 插两行数据。
// **不改客户端、不改表约束、不改 service。**
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrProviderNotConfigured：payment_channels.provider 指向一个没有注册适配器的名字。
	//
	// 这是一次**明确的 5xx 而不是静默降级**：渠道行存在、代码不认识它，说明配置与代码
	// 版本对不上（回滚到旧版本、或者插数据时写错了 provider 名）。返回一个空支付参数
	// 会让用户看到一个「点了没反应」的支付，而真正的原因消失在日志里。
	ErrProviderNotConfigured = errors.New("payment channel provider is not registered")
	// ErrSecretNotConfigured：渠道的 secret_ref 指向的密钥读不到。
	//
	// 验签路径上遇到它必须**拒绝**，不能放行：验不了签就等于没有防重放。而且它与
	// 「签名不对」是两件事——前者是运维漏配，后者可能是有人在伪造回调。
	ErrSecretNotConfigured = errors.New("payment channel secret is not configured")
	// ErrSignatureMismatch：回调验签不通过。
	ErrSignatureMismatch = errors.New("payment notification signature does not match")
	// ErrInvalidNotification：验签过了，但报文里缺必需的字段。
	ErrInvalidNotification = errors.New("payment notification is malformed")
)

// Action 是「这条支付方式被选中之后怎么起支付」，取值与 payment_methods.action 的
// CHECK 逐字一致。
//
// 客户端只 switch 它、不 switch 渠道 code——这是「新增一家同形态渠道不用改客户端」的
// 全部依据。加第七个取值意味着要同步改客户端，所以它不该随手加。
type Action string

const (
	// ActionJumpMiniapp：跳对方小程序（丰选万联、优联、首创饭卡这类）。
	ActionJumpMiniapp Action = "jump_miniapp"
	// ActionNativePay：小程序内 requestPayment（微信、银联）。
	ActionNativePay Action = "native_pay"
	// ActionDirectPay：对接方直接扣款、不跳转（北方工业饭卡）。
	ActionDirectPay Action = "direct_pay"
	// ActionQrcode：扫普通二维码。
	ActionQrcode Action = "qrcode"
	// ActionH5：跳 H5 收银台。
	ActionH5 Action = "h5"
	// ActionAccount：走本仓库的账户服务扣余额（咖啡豆）。account-service 今天只有福卡
	// 账户，咖啡豆账户还没有归属，这条 action 仍然落不了地，见 Service 的发起支付路径。
	ActionAccount Action = "account"
)

// Valid 判断一个 action 取值是不是已知的六种之一。
//
// 与数据库的 CHECK 重复了一遍，这是**有意的**：数据库挡的是写错的数据，这里挡的是
// 「代码里的 switch 漏了一个分支」。从库里读出来的值一定合法，但把校验放在这里，
// 才能让「加了新 action 却忘了在服务里处理」在第一次调用时就报错而不是走 default。
func (a Action) Valid() bool {
	switch a {
	case ActionJumpMiniapp, ActionNativePay, ActionDirectPay, ActionQrcode, ActionH5, ActionAccount:
		return true
	}
	return false
}

// Result 是一次渠道调用的分类，取值与 payment_provider_calls.result 的 CHECK 逐字一致。
type Result string

const (
	// ResultSuccess：渠道明确接受了这次请求。
	ResultSuccess Result = "success"
	// ResultFailed：渠道明确拒绝了。**可以重试**（换一张单）。
	ResultFailed Result = "failed"
	// ResultTimeout：请求发出去了，我们没等到应答。
	//
	// 与 ResultFailed 分开是必须的：失败可以重试，超时不能直接重试——钱可能已经收了。
	// 正确动作是拿商户单号去渠道查单，这正是 payment_provider_calls 要留痕的原因。
	ResultTimeout Result = "timeout"
	// ResultUnknown：连「发出去没有」都不确定。
	ResultUnknown Result = "unknown"
)

// Method 是发起一次支付所需的渠道侧事实：payment_methods ⋈ payment_channels 的投影。
//
// 它是个**值**，不是 model.PaymentMethod：适配器不该看到 status、sort_order、created_at
// 这些与「怎么起支付」无关的列，也不该有能力改数据。装配处（service）负责把两张表的行
// 拼成它，适配器只读。
type Method struct {
	// ID 是 payment_methods.id，写进 payments.payment_method_id。
	ID string
	// Code 是给人看的标识（后台配置、日志排查），**不用来做分派**。
	Code string
	// Action 才是分派依据。
	Action Action
	// FundingType 是本方式对应的出资类型，落到 payments.funding_type，再由事件带给
	// order-service 写 orders.payment_method。
	FundingType string
	// Params 是 payment_methods.params：跳转 path、附加 query 等，**不放密钥**。
	Params map[string]string

	// —— 以下是 payment_channels 的投影，账户出资方式没有渠道，这些全为零值 ——
	//
	// ChannelID 是 payment_channels.id，写进 payments.channel_id。
	ChannelID string
	// ChannelCode 是渠道代码，回调路径里的那段就是它。
	ChannelCode string
	// Provider 是适配器名字，Registry 按它查实现。
	Provider string
	// Mode 是 sandbox / live。**适配器必须看它**：沙箱与生产的密钥、域名、商户号都不同，
	// 不看就可能把测试单打到生产渠道上。
	Mode string
	// ChannelConfig 是 payment_channels.config：商户号、appid、回调地址等非敏感参数。
	// 返回客户端时与 Params 两层合并，所以两边都**不放密钥**。
	ChannelConfig map[string]string
	// SecretRef 是密钥在受控 Secret 里的**键名，不是密钥本身**。适配器自己不去解析它——
	// 解析要读环境或 Secret 系统，那是装配处的事，由 service 把解出来的 Secret 传进
	// CreateRequest / NotificationRequest。这样适配器是可测的纯函数。
	SecretRef string
}

// CreateRequest 是一次发起支付所需的全部输入。
//
// 金额由**调用方权威给出**（order-service 在锁内校验完订单后通过 gRPC 传来），本服务
// 不读订单库、也不对着订单库复核——订单事实的归属方是订单服务（方案 5.9 给支付服务的
// 职责清单里本来就没有「读订单」这一项）。
type CreateRequest struct {
	// PaymentNo 是我们自己的支付单号，渠道侧的商户订单号用它。
	PaymentNo string
	// OrderNo 是订单号，只用于渠道账单上的展示与对账关联。
	OrderNo string
	UserID  string
	// Amount 单位为分。**这是权威金额**，适配器不许改它再发给渠道。
	Amount int64
	// Subject 是渠道收银台与账单上显示的商品描述。
	Subject string
	// RequestID 是调用方给的幂等号，透传给渠道的商户请求号，方便对账时对时间线。
	RequestID string
	// WalletOpenID 是渠道附加参数里需要用户身份的那一项（微信小程序支付要 openid）。
	WalletOpenID string
	// Attach 是其余渠道附加数据（设备号等）。**不放密钥**。
	Attach map[string]string
	// NotifyURL 是渠道回调我们的地址。它由装配处按渠道配的回调基址拼出来，不进数据库
	// 配置——同一个渠道在 dev / staging / prod 的域名不同，写进 config 就得多一套配置。
	NotifyURL string
	// ExpiresAt 是这单的待支付超时。渠道侧的预支付单有效期应当不长于它，否则会出现
	// 「我们关了单、渠道那边还能付」。
	ExpiresAt time.Time
	// Method 是要用的支付方式及其渠道。
	Method Method
	// Secret 是从 Method.SecretRef 解析出来的密钥，仅当渠道需要签名时非空。
	Secret string
}

// CreateResult 是渠道对一次发起支付的答复。
//
// 无论成败调用方都要把它写进 payment_provider_calls（方案 6：所有第三方适配器都要有
// 调用流水，超时与结果未知尤其要留痕）。
type CreateResult struct {
	// Result 是这次调用的分类，直接写进 payment_provider_calls.result。
	//
	// 规定：适配器**必须**填它。零值会被调用方记成 ResultUnknown——那对「适配器漏填」
	// 来说是恰好正确的兜底：我们确实不知道发生了什么。
	Result Result
	// ProviderTransactionID 是渠道侧的交易号。回调与查单都以它为准，回来的回调必须
	// 原样带上，对不上就拒（见 service 的 callback）。
	ProviderTransactionID string
	// PayParams 是回给客户端的支付参数，扁平字符串键值。
	//
	// 用扁平 map 而不是每种 action 一个结构体：native_pay 的
	// timeStamp/nonceStr/package/signType/paySign、jump_miniapp 的 path 与附加 query、
	// qrcode 与 h5 的 URL 都装得下。**凡有金额一律是字符串**（方案 13.1：JSON 传输里的
	// 金额不能用数字，JS 的数字精度接不住分）。
	PayParams map[string]string
	// FailureCode / FailureMessage 在 Result 不是 success 时说明原因，写进
	// payments.failure_code / failure_message 与事件体。**不进客户端可见的支付参数**。
	FailureCode    string
	FailureMessage string
	// ResponseSummary 是脱敏后的应答摘要，写 payment_provider_calls.response_summary。
	// 不放签名原文、密钥、完整卡号（方案 11.5），只放能定位这一笔的键。
	ResponseSummary map[string]any
}

// Pending 说明渠道是否已经接受了这笔支付、在等回调。
//
// 它不是 CreateResult 的字段而是从 Result 推出来的：Result==success 就意味着渠道收了单、
// 在等结果。单独一个 bool 字段会与 Result 打架（success 但 Pending=false 是什么？）。
func (r CreateResult) Pending() bool { return r.Result == ResultSuccess }

// NotificationRequest 是一次渠道回调的原始输入。
type NotificationRequest struct {
	// ChannelCode 是回调路径里的那段，日志与排查时用它定位。
	ChannelCode string
	// Body 是原始报文。**除 payment_notifications.body 外不落到任何地方**：方案 11.5
	// 禁止未脱敏的完整回调报文进日志，日志侧只允许记 sha256 与摘要。
	Body []byte
	// Headers 是回调请求头。适配器只该读签名与时间戳那几个，别整包留档。
	Headers http.Header
	// Method 是这条回调对应的渠道配置（适配器要读非敏感 config）。
	Method Method
	// Secret 是从 Method.SecretRef 解析出来的密钥。**空则必须拒绝验签**，见
	// ErrSecretNotConfigured。
	Secret string
}

// Notification 是验签通过、归一化之后的回调结论。
//
// 调用方只认这个结构，不认任何渠道特有的字段名：微信的 resource.id、银联的请求流水号、
// 丰选的通知序号在各自适配器里被翻译成这里的 NotificationID。**换渠道不改 service。**
type Notification struct {
	// NotificationID 是渠道侧的通知唯一号。它是防重放的全部依据——落库时
	// UNIQUE (provider, notification_id) 挡第二次。
	NotificationID string
	// EventType 是归一化后的事件类型。本轮只有 succeeded 与 failed 两种，
	// 其余取值调用方会记成 ignored 而不动支付状态（渠道会推各种与收款无关的通知）。
	EventType NotificationEvent
	// PaymentNo 是我们自己的支付单号，渠道原样带回来。为空则整条回调无法处理。
	PaymentNo string
	// Amount 是渠道说的成交金额，单位为分。调用方必须拿它与支付单的 amount 对一遍：
	// 对不上就拒绝并进 DLQ，而不是信它。
	Amount int64
	// ProviderTransactionID 是渠道侧交易号。与建单时记下的那个不一致时调用方会拒绝——
	// 这是防伪造回调的一条（签名之外的第二道）。
	ProviderTransactionID string
	// PaidAt 是渠道给的**成交时间**，不是我们收到回调的时间。它要写进 payments.paid_at
	// 与 payment_transactions.occurred_at：对账以渠道时间为准，我们晚收几小时不该让
	// 这笔钱看起来晚收了几小时。
	PaidAt time.Time
	// FailureCode / FailureMessage 在 EventType 是 failed 时说明原因。
	FailureCode    string
	FailureMessage string
}

// NotificationEvent 是归一化后的回调事件类型。
type NotificationEvent string

const (
	// EventSucceeded：钱到账了。
	EventSucceeded NotificationEvent = "succeeded"
	// EventFailed：这笔支付失败了。用户没被扣款，可以换一种方式重付。
	EventFailed NotificationEvent = "failed"
)

// Ack 是我们对一次回调的应答，形状由渠道约定。
//
// 为什么它属于适配器而不是控制器：**「怎么答」和「怎么验签」是同一种东西**——都是这家渠道
// 的私有协议。微信 v3 要 200 + {"code":"SUCCESS"}，支付宝要 200 + 裸文本 success，微信 v2 要
// 200 + {"return_code":"SUCCESS"}，而它们对「不认这条回调」的表达也各不相同（有的是非 200、
// 有的是 200 加一个失败码）。把这些写在控制器里，接第一家真实渠道就要改控制器——那正好是
// 这个抽象要消灭的事（见包注释：接渠道 = 写包 + 注册 + 插数据）。
type Ack struct {
	// Status 是 HTTP 状态码。**必须填**：零值不是一个合法的 WriteHeader 参数。
	Status int
	// ContentType 是应答体的媒体类型，空则用 text/plain。
	ContentType string
	// Body 是应答体原文。
	Body []byte
}

// Provider 是一家渠道的适配器。
//
// 接口只有三个方法，因为它们对应适配器要承担的**全部**职责：向渠道发起支付、认渠道的回调、
// 按渠道的约定应答那次回调。退款与查单将来也在这里长（operation 词表已经预留了
// refund / query），但本轮不写——没有真实渠道凭据时写它们只能验参数构造，那不是验证，是猜。
type Provider interface {
	// Name 是注册名，与 payment_channels.provider 的值对应。
	Name() string

	// Create 向渠道发起一次支付。
	//
	// 两个返回值分工明确，别把它们混成一个：
	//   - result 描述**这次尝试的结果**，无论成败调用方都要写进 payment_provider_calls。
	//   - err 只在「这次调用根本没发出去」时非 nil（适配器没配密钥、参数拼不出来）。
	//     渠道拒绝、超时、连不上渠道都算「发过了」，用 result.Result 区分。
	//
	// 规定：err 非 nil 时 result.Result 也要填（至少是 ResultUnknown）。适配器偷懒不填
	// 也不会漏掉留痕——调用方对零值有兜底。
	Create(ctx context.Context, req CreateRequest) (CreateResult, error)

	// Verify 验签并归一化一次回调，返回渠道无关的结论。
	//
	// **验签失败必须返回错误，绝不能返回一个「验证过了」的 Notification**。调用方拿到
	// 错误时会把这条回调记成 signature_verified=false / status='failed' 并且**一点都不碰
	// 支付状态**——这是「伪造回调不会让订单变成已支付」的全部依据。
	Verify(ctx context.Context, req NotificationRequest) (Notification, error)

	// Ack 把「我们收不收这条回调」翻成渠道约定的应答。
	//
	// accepted=false 覆盖的是一整类原因（验签不过、金额对不上、状态冲突、我们暂时处理不了），
	// 它对渠道只有一个含义：**这条通知还没被认下来**，按你的重投策略重投。不区分原因是故意的——
	// 把内部原因回给渠道既没用又是信息泄漏，真要查去看 payment_notifications.failure_reason。
	//
	// 将来某家渠道需要第三种答案（比如「收到了、别重投、但我们还在处理」）时，参数在这里长，
	// 而不是在控制器里对渠道码做 if。
	Ack(accepted bool) Ack
}

// Registry 按 payment_channels.provider 的值查适配器。
//
// 它没有并发保护：注册只发生在装配处（cmd/main.go），服务起来之后只读。
type Registry struct {
	providers map[string]Provider
}

// NewRegistry 构造注册表。名字为空、或两个适配器抢同一个名字时它是**用不了的**，
// 因为那意味着渠道行会指向一个不确定的实现——宁可在装配处就炸，也不要线上随机选一个。
func NewRegistry(providers ...Provider) *Registry {
	registry := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		name := strings.TrimSpace(p.Name())
		if name == "" {
			panic(fmt.Sprintf("provider %T has an empty name", p))
		}
		if _, exists := registry.providers[name]; exists {
			panic(fmt.Sprintf("provider %q is registered twice", name))
		}
		registry.providers[name] = p
	}
	return registry
}

// Lookup 按名字取适配器。找不到返回包装过的 ErrProviderNotConfigured，里面带上名字：
// 「哪个名字没注册」是排查时唯一有用的信息。
func (r *Registry) Lookup(name string) (Provider, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: registry is not configured", ErrProviderNotConfigured)
	}
	p, ok := r.providers[strings.TrimSpace(name)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotConfigured, name)
	}
	return p, nil
}
