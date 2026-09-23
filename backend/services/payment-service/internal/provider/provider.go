// Package provider 是渠道适配器：把「向一家渠道收一笔钱」抽象成一个接口。
//
// # 一个适配器 = 一套报文格式，不是一个商户号
//
// 注册进 Registry 的是一个**协议**：怎么签名、怎么拼字段、怎么认应答。银联商务的小程序
// 支付与 H5 支付是这家渠道下的两条协议，装在同一个 ums 适配器里，由 action 与下单路径分岔。
//
// 一个具体的商户号（账户值、密钥、地址）不是适配器的一部分——它是**配置**，由装配处
// （payment-service 的 internal/catalog）拼成 provider.Config 交给适配器。适配器因此不认识
// 「渠道代码」这种东西：`Method.ChannelCode` 只用来写日志。
//
// # 新增一条渠道要做什么
//
// 写一个实现 Provider 的包 + 在装配处的目录里加一行 + 在 .env 里加一组变量。**不改客户端、
// 不改表约束、不改 service**——service 只认 Action 与适配器接口，不认具体是哪一家。
//
// 这条边界靠类型而不是靠纪律维持：适配器看不到 *model.Channel 之类的行对象，只看到
// Config / Credentials 两个值类型，于是它没有能力去读一个它不该读的列。
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrProviderNotConfigured：目录里那条渠道指向一个没有注册适配器的 provider 名。
	//
	// 这是一次**明确的 5xx 而不是静默降级**：目录（internal/catalog）与 Registry 是同一批
	// 代码里的两处，对不上就是代码或装配的 bug。返回一个空支付参数会让用户看到一个「点了
	// 没反应」的支付，而真正的原因消失在日志里。
	ErrProviderNotConfigured = errors.New("payment channel provider is not registered")
	// ErrSecretNotConfigured：Channel.SecretEnv 里那个槽对应的环境变量读不到（或为空）。
	//
	// 验签路径上遇到它必须**拒绝**，不能放行：验不了签就等于没有防重放。而且它与
	// 「签名不对」是两件事——前者是运维漏配，后者可能是有人在伪造回调。
	ErrSecretNotConfigured = errors.New("payment channel secret is not configured")
	// ErrSignatureMismatch：回调验签不通过。
	ErrSignatureMismatch = errors.New("payment notification signature does not match")
	// ErrInvalidNotification：验签过了，但报文里缺必需的字段。
	ErrInvalidNotification = errors.New("payment notification is malformed")
	// ErrNotificationNotActionable：验签过了，报文也读得懂，但它说的那件事不该由这条入口处理。
	//
	// 与 ErrInvalidNotification 分开的东西只有一句话：那一个是「报文读不出一件事」，本一个是
	// 「读出来了，但那件事不是我们的」。两者对渠道的应答相同（都是拒），分开是为了让事后看
	// 那条失败记录的人分得清「有人在伪造」与「这条报文走错了门」——它们的排查方向完全不同。
	//
	// 它只可能由适配器**验完签之后**报出，所以服务层把它记进
	// payment_notifications.signature_verified 时记的是「验过了」（见 service.signatureVerified）。
	ErrNotificationNotActionable = errors.New("notification is not actionable on this path")
	// ErrOperationNotSupported：这一族实现了 Operator，但没有实现被请求的那个操作。
	//
	// 它与「这一族根本不支持对已有支付单的操作」是两件事，后者表现为类型断言失败
	// （见 Operator 的注释）。这一条是运行期才说得出来的那句「有这能力，只是这一家/这个
	// 场景没有」，所以它必须是一个**明确的错误**而不是一个静默的空结果。
	ErrOperationNotSupported = errors.New("provider does not support this operation")
)

// Action 是「这条支付方式被选中之后怎么起支付」。
//
// 客户端只 switch 它、不 switch 支付方式的 code——这是「新增一条同形态的支付方式不用改
// 客户端」的全部依据。加一个取值意味着要同步改客户端，所以它不该随手加。
//
// 今天只有三个取值是活的（native_pay / h5 / account，见 catalog 里那六条方式）。另外三个
// 留在词表里是因为它们描述的是**交互形态**，不是某一家的名字：跳对方小程序、对接方直接扣款、
// 扫二维码，这三种交互将来接渠道时还会用到，而它们的含义不会因为今天没有实现而改变。
// 适配器与客户端都按取值 switch，多一个没人用的取值不会让任何一条代码路径走偏。
type Action string

const (
	// ActionJumpMiniapp：跳对方小程序（老系统里丰选万联、优联、首创饭卡那类）。
	ActionJumpMiniapp Action = "jump_miniapp"
	// ActionNativePay：小程序内 requestPayment（微信、银联）。
	ActionNativePay Action = "native_pay"
	// ActionDirectPay：对接方直接扣款、不跳转（老系统里北方工业饭卡那类）。
	ActionDirectPay Action = "direct_pay"
	// ActionQrcode：扫普通二维码。
	ActionQrcode Action = "qrcode"
	// ActionH5：跳 H5 收银台。
	ActionH5 Action = "h5"
	// ActionAccount：走本仓库的账户服务扣余额（咖啡豆）。这条路上没有第三方，也没有渠道
	// （catalog.Method.ChannelCode 为空只对它合法），钱在我们自己的账户库里，扣成功就是成功。
	ActionAccount Action = "account"
)

// Valid 判断一个 action 取值是不是已知的六种之一。
//
// 它挡的是「代码里的 switch 漏了一个分支」：取值写死在 catalog 里，装配处不会给出别的，
// 但把校验放在这里，才能让「加了新 action 却忘了在服务里处理」在第一次调用时就报错而不是
// 走 default。它不查 catalog 里有没有人用它——**没有哪条方式用某个取值不是错误**。
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

// Method 是发起一次支付所需的渠道侧事实：目录里那一条支付方式 + 它落的渠道。
//
// 它是个**值**，不是 model.PaymentMethod：适配器不该看到 status、sort_order 这些与
// 「怎么起支付」无关的列，也不该有能力改数据。装配处（service）负责把 catalog 的那两项
// 拼成它，适配器只读。
type Method struct {
	// Code 是支付方式的 code（catalog 里那组常量），**不用来做分派**，只用来写日志与流水。
	Code string
	// Action 才是分派依据。
	Action Action
	// Params 是这条方式自己那几个旋钮（H5 的下单路径、微信的三项应用标识），**不放密钥**。
	Params map[string]string

	// —— 以下是渠道那一侧的事实，账户出资方式没有渠道，这几项全为零值 ——
	//
	// ChannelCode 是渠道代码，回调路径里的那段就是它。
	ChannelCode string
	// Provider 是适配器名字，Registry 按它查实现。
	Provider string
	// ChannelConfig 是装配处拼出来的那份配置树：地址、账户值、协议声明（怎么签、字段怎么
	// 映射、成功码是什么）。
	//
	// 它是**分层的**（见 provider.Config），并且**不再整份回给客户端**——早先的注释说它
	// 「返回客户端时与 Params 两层合并」，那是在 config 还只装商户号一类的年代写的。现在
	// config 里装着签名规则与渠道地址，整份发到浏览器等于把对接细节和收单地址一起送出去。
	// 客户端可见的那部分由适配器显式挑出来（见 ums 的 response.payParams）。与 Params 的
	// 两层合并仍在，但它发生在**适配器产出之后**，见 service.clientPayParams。
	ChannelConfig Config
}

// Credentials 是一次调用可用的凭据**明文**，键是槽名。
//
// # 为什么是「按名字取」而不是「一个字符串」
//
// 一个渠道不一定只有一把凭据。form_md5 那一族要一把签名密钥就够了，微信 v3 一条路上就要
// 三样（商户私钥签名、平台证书验签、apiV3Key 解回调），而且三条路各要两样——没有哪一样能
// 单独满足任何一条路。一个字符串装不下，装成一份 JSON 又等于把「这几样东西叫什么」这件事
// 从适配器挪到了运维的输入框里（见 wechatv3 包注释里那一段）。
//
// 键是**适配器自己声明的槽名**（见 SecretSlotter），装配处照着它逐个解析，所以适配器这边
// 读的就是它写配置时用的同一个名字。
//
// # 取不到就是空串
//
// Get 对「没有这个槽」与「槽里是空的」返回同一个东西：空串。调用方对这两者的处置也是同一个
// ——签不了名，一律拒绝（见 ErrSecretNotConfigured）。分成两个返回值只会让每个调用点多写
// 一个今天用不上的分支。
type Credentials map[string]string

// Get 取某个槽的凭据明文。
//
// 在这里 trim：值从环境变量来，那条路上可能带着尾随空白——`.env` 里一行 `KEY=abc ` 的
// 尾巴会进 HMAC 的密钥，而那个签出来的东西谁都验不过（包括我们自己）。归一成同一个形状，
// 调用方就不必记得「哪条路上要自己 trim」。
//
// 「没有这个槽」与「槽里是空的」返回同一个东西：空串。调用方对这两者的处置也是同一个
// ——签不了名，一律拒绝（见 ErrSecretNotConfigured）。分成两个返回值只会让每个调用点
// 多写一个今天用不上的分支。
func (c Credentials) Get(slot string) string {
	return strings.TrimSpace(c[slot])
}

// DivisionInstruction 是这次发起要随单下发给渠道的**分账指令**。
//
// 它是渠道能力，不是所有渠道都认：银联商务把它挂在**下单报文**里一次下发（老系统
// umspay_service.go:124-130 那三个键），微信那两条路今天都不带。
//
// **nil 与「空指令」是两件事**：没有子单的指令在银联那边是个非法请求（规范：divisionFlag=true
// 时 subOrders 不能为空，见 docs/unionpay-h5-pay.md:104-107），所以「这一笔不分账」只能表达成
// nil，不能表达成一个零值结构体。
type DivisionInstruction struct {
	// PlatformAmount 是平台自留的那一份（分）。它是**差额倒挤**来的：
	// PlatformAmount = 这笔支付的金额 − Σ SubOrders.Amount。规范那条硬约束
	// 「totalAmount = Σ subOrders.totalAmount + platformAmount」就是靠这个满足的。
	PlatformAmount int64
	// SubOrders 是分给各方的子单。非空是下发的前提。
	SubOrders []DivisionSubOrder
}

// DivisionSubOrder 是一条子单：钱分给哪个子商户号、分多少。
//
// 字段只有两个半，因为报文里也只有这些（键名 mid / totalAmount，见老系统
// umspay_service.go:35-39 的 DivisionOrder）。Mid 是**渠道那边的子商户号**
// （settlement_accounts.receiver_id），不是我们库里的账户 uuid——渠道不认后者。
type DivisionSubOrder struct {
	Mid    string
	Amount int64
	Remark string
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
	// ReturnURL 是**用户在渠道收银台上付完之后被送回**的地址，只有 H5 那条路用它。
	//
	// 它与 NotifyURL 是两件事，不能互相顶替：NotifyURL 是渠道服务器发给我们的**异步通知**
	// （成败的依据），ReturnURL 是把**浏览器**送回我们这里的一条跳转（只是给用户看的，
	// 见 service 的 HandleReturn）。同一个基址、不同的路径，同样由装配处拼。
	//
	// 为空表示这个渠道/这条支付方式不开结果页回跳：银联商务的 H5 会因此不带 returnUrl，
	// 用户付完就停在渠道自己的结果页上——功能上不缺什么，缺的是一次自动跳回。
	ReturnURL string
	// ExpiresAt 是这单的待支付超时。渠道侧的预支付单有效期应当不长于它，否则会出现
	// 「我们关了单、渠道那边还能付」。
	ExpiresAt time.Time
	// Method 是要用的支付方式及其渠道。
	Method Method
	// Secrets 是这次调用要用的凭据**明文**，键是槽名（见 Credentials）。渠道不需要签名时
	// 为空表。
	//
	// 它由装配处按适配器自己声明的槽逐个解析（密文槽 → secret_ref 环境变量 → 空串，见
	// SecretSlotter）。取不到在适配器这边只有一个含义：**这一把签不了名，一律拒绝**——
	// 绝不能有「解不出密钥就跳过签名」这种降级。
	//
	// 它只活在这一层与调用栈上：不入库、不进摘要、不进日志、不进错误串。
	Secrets Credentials
	// Division 是这次发起要**随单下发**的分账指令，nil 表示这一笔不分账。
	//
	// 它随下单一起走，不是一次单独的调用：银联商务的下单报文里有分账三键
	// （divisionFlag / platformAmount / subOrders），钱在支付成功那一刻就已经分出去了，没有
	// 「再发起一次分账」这一步。这与微信的四步（发起动账 → 查 → 完结 → 回退）不是一回事——
	// payment_provider_calls.operation 上那四个分账值说的是后者，今天没有一条路在写。
	//
	// 适配器不许改它：金额是调用方算好的（见 service/settlement.go 的 computeSettlement），
	// 动一个数字就破坏了「Σ子单 + 平台 = 实付」这条恒等式，渠道会因此拒掉整笔支付。
	Division *DivisionInstruction
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

	// HTTPStatus 是渠道应答的 HTTP 状态码，0 表示**没拿到应答**（超时、连接失败、
	// 报文没构造出来）。零值落库是 NULL，页面上显示 `—`，与「渠道回了 200」分得开。
	//
	// 它单独一个字段而不是塞进 ResponseSummary，是因为 payment_provider_calls 有这一列，
	// 而排查「对面到底回了什么」时它是第一个要看的：200 加一个失败码，与 502，是完全
	// 不同的两件事。
	HTTPStatus int
	// Attempts 是这次调用**实际发出去的次数**，0 当 1。
	//
	// 它只在「连接根本没建立」时才会大于 1（见 httpx 的重试策略）：重试一次是常见的，
	// 而一笔走了两次的调用在排查延迟问题时必须看得出来，否则「对面慢」与「我们重试了」
	// 在流水里长得一模一样。
	Attempts int
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
	// HTTPMethod / RequestPath 是这条回调**被投递到的方法与路径**。
	//
	// 它们在这里的理由只有一个：有的协议族把两者签进了待签串。hmac_body（家园消费金）就是
	// ——签名原文是 `UPPER(method)\npath\n…`，而 path 是回调被投递到的那个路径
	// （老系统传的是 `c.Request.URL.Path`）。**没有它们，这类渠道的回调一条都验不了签。**
	//
	// 空值必须被当成「验不了」而不是「不用验」：适配器算不出签名时只有拒绝这一条路
	// （见 hmacbody 的 Verify）。
	//
	// Path 要的是**服务收到的那个路径**，不是渠道配置里的回调基址：中间可能有网关，
	// 而渠道签的是它实际 POST 的那个地址。
	HTTPMethod  string
	RequestPath string
	// Query 是这次入站请求**查询串里**的参数，只有把参数放在查询串里的协议族才用它。
	//
	// 今天只有一家：银联商务的结果页回跳（H5 支付完成后用户被送回 returnUrl，渠道把支付结果
	// 拼在查询串上）。GET 没有报文体，所以那条路上 Body 是空的——把查询串塞进 Body 也能跑通，
	// 但那会让「Body 是什么」变成一个按方法而异的事实，而验签对不上时错误串指着的会是一个
	// 根本没有体的报文。语义写死在字段上，读的人不必去猜这条请求的体该是什么。
	//
	// 多数协议族上它是空的（微信 v3 的回调是 POST + JSON，参数全在体里）。**回调路径不需要
	// 填它**：那条路的参数一直是从 Body 里读的，两个来源混起来只会让「签名盖的是哪些字符」
	// 变得说不清。
	Query url.Values
	// Method 是这条回调对应的渠道配置（适配器要读非敏感 config）。
	//
	// 注意回调路径上 Method.Params 是空的：我们不知道用户当时选的是哪条支付方式（那要读
	// payments.payment_method_id，而报文还没验签）。协议族读的必须是 config 里的东西。
	Method Method
	// Secrets 是这次验签要用的凭据**明文**，键是槽名。**取不到就必须拒绝验签**，见
	// ErrSecretNotConfigured。
	//
	// 它由装配处解析，槽名来自适配器的 SecretSlotter（回调路径同样问适配器：验签的凭据
	// 与签名的凭据是同一批，槽名就该从同一处来）。
	Secrets Credentials
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
//
// **它收 method**（与 Verify 收 NotificationRequest 的理由相同）：应答体是渠道的协议声明，
// 而声明住在渠道的 config 里。form_md5 那一族的 `notify.ack` 就是它——协议族里没有统一的
// 「成功怎么答」（有的收 {"code":0} 数字，有的收 {"code":"0"} 字符串），不收 method 的话
// 这个配置项就成了一个**配了不生效**的旋钮，那比没有它更糟。
//
// method 里 Params 是空的（回调路径上不知道用户选的是哪条支付方式，见 NotificationRequest
// 的注释），所以应答体只能由 config 决定——这正好也是对的：应答形状是渠道级的，与支付方式无关。
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
// 按渠道的约定应答那次回调。查单与「对一张已有支付单的操作（退款、关单）」**不在这里**，
// 它们是可选接口（见 Querier 与 Operator）——往这个接口上加方法会逼每一个适配器实现它，
// 不支持的只能返回一个「不支持」的桩，于是「这一族不支持」从编译期事实降级成运行期日志。
type Provider interface {
	// Name 是注册名，与 catalog.Channel.Provider 的值对应。
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
	Ack(method Method, accepted bool) Ack
}

// ============================================================
// 可选接口
// ============================================================
//
// 下面这些接口**不在 Provider 上**，是调用方用类型断言探的。这不是为了少写代码，恰恰相反：
// 往 Provider 上加第四个方法会逼每一个适配器实现它，不支持的只能返回一个「不支持」的桩——
// 于是「这个协议族不支持查单」从**编译期事实**降级成**运行期日志**，而桩函数是没有测试的。
//
// 探不到时的默认行为必须都是安全的：不查单 → 维持「结果不明」的既有处置；不能对已有支付单
// 做操作 → 维持现状、等人工；没有额外凭据槽 → 用渠道行的 secret_ref；不留请求头 → 空 map。
// 这几条默认都不会让任何一笔钱变得可疑。

// Querier 是适配器**可以**额外实现的一个接口：拿商户单号去渠道查这一笔到底成没成。
//
// 它是收真钱这件事的必需品，而不是锦上添花：出网调用超时之后，我们手上只有两个都不对的
// 选项——标失败（可能钱已经收了）、或者等超时关单（用户付过的那笔会被关掉）。正确答案是
// 去问渠道，这就是它存在的理由。见 service 里 ErrProviderResultUncertain 的注释。
//
// 实现它的协议族不多：微信 APIv3、银联商务有查单接口，四家饭卡渠道里有几家只有回调。
// 那几家不实现它，超时路径保持今天的行为（停在 created、等超时关单收走）。
type Querier interface {
	// Query 按我们自己的支付单号问渠道这笔的状态。
	//
	// 返回的 CreateResult 与 Create 同形，因为调用方对两者的处置完全一样（成功就推进
	// pending、明确失败就落 failed、还是不明就继续等）。FailureCode 里要能看出这是**查单
	// 得到的结论**，而不是一次新的发起。
	Query(ctx context.Context, req QueryRequest) (CreateResult, error)
}

// QueryRequest 是一次查单的输入。字段是 Create 的子集：查单不需要金额与商品描述，
// 渠道按商户单号认这一笔。
type QueryRequest struct {
	PaymentNo string
	OrderNo   string
	RequestID string
	Method    Method
	// Secrets 与 CreateRequest.Secrets 同一份东西（同一次调用解出来的），查单复用发起时
	// 那一批槽，不重新解析一遍。
	Secrets Credentials
}

// Operation 是对**一张已经存在的支付单**做的操作，取值与 payment_provider_calls.operation
// 的 CHECK 逐字一致（那是有意的：每一次这样的操作都要在调用流水里留一行，词表分成两套
// 就得在写入处做一次翻译，而翻译漏掉一个值是一次运行期的 CHECK 违约）。
//
// 只列**今天真有路可走**的三个。担保撤销 / 担保完成 / 异步分账确认不在词表里，因为
// payment_provider_calls 的 operation CHECK 里也没有它们——那不是「这一族不支持」，
// 是「这两件事我们整个系统都还没有」。真要做时先改迁移，再在这里加值。
type Operation string

const (
	// OperationRefund：向渠道发起一笔退款。
	OperationRefund Operation = "refund"
	// OperationQueryRefund：查一笔退款在渠道那边的状态。退款应答里的
	// PROCESSING / UNKNOWN 是不确定的，只有它能给出结论。
	OperationQueryRefund Operation = "query_refund"
	// OperationClose：关掉渠道侧那张还没付的预支付单。
	OperationClose Operation = "close"
)

// Operator 是适配器**可以**额外实现的一个接口：对一张已有的支付单发起一次渠道侧操作。
//
// # 为什么是一个接口而不是三个
//
// 退款、退款查询、关单的**输入与输出是同一个形状**（对哪一笔、多少钱、我们自己的操作单号
// → 渠道认没认、渠道那边的单号是多少），调用方对它们的处置也逐字相同（记一行
// payment_provider_calls、把渠道单号写回、结果不明就维持原状）。拆成三个接口就是三份
// 一模一样的结果体加三次类型断言，而它们永远不会独立演化。
//
// 这与 Querier 是同一种取舍：**探不到时调用方的默认行为必须是安全的**。探不到 Operator
// = 这一族没有这个能力，调用方维持现状；探得到但收到一个它没实现的操作 = 明确的
// ErrOperationNotSupported，那是配置问题，不该被当成「操作失败了」。
//
// # 它与上游聚合的关系
//
// 这个接口只负责「把一次操作发给渠道、把渠道的答复翻成结论」。**谁在什么条件下决定要退、
// 退多少、退完之后通知谁**，全部属于上游（退款单聚合与订单侧的售后），不在这里——见
// internal/service/state.go 里「退款是 payment_refunds 表上的独立聚合」那一段。
type Operator interface {
	// Execute 执行一次操作。
	//
	// 返回值分工与 Provider.Create 逐字相同：result 描述**这次尝试**（必须填 Result），
	// err 只在「这次调用根本没发出去」时非 nil。渠道拒绝是 result 里的一个取值，不是 err。
	Execute(ctx context.Context, req OperationRequest) (OperationResult, error)
}

// OperationRequest 是一次操作的输入。
type OperationRequest struct {
	// Operation 是这次要做的事。
	Operation Operation
	// PaymentNo 是我们自己的支付单号，渠道侧的商户订单号就是它——退款与关单都按它认这一笔。
	PaymentNo string
	// OrderNo 只用于渠道账单上的展示与对账关联。
	OrderNo string
	// RefundNo 是我们自己的**退款单号**，只有 refund 与 query_refund 用它。
	//
	// 退款这条路上它有一个硬约束：**同一笔支付的多次退款，每次必须给一个不同的值**。
	// 重复送同一对 (PaymentNo, RefundNo) 时渠道会把上一次那张退货单原样回给我们，而不是
	// 再退一次——那是幂等，不是错误，但把「退第二次」误当成「已经退过了」就会少退钱。
	RefundNo string
	// Amount 是这次要退的金额，单位为**分**，只有 refund 用它。
	Amount int64
	// Reason 是退款原因，进渠道的账单备注。
	Reason string
	// RequestID 是调用方给的幂等号，透传给渠道的商户请求号，方便对账时对时间线。
	RequestID string
	// Method 是要用的支付方式及其渠道。
	Method Method
	// Secrets 与 CreateRequest.Secrets 同一份东西（同一次调用解出来的）。**取不到就拒绝**。
	Secrets Credentials
}

// OperationResult 是渠道对一次操作的答复。
//
// 字段与 CreateResult 刻意保持同形（Result / FailureCode / ResponseSummary / HTTPStatus /
// Attempts 的含义逐字相同），只少了 PayParams——一次退款或关单没有东西要给客户端渲染。
type OperationResult struct {
	// Result 是这次调用的分类，直接写进 payment_provider_calls.result。**适配器必须填它**，
	// 零值会被调用方记成 ResultUnknown（见 CreateResult.Result）。
	Result Result
	// ProviderRefundID 是渠道侧的退款单号（只有退款那条路有）。它写回
	// payment_refunds.provider_refund_id，是「这笔退款在渠道那边叫什么」的唯一凭据。
	ProviderRefundID string
	// FailureCode / FailureMessage 在 Result 不是 success 时说明原因。**不进客户端**。
	FailureCode    string
	FailureMessage string
	// ResponseSummary 是脱敏后的应答摘要，写 payment_provider_calls.response_summary。
	ResponseSummary map[string]any
	// HTTPStatus 是渠道应答的 HTTP 状态码，0 表示没拿到应答。
	HTTPStatus int
	// Attempts 是这次调用实际发出去的次数，0 当 1。
	Attempts int
}

// AgreementState 是**渠道无关**的协议状态：拿它去问渠道，渠道的答复翻成这三个之一。
//
// 只有三个，因为上游能做的判断只有三个：**能扣款**、**不能扣款了**、**还没定**。渠道那边的
// 状态机比这细（微信就还有「已解约但商户侧记录仍在」这类中间态），但那些区别没有一个能让
// membership-service 做出不同的动作——它要么续、要么不续、要么再等等。
type AgreementState string

const (
	// AgreementSigned：协议有效，可以扣款。
	AgreementSigned AgreementState = "signed"
	// AgreementTerminated：协议无效——**它同时是「用户从没签过」与「用户已经解约」**。
	//
	// 这两件事在渠道那边是同一个状态（微信 querycontract 对两者回同一组错误码），我们也
	// 不去猜：把「没签过」当成「签过又解了」的后果是给用户推一条「你的自动续费已关闭」，
	// 而他从没开过。所以这个状态只驱动「不能扣款、本地状态该收了」。
	AgreementTerminated AgreementState = "terminated"
	// AgreementPending：签约进行中，渠道那边已经有这一份协议但还没生效。
	AgreementPending AgreementState = "pending"
)

// AgreementKeeper 是适配器**可以**额外实现的一个接口：委托代扣的**签约、查约、解约、代扣**。
//
// # 为什么在 provider 这里而不在支付聚合里
//
// 协议是**渠道侧的一份授权**，不是我们库里的一条记录：它由渠道签发、由渠道的状态机决定
// 生死、由渠道在扣款时校验。所以「怎么签、怎么问、怎么解」这件事**只有适配器知道**，
// 而聚合（payment_agreements 那张表）知道的是「我们记了这一份协议、它现在是什么状态」。
// 这个接口就是两者之间的那条缝，形状与 Querier / Operator 完全一样。
//
// # 探不到时调用方的默认行为
//
// 与 Querier / Operator 同一条规矩：**探不到（这一族不支持协议）时调用方必须什么也不做**
// ——不建协议记录、不发起签约，把「这个渠道不支持自动续费」如实报给上游。签约这条路上
// 「猜一个默认」的代价比别处都高：它会让用户以为自己在微信里授权了，而其实什么都没有。
type AgreementKeeper interface {
	// Sign 算出一份签约的跳转参数。**它不发任何请求**——微信的纯签约是客户端跳进微信官方
	// 小程序完成的，服务端只负责算签名与参数（见 wechatpay 包注释）。
	//
	// 返回的 AgreementSignResult.PayParams 是给客户端的：客户端拿它去调起签约流程，
	// 签完之后渠道把通知推到 NotifyURL，再走「查约确认」那条路。
	Sign(ctx context.Context, req AgreementSignRequest) (AgreementSignResult, error)
	// QueryAgreement 回渠道问一份协议的状态。它是**幂等的**，也是这条链路上唯一能用来
	// 「纠正本地状态」的手段（后台那个「同步」按钮走的就是它）。
	QueryAgreement(ctx context.Context, req AgreementQuery) (AgreementQueryResult, error)
	// TerminateAgreement 解一份协议。
	TerminateAgreement(ctx context.Context, req AgreementTerminate) (AgreementCallResult, error)
	// Charge 对一份已签约的协议发起一次扣款。**受理不等于扣到钱**：扣款是异步的，
	// 真正的结果在渠道推回来的通知里。
	Charge(ctx context.Context, req AgreementChargeRequest) (AgreementChargeResult, error)
}

// AgreementSignRequest 是一次「发起签约」的输入。
//
// 它比别的请求多一个字段：**WalletOpenID**。签约是用户与渠道之间的事，渠道要知道「签的是谁」，
// 而那个标识是**用户在某一个渠道里的身份**（微信的 openid），不是我们的 user_id——两者的
// 关系由上游（member / user 域）维护，这里只接收、不推导。user_id 也带上，但它是给我们自己
// 的流水与事件用的（见 payment_provider_calls.agreement_no 旁边那一列）。
type AgreementSignRequest struct {
	// AgreementNo 是我们自己的协议号（payment_agreements.agreement_no），也是这条链路上
	// 唯一的关联键。它由**调用方**生成并先落库，因为渠道的通知只认它自己那套标识
	// （contract_code / contract_id），我们得有一个自己的编号能把两边对上。
	AgreementNo string
	// ContractCode 是渠道侧的签约协议号，由我们生成、渠道只负责确认。用户换一份协议就换一个，
	// 所以它必须**每次发起签约都不相同**（同一个 contract_code 重复签约在微信那边是同一份协议）。
	ContractCode string
	// PlanID 是渠道侧的签约模板 id。会员套餐上是哪个月扣多少的模板由渠道定义，我们只引用
	// 它的编号，不把模板内容抄进我们库。
	PlanID string
	// UserID 是付款用户，值引用（同 CreateRequest.UserID）。
	UserID string
	// WalletOpenID 是用户在这个渠道里的身份（微信 openid），渠道要拿它认人。
	WalletOpenID string
	// NotifyURL 是渠道签约成功后推通知的地址，由调用方按渠道拼（**不能写死**：它是部署事实，
	// 见 service 里的 notifyBaseURL）。
	NotifyURL string
	// Method 是渠道与支付方式。签约这条路上它主要是**渠道的载体**：config 与 secret_ref 都
	// 挂在渠道上，方法本身只决定目录里那条记录长什么样。
	Method Method
	// Secrets 与 CreateRequest.Secrets 同一份东西，**取不到就拒绝**。
	Secrets Credentials
}

// AgreementSignResult 是一次发起签约的结果。
//
// 字段与 CreateResult 刻意保持同形的那几个含义逐字相同，只少了 HTTPStatus / Attempts
// ——**纯签约没有出网调用**，那两个数字在这里没有来源（写 0 会让流水里出现一行「0 次尝试、
// HTTP 0」的记录，看上去像一次失败的请求，而它根本不是请求）。
type AgreementSignResult struct {
	// ContractCode 原样回带，方便调用方在一处拿到它。
	ContractCode string
	// PayParams 是给客户端的跳转参数（扁平字符串键值，含签名）。客户端按它调起签约流程。
	//
	// 它**含签名**，所以它进响应、进日志都要当敏感值对待：与 CreateResult.PayParams 同一条
	// 规矩——回给付款的那个客户端是一次性凭据，不进任何流水。
	PayParams map[string]string
	// FailureCode / FailureMessage 在 err 非 nil 时说明原因。**不进客户端**。
	FailureCode    string
	FailureMessage string
	// ResponseSummary 是脱敏后的摘要，写 payment_provider_calls.response_summary。
	// **不含签名原文，也不含密钥**。
	ResponseSummary map[string]any
}

// AgreementQuery 是一次查约的输入。
//
// PlanID 与 ContractCode 是渠道认这一份协议的两个凭据（微信按 (plan_id, contract_code) 查），
// 两个都要带：只带 contract_code 在微信那边是查违约定的那一份。
type AgreementQuery struct {
	AgreementNo  string
	PlanID       string
	ContractCode string
	Method       Method
	Secrets      Credentials
}

// AgreementQueryResult 是渠道对一份协议的状态判断。
//
// 字段与 OperationResult 同形（Result / FailureCode / ResponseSummary / HTTPStatus /
// Attempts 的含义逐字相同），另加渠道侧的协议标识。
type AgreementQueryResult struct {
	// State 是渠道无关的结论。**只有 Result 是 success 时它才有意义**——结果不明时它是空串，
	// 调用方绝不能把空串当成「没签约」：那会把一份有效的协议在本地判死。
	State AgreementState
	// ProviderContractID 是渠道侧的协议号（微信的 contract_id），扣款与解约都按它认这一份
	// 协议。它写回 payment_agreements.contract_no。
	ProviderContractID string
	// TerminationMode 是渠道说的解约方式（用户主动解约 / 商户解约），只在 State 是
	// terminated 时可能非空。它进流水，用来回答「这份协议是怎么没的」。
	TerminationMode string
	// Result 是这次调用的分类。**适配器必须填它**，零值会被调用方记成 ResultUnknown。
	Result          Result
	FailureCode     string
	FailureMessage  string
	ResponseSummary map[string]any
	HTTPStatus      int
	Attempts        int
}

// AgreementNotifier 是适配器**可以**额外实现的一个接口：认一份**协议变更通知**。
//
// # 为什么不复用 Provider.Verify
//
// 两条理由，任一条都够：
//
//   - **形状不对**。provider.Notification 要求一个 payment_no，而签约通知说的是协议不是支付单
//     ——见 wechatpay.Provider.Verify 的注释。为了塞进那个形状而返回一个空的 payment_no，
//     会让落库那一行显示成「这条通知指向空支付单」，而那与「这条通知不指向支付单」是两件事。
//   - **判据不同**。支付回调要判金额、判渠道、判交易号（钱的事，宁可拒也不猜）；协议通知判的
//     是「哪个协议变成什么状态」，一个金额字段都没有。
//
// 与 Querier / Operator / AgreementKeeper 同一种取舍：**探不到时调用方的默认行为必须是安全
// 的**——探不到就是「这条渠道不会给我们推协议通知」，入口直接拒（回失败应答），而不是把一份
// 报文当成支付回调去处理。
type AgreementNotifier interface {
	// VerifyAgreementNotification 验签并读懂一份协议变更通知。
	//
	// 它与 AgreementKeeper.QueryAgreement 是**同一件事的两个入口**：渠道路过来说的（这条）与
	// 我们主动去问的（那条）。两条最后落回同一个状态机（见 repository.applyAgreementTarget），
	// 所以它们对「这是什么状态」的判定必须一致——不一致就是「同一个渠道状态在两条路上得到
	// 不同结论」。
	VerifyAgreementNotification(ctx context.Context, req NotificationRequest) (AgreementNotification, error)
}

// AgreementChargeNotifier 是适配器**可以**额外实现的一个接口：认一份**扣款结果通知**。
//
// # 为什么与 AgreementNotifier 是两个接口
//
// 它们收的报文说的是两件事：那一份说「协议现在是什么状态」，这一份说「这一期扣到了没有」。
// 今天实现它们的恰好是同一族（微信），而**必须能分开探**——一家只推协议不推扣款结果的渠道
// （或者反过来）是完全正常的形态，合成一个接口会逼着那种渠道实现一个它根本不认的方法。
//
// 与 AgreementNotifier 同一条规矩：探不到就是「这条渠道不给我们推扣款结果」，入口直接拒
// （回失败应答），绝不把一份报文当成别的东西去处理。
type AgreementChargeNotifier interface {
	// VerifyAgreementChargeNotification 验签并读懂一份扣款结果通知。
	//
	// 它与 AgreementKeeper.Charge 是**同一件事的两个入口**：这一次是我们主动发起的（那条），
	// 这一次是渠道事后告诉我们结果的（这条）。两条最后落回同一行（见
	// repository.applyChargeTarget），所以它们对「这一期现在怎样」的判定必须一致。
	VerifyAgreementChargeNotification(ctx context.Context, req NotificationRequest) (AgreementChargeNotification, error)
}

// AgreementChargeNotification 是一份读懂了的扣款结果通知。
//
// 与 AgreementNotification 一样**没有**通知号：微信的扣款通知里没有这样一个字段，所以那一行
// 用报文自身的摘要做键（见 service.chargeNotificationID）。
//
// # 它没有「协议号」
//
// 报里有 contract_id，但那不是本地那份协议的号——真正的关联键是 OutTradeNo（我们自己发给
// 渠道的商户单号）。这条链路上一期扣款只有这一个键能对上（见 payment_agreement_charges.out_trade_no 的列注释）：微信报里
// 没有协议号、也没有期次。所以这个结构**刻意不提供 AgreementNo 字段**，免得调用方拿一个
// 找不到东西的键去查库，再得到一句语焉不详的「查无此约」。
type AgreementChargeNotification struct {
	// OutTradeNo 是报文里的商户单号，也就是 payment_agreement_charges.out_trade_no。
	OutTradeNo string
	// ProviderTransactionID 是渠道侧的流水号（微信的 transaction_id）。**成功时才有**
	// ——渠道拒一笔时不给交易号。
	ProviderTransactionID string
	// ProviderContractID 是渠道侧的协议号（微信的 contract_id）。它只进流水与排查：定位这一期
	// 靠的是 OutTradeNo（见上面）。
	ProviderContractID string
	// Amount 是报文里报的金额，单位**分**。调用方**必须**拿它与本地那一期的金额对一遍
	// （见 repository.ErrChargeAmountMismatch）：金额对不上的两条报文说的不是同一笔。
	Amount int64
	// Result 是这条通知给出的结论：ResultSuccess（扣到了）或 ResultFailed（没扣到）。
	//
	// **没有 ResultUnknown**：通知本身就是结论。一条读不懂的报文在 Parse 那一层就被拒了
	// （ErrInvalidNotification），不会走到这里变成一个「不知道」的结果。
	Result Result
	// FailureCode / FailureMessage 只在 ResultFailed 时有值（微信的 err_code / err_code_des）。
	FailureCode    string
	FailureMessage string
	// ResponseSummary 是脱敏后的摘要（白名单，见 wechatpay.responseSummary）。
	ResponseSummary map[string]any
}

// AgreementNotification 是一份读懂了的协议变更通知。
//
// 它**没有**通知号：微信的签约通知里没有这样一个字段，所以 payment_notifications 那一行用
// 报文自身的摘要做键（见 service.agreementNotificationID）——同一个报文重推就是同一件事，
// 这正是防重放要的语义。
type AgreementNotification struct {
	// AgreementNo 是报文里的 contract_code，也就是**我们自己的协议号**（见
	// AgreementSignRequest.ContractCode 的「一个协议号，两个身份」）。
	//
	// **它可能是空的**：微信的解约通知（change_type=DELETE）不带 contract_code，只带
	// contract_id。所以调用方必须能按 ContractNo 找协议，不能假定这个字段有值
	// （见 repository.FindAgreementForNotification）。
	AgreementNo string
	// ContractNo 是渠道侧的协议号（微信的 contract_id）。签约通知与解约通知都带着它。
	ContractNo string
	// State 是这条通知在说的协议状态，只可能是 AgreementSigned 或 AgreementTerminated。
	//
	// 「渠道用**变更**表达、我们用**状态**记账」这件事在这里收口：ADD 就是「现在它是有效的」，
	// DELETE 就是「现在它不再有效」。翻成状态之后，通知与查约两条路落回同一个函数
	// （repository.applyAgreementTarget），而不是各判一次 change_type。
	State AgreementState
	// ProviderState 是渠道报文里的原始状态词（微信的 contract_state），只进流水与排查：
	// 前两个字段是我们的判断，这一个才是渠道的原话。**多半是空的**——协议变更通知不保证
	// 带这个字段，而「报文没写」与「写的是某个值」是两件事，所以不猜。
	ProviderState string
	// ResponseSummary 是脱敏后的摘要，写 payment_provider_calls.response_summary 与
	// payment_notifications 的旁证。**不含签名原文与密钥**。
	ResponseSummary map[string]any
}

// AgreementTerminate 是一次解约的输入。
//
// 按**渠道侧的协议号**解约而不是按我们自己的协议号：解约这个动作在渠道那边认的是它签发的
// 那份凭证。我们自己的 agreement_no 只用于流水（payment_provider_calls.agreement_no）。
type AgreementTerminate struct {
	AgreementNo string
	// ProviderContractID 是渠道侧的协议号（微信的 contract_id）。
	ProviderContractID string
	// Reason 进渠道的备注，是解约这件事在渠道账单上留下的唯一一句人话。
	Reason  string
	Method  Method
	Secrets Credentials
}

// AgreementCallResult 是「解约」这类**没有返回体**的调用的结果。
//
// 与 OperationResult 同形但不带渠道单号：解约成功之后渠道那边什么也没留下，能说的只有
// 「渠道认了」。
type AgreementCallResult struct {
	Result          Result
	FailureCode     string
	FailureMessage  string
	ResponseSummary map[string]any
	HTTPStatus      int
	Attempts        int
}

// AgreementChargeRequest 是一次代扣的输入。
//
// # 为什么它没有一个 OrderNo
//
// 支付聚合那边的每一笔出资都要挂一张订单（payments.order_no 是 NOT NULL，注释写着是
// orders.order_no），而代扣**没有订单**：它是「会员还有 3 天到期、该续一期」这个判断的结果，
// 不来自任何一次下单。所以这条路上我们自己的单号是 OutTradeNo——它是**渠道侧的商户订单号**，
// 而它同时是我们记账的凭据（payment_agreement_charges.biz_period 是那个判断的幂等键）。
type AgreementChargeRequest struct {
	// AgreementNo 是我们自己的协议号，用于流水。
	AgreementNo string
	// ProviderContractID 是渠道侧的协议号。
	ProviderContractID string
	// OutTradeNo 是这一笔扣款在渠道那边的商户订单号，由调用方生成。
	//
	// 它必须**每一期都不相同**：重复送同一个号时渠道会把这当成同一笔（幂等），那既是保护
	// （重投不会扣两次）也是陷阱（「这一期没扣到、下期接着扣」如果复用了号，就永远扣不到）。
	OutTradeNo string
	// Subject 是账单上的一句话（微信的 body）。
	Subject string
	// Amount 是扣款金额，单位**分**。
	Amount int64
	// NotifyURL 是扣款结果推回来的地址。与签约的 notify_url 不同：那条推的是协议变更，
	// 这条推的是钱。
	NotifyURL string
	Method    Method
	Secrets   Credentials
}

// AgreementChargeResult 是渠道对一次代扣请求的答复。
type AgreementChargeResult struct {
	// ProviderTransactionID 是渠道侧的这笔扣款的流水号（微信的 transaction_id），
	// 回调回来时按它对账。**扣款受理时它就有值**，而这笔钱到没到还要等通知。
	ProviderTransactionID string
	Result                Result
	FailureCode           string
	FailureMessage        string
	ResponseSummary       map[string]any
	HTTPStatus            int
	Attempts              int
}

// SecretSlotter 是适配器**可以**额外实现的一个接口：声明自己的凭据住在哪几个槽里。
//
// 为什么由适配器说而不是 service 去 config 里找：槽名写在协议声明里（form_md5 是
// `sign.secretRef`），而「协议声明长什么样」正是适配器独占的知识。让 service 去读
// `config["sign"]["secretRef"]`，等于把 form_md5 的配置格式抄进了 service——下一个协议族
// 进来时，那行代码就成了「有的协议族读得到、有的读不到」的分支。
//
// **它报的是若干个槽，不是一个**：一个渠道可以有多把凭据，而「几把」是协议族自己的事实
// ——form_md5 一把签名密钥就够，微信 v3 要三样（商户私钥、平台证书、apiV3Key），而且三条
// 路各要两样。装配处照着这张名单逐个解析，适配器随后按同一个名字取回来（见 Credentials）。
type SecretSlotter interface {
	// SecretSlots 返回这次调用要用的槽名。没声明槽时返回空切片。
	//
	// 空切片表示「这个渠道一把密钥，名字由渠道行的 secret_ref 指」——这正是没有实现本接口
	// 的适配器（手工渠道）的默认行为。装配处对它的处置是解析**那一把兜底凭据**，放在
	// Credentials 的 ChannelSecret 那一格。
	//
	// 名字里没有「本族要几把」这一层：返回一个还是三个由协议族说了算，调用方只照着解析。
	//
	// 它按 method 算而不是无参：将来同一协议族的不同支付方式用不同槽时（微信小程序与
	// 微信 H5 共用一把私钥，但服务商模式的子商户号不同），差别在 method 上，不在类型上。
	SecretSlots(method Method) []string
}

// SecretSlotsFor 问适配器它的凭据槽名，没实现本接口时返回空切片。
//
// 空切片是一个**有意义的取值**，不是「不知道」：它表示「这个渠道一把密钥，名字由渠道行的
// secret_ref 指」。装配处的解析顺序对它逐字适用（密文槽 → 环境变量 → 空串）。
//
// 逐项 trim 并去掉空串：一个解析不出配置的适配器会把槽名报成空串（见各族的 SecretSlots），
// 而空槽名对装配处是一个**有含义**的取值——「用 secret_ref 兜底」。那个兜底只该发生在
// 「适配器一把都没声明」时，不该发生在「它声明了三把、其中一把没解析出来」时。
func SecretSlotsFor(adapter Provider, method Method) []string {
	slotter, ok := adapter.(SecretSlotter)
	if !ok {
		return nil
	}
	// 只问一次：这个接口的返回值是「这一族这一次要用哪几把」，问两遍等于让容量提示与迭代
	// 各由一次调用决定。今天的四个实现都是纯函数，但契约上没这么保证。
	declared := slotter.SecretSlots(method)
	slots := make([]string, 0, len(declared))
	for _, slot := range declared {
		if name := strings.TrimSpace(slot); name != "" {
			slots = append(slots, name)
		}
	}
	return slots
}

// HeaderRecorder 是适配器**可以**额外实现的一个接口：声明回调的哪些请求头值得留档。
//
// 从「排查方便」升级成必需的是微信 APIv3：它的验签与防重放**完全依赖请求头**
// （Wechatpay-Signature / -Timestamp / -Nonce / -Serial），不留下它们，一条被拒的回调
// 事后完全无法复盘。银联商务（ums）**不在此列**：它那一段签名材料（AppId / Timestamp /
// Nonce / Signature）整份装在同一个 Authorization 头里，摘不出单独的时间戳与随机串，而
// 「凑出一段签名材料」本身就等于留下了一份有效签名的凭证——所以那一族的名单是空的。
//
// 名单由适配器给而不是 service 拉一张全渠道的白名单：哪些头是这家渠道的协议凭证，
// 只有它自己知道；而「留了一切」比「一个都不留」更糟——那一列会装进签名原文本身。
type HeaderRecorder interface {
	// RecordableHeaders 返回要保留的头名（大小写不敏感）。**签名头不在其中**：哪怕是
	// 截断的前 8 位，它也是「这条报文有有效签名」的一份凭证，留在库里让重放在签名有效期
	// 内变得可行。要留的是时间戳、随机串、请求号那类能定位这一笔的东西。
	RecordableHeaders() []string
}

// Registry 按 catalog.Channel.Provider 的值查适配器。
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
