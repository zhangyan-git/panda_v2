package ums

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// eventReturn 是**结果页回跳**这条路上带出去的事件类型。
//
// 它不是一个结算事件。用户被渠道送回 returnUrl 时，钱到没到**早已由支付结果通知或查单决定
// 过**了——这一次跳转不携带任何新的收款事实（规范原文 §3：这一路只该「验签 → 展示」）。
//
// 取一个既不是 succeeded 也不是 failed 的值，同时买到一件事：provider.Notification 的约定
// 是「其余取值调用方记成 ignored 而不动支付状态」。哪怕将来有人把回跳的路由接到了回调处理器
// 上，这笔支付也一个字段都不会变——那道安全网是既有的，这里只是站到它后面。
//
// **不带渠道回跳里的状态词**（渠道的结果页通常会带一个 status）。理由同上：它看着像结论，
// 但它是展示用的，把它写进 payment_notifications.event_type 会让一条「用户点了完成」在库里
// 长得像一条收款结论。要那个词的话，它在浏览器地址栏里。
const eventReturn = provider.NotificationEvent("return")

// paymentCallback 是支付回调的报文。字段名逐字照抄老系统 HandlePayNotify:2242 读的那几个键。
//
// 老系统的回调入口（order_handler.go:1043）会按 Content-Type 把体解成 **JSON 或表单**两种
// map，而表单那条路上每个值都是**字符串**。所以 totalAmount 在两种投递方式下类型不同——这正是
// amountFen 要同时吃数字与字符串的原因。
type paymentCallback struct {
	MerOrderID    string `json:"merOrderId"`
	Status        string `json:"status"`
	TargetOrderID string `json:"targetOrderId"`
	TransactionID string `json:"transactionId"`
	// NotifyID 是渠道的通知号。**它只当交易号的最后一道兜底，绝不用来做去重键**
	// （见 notificationID 与 providerTransactionID）。
	NotifyID    string          `json:"notifyId"`
	TotalAmount json.RawMessage `json:"totalAmount"`
	PayTime     string          `json:"payTime"`
}

// Verify 实现 provider.Provider：验签并归一化一次渠道入站请求。
//
// # 三条路，入口只有一个
//
// 渠道打我们这个地址的方式有三种，判据是**这条请求自己的形状**，不是配置里配了什么：
//
//	GET                            → 结果页回跳（H5 独有）。只带出一个支付单号，事件类型是
//	                                 eventReturn，**结算侧一行都不动**（见 verifyReturn）
//	POST + Authorization 头        → 小程序那条路的支付结果通知，OPEN-BODY-SIG（见 verifySignedCallback）
//	POST + 没有 Authorization 头   → H5 的支付结果通知，key 拼接签名（见 verifyKeyAppendCallback）
//
// 分派的依据里**没有「哪个槽配了没配」**：一条什么签名材料都没带的请求，不该因为我们恰好
// 没配通讯密钥就被报成「运维漏配」。形状决定该用哪一套算法，密钥只决定那一套验不验得过。
//
// # 错误分档（三条路共用，顺序有讲究）
//
//	配置解不开               → 普通错误（签名还没验，不能声称验过了）
//	密钥是空的               → ErrSecretNotConfigured（运维漏配，不是有人在伪造）
//	签名缺失 / 解不出 / 对不上 → ErrSignatureMismatch
//	签名过了但报文读不懂 / 缺字段 → ErrInvalidNotification
//
// 最后一档的分界线不是装饰：调用方（service.callback）拿它写
// payment_notifications.signature_verified——**只有 ErrInvalidNotification 才记 true**。
// 在这一档之外包上它，库里就会出现一条「签名验过了」而其实根本没验的记录。
//
// 「签名材料缺失」一律算进 ErrSignatureMismatch 而不是普通错误：一条没有 Authorization 头、
// 也没有 key 拼接签名的请求，**不存在**一份我们该认下来的签名，它就是签名不对。
func (p *Provider) Verify(_ context.Context, req provider.NotificationRequest) (provider.Notification, error) {
	protocol, err := Parse(req.Method.ChannelConfig)
	if err != nil {
		return provider.Notification{}, fmt.Errorf("channel %q: %w", req.ChannelCode, err)
	}
	// GET 是结果页回跳。**只有它走这条路**：回跳是浏览器带来的，它不携带任何新的收款事实，
	// 所以它的处置与回调差得很远（一个动支付状态、一个只展示），不能合流。
	if req.HTTPMethod == http.MethodGet {
		return verifyReturn(protocol, req)
	}
	if authorization := req.Headers.Get(headerAuthorization); authorization != "" {
		return verifySignedCallback(protocol, req, authorization)
	}
	return verifyKeyAppendCallback(protocol, req)
}

// verifySignedCallback 是**小程序那条路**的支付结果通知：Authorization 头里的 OPEN-BODY-SIG。
//
// # 这里的验签是本仓库的推断，不是从老系统抄来的
//
// **老系统根本没有验签**：umspay_service.go:375 的 ParseNotify 是个空壳（注释写着「验签
// （可选）」，函数体只有一句 json.Unmarshal），而真正在收回调的 UMSPayNotify 连那个空壳都
// 没调——它把体解成 map 就直接去改订单状态。也就是说这一族的回调在 panda_serve 里是一个
// **不设防的入口**。
//
// 所以下面这段验签是按**出站签名**做的对称推断：把 Authorization 头拆回四段，用 appId +
// 时间戳 + 随机串 + 报文摘要重算一遍，常量时间比较。它成立的前提是渠道按同一套算法签它的
// 回调——**这个前提我们没有任何证据**，必须拿真实凭据用渠道侧的回调实测确认。要确认的具体
// 是两件事：回调到底带不带 Authorization 头，以及带的那个是不是同一个算法的结果。
//
// 拿到真实回调后**如果发现它带的是一个别的东西**（比如 H5 那条路的 key 拼接签名被用在了
// 小程序这条路上），改法是改这里的判据，而**不是**再加一层「两套都试一遍」——那等于把两次
// 验签里较松的那一次当成结论。
//
// # appId 必须钉死
//
// 待签串以 appId 开头，所以一个用**别人的 appId** 签出来的报文是可以自洽的——除非我们先把它
// 与配置里的 appId 比一遍。这一条不能省：少了它，任何知道「某个 appId 配某把密钥」的人都能
// 伪造回调。
//
// # 不做时间窗
//
// 时间戳进了待签串，所以我们**可以**做 ±5 分钟窗口。这里仍然不做，理由与 hmacbody 那边逐字
// 相同：真正的防重放靠 payment_notifications 的 UNIQUE (provider, notification_id)，而时间窗
// 会带来一个新的失败模式——渠道重投一条几小时前的通知（这是重投队列的常态），我们判它过期、
// 回非 2xx、于是它一直重投。**宁可多收几条重复的通知，不能把一条真的收款通知判成过期而永远
// 收不到。**
func verifySignedCallback(protocol *Protocol, req provider.NotificationRequest, authorization string) (provider.Notification, error) {
	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		// 空密钥是「验不了签」，一律拒绝。绝不能有「解不出密钥就跳过验签」这种降级——
		// 那是一条不设防的回调入口，而老系统的现状正好演示了它长什么样（见上）。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: credential slot %q is empty",
			provider.ErrSecretNotConfigured, req.ChannelCode, protocol.secretRef)
	}

	fields, err := parseAuthorization(authorization)
	if err != nil {
		return provider.Notification{}, fmt.Errorf("%w: channel %q: %v",
			provider.ErrSignatureMismatch, req.ChannelCode, err)
	}
	if fields.appID != protocol.appID {
		return provider.Notification{}, fmt.Errorf("%w: channel %q: callback %s is not this channel's appId",
			provider.ErrSignatureMismatch, req.ChannelCode, headerAuthorization)
	}
	expected := Sign(fields.appID, fields.timestamp, fields.nonce, req.Body, secret)
	if !hmac.Equal([]byte(fields.signature), []byte(expected)) {
		params, _ := bodyParams(req.Body)
		return provider.Notification{}, fmt.Errorf("%w: channel %q: %s header does not match this body (%s)",
			provider.ErrSignatureMismatch, req.ChannelCode, headerAuthorization, inboundDigest(params, true))
	}

	// —— 从这里往下，报文是渠道发来的、没被改过。字段校验才有意义。 ——
	return normalizeCallback(protocol, req.ChannelCode, req.Body)
}

// verifyKeyAppendCallback 是 **H5 那条路**的支付结果通知：没有 Authorization 头，签名是渠道
// 用通讯密钥按 key 拼接算出来的（见 keyappend.go）。
//
// 它落在这个入口上是因为**同一张渠道行两条路共用**：小程序与 H5 的支付方式挂在同一个渠道上，
// 而回调地址是渠道级的（/v1/payments/callback/{channelCode}）。区分它们的是请求的形状，
// 不是配置。
//
// # 报文体两种都认
//
// 规范写的是 form 表单，而老系统的回调入口按 Content-Type 分流成 JSON 或表单两种（见
// bodyParams）。所以这里不按 Content-Type 认、也不按「哪种投递方式」分流——参数取出来之后
// 两条路合流成同一段归一化（normalizeCallback）。
func verifyKeyAppendCallback(protocol *Protocol, req provider.NotificationRequest) (provider.Notification, error) {
	params, err := inboundParams(req)
	if err != nil {
		// 报文解不出参数 → 验不了签 → 与「签名头解不出」同一档。**绝不能是
		// ErrInvalidNotification**：那一档的意思是「签名验过了、报文不合法」，调用方会据此
		// 把 signature_verified 记成 true。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: %v",
			provider.ErrSignatureMismatch, req.ChannelCode, err)
	}

	given := givenSignature(params)
	if given == "" {
		// 既没带头、签名参数也不在。这一条**发生在读凭据之前**：它说的是这条请求本身没有
		// 任何签名材料，与我们的槽配没配无关——把一份伪造的请求报成「密钥没配」会让排查的人
		// 去翻配置，而真正该看的是「谁在打这个地址」。摘要里带着参数名，是给第一条真实回调
		// 留的线索（见 inboundDigest）。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: callback carries neither a %s header nor a key-appended signature (%s)",
			provider.ErrSignatureMismatch, req.ChannelCode, headerAuthorization, inboundDigest(params, false))
	}

	secret, err := commSecret(protocol, req)
	if err != nil {
		return provider.Notification{}, err
	}
	signType, err := resolveSignType(params[paramSignType])
	if err != nil {
		return provider.Notification{}, fmt.Errorf("%w: channel %q: %v",
			provider.ErrSignatureMismatch, req.ChannelCode, err)
	}
	if !signatureMatches(given, keyAppendSignature(params, secret, signType)) {
		return provider.Notification{}, fmt.Errorf("%w: channel %q: key-appended signature does not match (%s)",
			provider.ErrSignatureMismatch, req.ChannelCode, inboundDigest(params, false))
	}

	// 签名过了。往下的字段校验与 Authorization 头那条路**共用同一段**：把参数表拼回一份
	// JSON 对象再走既有的解码路径，比另写一份「从 map 里读字段」的归一化安全——两份实现
	// 迟早会在「成功必须有金额」这类判据上分叉。
	body, err := paramsAsJSON(params)
	if err != nil {
		return provider.Notification{}, fmt.Errorf("channel %q: re-encode the callback parameters: %w", req.ChannelCode, err)
	}
	return normalizeCallback(protocol, req.ChannelCode, body)
}

// verifyReturn 是**结果页回跳**：用户在渠道收银台上付完、点「完成」之后被送回我们这里那一次
// GET。
//
// # 它只验签，不结算
//
// 规范原文 §3 把这个语义写得很清楚：**它只是给用户看的，钱到没到只看支付结果通知与查单**。
// 所以这条路上没有金额、没有状态、没有渠道交易号——只有「哪一单」。带出去的事件类型是
// eventReturn（一个**不是** succeeded / failed 的值），于是 provider.Notification 那条既有
// 约定（「其余取值调用方记成 ignored 而不动支付状态」）在我们接错入口时兜住最后一层。
//
// # 它与通知共用那套 key 拼接签名
//
// 规范把两者分开写（§1.9.3 与 §1.10.2），但规则是同一套：排序拼串、末尾接通讯密钥、按
// signType 取 MD5 或 SHA256。合起来实现有两处收益：只可能错一次，以及「两条路验的是同一件
// 事」这件事在代码里是可见的。**若真实回跳用的其实是另一套**（这是文档 §10 列的待确认项），
// 分开的成本只是在这里多一个分支，而现在先按同一套写——它符合规范对这一路唯一的那句描述。
func verifyReturn(protocol *Protocol, req provider.NotificationRequest) (provider.Notification, error) {
	params := flattenValues(req.Query)
	if len(params) == 0 {
		// 查询串里一个参数都没有：那不是一个从收银台回来的浏览器，是有人直接打了这个地址。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: result-page return carries no query parameters",
			provider.ErrSignatureMismatch, req.ChannelCode)
	}

	given := givenSignature(params)
	if given == "" {
		return provider.Notification{}, fmt.Errorf("%w: channel %q: result-page return carries no signature (%s)",
			provider.ErrSignatureMismatch, req.ChannelCode, inboundDigest(params, false))
	}
	secret, err := commSecret(protocol, req)
	if err != nil {
		return provider.Notification{}, err
	}
	signType, err := resolveSignType(params[paramSignType])
	if err != nil {
		return provider.Notification{}, fmt.Errorf("%w: channel %q: %v",
			provider.ErrSignatureMismatch, req.ChannelCode, err)
	}
	if !signatureMatches(given, keyAppendSignature(params, secret, signType)) {
		return provider.Notification{}, fmt.Errorf("%w: channel %q: result-page return signature does not match (%s)",
			provider.ErrSignatureMismatch, req.ChannelCode, inboundDigest(params, false))
	}

	// 回跳带回来的 merOrderId 是**渠道侧**的写法（带账户要求的前缀），解回我们的支付单号再
	// 往外交（见 orderid.go）：调用方要拿它去填结果页地址、去库里找那一单，两处认的都是
	// 内部单号。
	paymentNo := protocol.paymentNoOf(strings.TrimSpace(params[paramMerOrderID]))
	if paymentNo == "" {
		// 一个跳回来说不清是哪一单的回跳——拼不出结果页地址，也就没有可展示的东西。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: result-page return has no %s",
			provider.ErrInvalidNotification, req.ChannelCode, paramMerOrderID)
	}
	return provider.Notification{
		NotificationID: paymentNo + ":" + string(eventReturn),
		EventType:      eventReturn,
		PaymentNo:      paymentNo,
	}, nil
}

// commSecret 取这一族的**第二把**凭据：通讯密钥（入站验签用，见 keyappend.go）。
//
// 两条都要拒：槽名没配（config 里 `sign.commSecretRef` 是空的）与槽是空的（配了名但没写值）。
//
// 前者尤其不能放过——凭据表里 `""` 这个键是**渠道行 secret_ref 那条兜底路径**
// （见 provider.ChannelSecret），拿它去取值等于把 appKey 当成通讯密钥用。那样我们会拿一把
// 不相干的密钥去验签、然后在日志里报「签名错误」，而真正的原因是槽名从来没配上。
func commSecret(protocol *Protocol, req provider.NotificationRequest) (string, error) {
	if protocol.commSecretRef == "" {
		return "", fmt.Errorf("%w: channel %q: config.sign.commSecretRef is not set, so an inbound key-appended signature cannot be verified",
			provider.ErrSecretNotConfigured, req.ChannelCode)
	}
	secret := req.Secrets.Get(protocol.commSecretRef)
	if secret == "" {
		return "", fmt.Errorf("%w: channel %q: credential slot %q is empty",
			provider.ErrSecretNotConfigured, req.ChannelCode, protocol.commSecretRef)
	}
	return secret, nil
}

// normalizeCallback 把一份**已经验过签**的报文翻成归一化的通知。
//
// 三条入站路里两条（OPEN-BODY-SIG 与 key 拼接）最后都走到这里：它们的差别全在「签名怎么验」，
// 而「报文里的字段怎么读、什么算成功、成功必须带什么」是同一件事。分开写两份的后果是同一条
// 判据在两个入口上不一致，而那种不一致只有在真实回调打进来时才会暴露。
//
// 它要 protocol 是因为报文里的 merOrderId 是渠道侧的写法（带账户要求的前缀），解回我们的
// 支付单号需要知道是哪个前缀（见 orderid.go）。
func normalizeCallback(protocol *Protocol, channelCode string, body []byte) (provider.Notification, error) {
	var callback paymentCallback
	if err := json.Unmarshal(body, &callback); err != nil {
		// 签名是好的，报文读不懂。这**是** ErrInvalidNotification：调用方据此记
		// signature_verified=true 并把这行标成 failed——「签名对了但报文不完整」正是那一列
		// 存在的意义。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: body is not json",
			provider.ErrInvalidNotification, channelCode)
	}

	notification := provider.Notification{
		NotificationID:        notificationID(callback),
		EventType:             classifyCallback(callback),
		PaymentNo:             protocol.paymentNoOf(strings.TrimSpace(callback.MerOrderID)),
		ProviderTransactionID: providerTransactionID(callback),
	}

	if notification.EventType != provider.EventSucceeded {
		// 老系统的回调**只处理成功**（HandlePayNotify:2298 那个 if），其余状态一律
		// 「跳过处理」——没有任何失败分支。这里照抄这个处置，并且**不**把它映射成
		// provider.EventFailed：
		//
		//   - 那些状态里混着 TRADE_CLOSED（确实没成）、WAIT_BUYER_PAY（还没付）、
		//     TRADE_REFUND（收款**之后**的退款）三类，映射成 failed 会把最后那一类变成
		//     「一笔已经收妥的单被标成失败」；
		//   - 原样把渠道的词当事件类型带出去，service 记一行 ignored、回成功应答：
		//     支付状态一个字段都不动，而「渠道说的到底是什么词」在库里查得到
		//     （payment_notifications.event_type 是自由文本，没有 CHECK）。
		//
		// 这里**不校验支付单号**：一条与收款无关的通知不带我们的单号是正常的，而拿一个必填
		// 校验把它变成「拒收」会让渠道一直重投一条我们永远处理不了的通知。
		return notification, nil
	}

	if notification.PaymentNo == "" {
		// 一个成功通知却不说是哪一单——报文没法用。这一条**必须**有人看见。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: callback has no merOrderId",
			provider.ErrInvalidNotification, channelCode)
	}

	amount, ok := amountFen(callback.TotalAmount)
	if !ok || amount <= 0 {
		// 金额是必填的，而且**这是推断、不是照抄**：老系统的回调里根本没读 totalAmount
		// （它直接拿 merOrderId 找到订单就把状态改成已支付），所以「成功回调一定带金额」
		// 这件事我们没有任何证据。
		//
		// 但 V2 必须要求它：repository/callback.go:216 会**无条件**拿 Notification.Amount 与
		// 支付单的金额比一遍，对不上就整条拒收。一条不带金额的成功回调在这里放行的话，
		// 到了那边一定会被拒——错误串会变成「金额对不上」，而真正的原因是「渠道没给金额」。
		// 在这里先报出来，错误串能指着 totalAmount 说。
		//
		// **若真实渠道的回调确实不带金额**，要回来重新讨论 Notification.Amount 的语义
		// （比如换成「金额可选、缺了就只信回调单号」），而不是在这个适配器里糊一个 0 或者
		// 去查一次单——那两样都会让「金额是这道防线的一部分」这个事实从库里消失。
		return provider.Notification{}, fmt.Errorf("%w: channel %q: succeeded callback has no usable totalAmount",
			provider.ErrInvalidNotification, channelCode)
	}
	notification.Amount = amount
	notification.PaidAt = parsePayTime(callback.PayTime)
	return notification, nil
}

// classifyCallback 把回调的状态词翻成归一化的事件类型。
//
// **只认成功**，其余原样带出去（见 Verify 里那一段的长注释）。所以这里没有 EventFailed 这条
// 分支——不是漏了，是这一族的回调里没有任何一个词能确定地表示「这笔钱不会来了」。
//
// 成功的三个写法与下单、查单共用 isSucceededStatus：老系统在三处各抄了一遍同样的字符串，
// 抄成一处是为了让「少认一个写法」这种错只可能犯一次。
func classifyCallback(callback paymentCallback) provider.NotificationEvent {
	status := strings.TrimSpace(callback.Status)
	if isSucceededStatus(status) {
		return provider.EventSucceeded
	}
	// 原样把渠道的取值当成事件类型带出去。空串也照带：它是一个「渠道连状态都没给」的通知，
	// 在库里显示成空比显示成某个我们编的词诚实。
	return provider.NotificationEvent(status)
}

// notificationID 取这次回调的去重键：merOrderId + ":" + status。
//
// # 为什么带上 status
//
// 渠道可能先推一条「处理中」再推一条「成功」，两条的 merOrderId 相同。单用订单号做键时，
// 第二条会撞上 payment_notifications 的唯一键、被当成重复丢掉，于是**这笔钱永远结算不了**。
// 带上 status，两个状态就是两条通知；而同一条通知重投时两段都不变，仍然被唯一键挡住。
// 这条理由与 hmacbody 那边逐字相同。
//
// # 为什么不用 notifyId
//
// 它是渠道给的通知号，看着更适合做去重键，但有两条不能用的理由：
//
//   - 我们**没有证据**说明它的粒度。若渠道是「一单一 notifyId」（不同状态共用一个通知号），
//     那么第二条通知会被永久丢掉——正是上面那个「钱结算不了」的陷阱，而且比它更隐蔽：
//     带上 status 的合成键在两种粒度下都安全。
//   - 它不一定存在。老系统 HandlePayNotify 把它当**最后一道兜底**读（前两个是 targetOrderId
//     与 transactionId），说明有的回调模板里它为空。
//
// # 两段都为空时
//
// 键会退化成 ":<status>"，于是所有「没有单号、状态也一样」的通知互相撞键。这是可接受的：
// 走到那里的通知状态一定不是成功（成功那条路要求 merOrderId 非空，见 Verify），也就是说它们
// 本来就不动任何支付状态——撞键的后果只是少记几行 ignored。
//
// # 键里用的是渠道侧的写法，不是解回来的支付单号
//
// 这一列是「渠道说它发的通知叫什么」，与 payment_notifications.body 一样属于**入站事实**，
// 所以原样存渠道给的那个 merOrderId（带账户要求的前缀，见 orderid.go）。它与 payment_no 不
// 同值不影响任何事：这个键只有两个用途——UNIQUE (provider, notification_id) 那道防重放的闸，
// 以及运维在库里认「这条通知是哪一条」。要按支付单找通知，这张表自己有 payment_no 列。
func notificationID(callback paymentCallback) string {
	return strings.TrimSpace(callback.MerOrderID) + ":" + strings.TrimSpace(callback.Status)
}

// providerTransactionID 取渠道侧的交易号，按老系统 HandlePayNotify 的三级回落：
// targetOrderId → transactionId → notifyId。
//
// 逐级回落的理由是「这三个未必都有」：老系统把三者写成 if/else 链，说明真实报文里出现过
// 只有其中某一个的情况。
//
// **最后一级是个已知的语义瑕疵**：notifyId 是**通知**号，不是**交易**号，把它写进
// payments.provider_transaction_id 等于把两个不同的东西混成一列。这里照抄老系统（它就是这么
// 落的），因为不抄的代价是「一个带 notifyId 的回调拿不到交易号」——而那个号在 V2 会被写进
// 支付单，成为对账时唯一的渠道侧锚点。要收口得先拿到一份真实回调，看它到底带了哪几个号。
func providerTransactionID(callback paymentCallback) string {
	return firstNonEmpty(callback.TargetOrderID, callback.TransactionID, callback.NotifyID)
}

// amountFen 把回调 / 查单应答里的金额读成**分**。
//
// 这一族的金额单位本来就是分（老系统的 totalAmount 就是 int64 分），所以这里只解码、不换算——
// 与 hmac_body 那一族（元、float64）相反。
//
// 需要它是因为同一个字段在两种投递方式下类型不同：老系统的回调入口按 Content-Type 把体解成
// JSON 或**表单**，表单那条路上每个值都是字符串，而 JSON 那条路上是数字。老系统的
// numberAsInt64（order_service.go:2092）只吃数字（float64 / float32 / int / int64 /
// json.Number），遇到字符串会返回「没给」——它只用在查单那条路上，恰好没踩到，但这说明
// 「金额的形状不止一种」是这条渠道的既有事实。
//
// 两种形状都认，输出一律是分：
//
//	12800        → 12800   （JSON 数字）
//	"12800"      → 12800   （表单字符串）
//	"12800.00"   → 12800   （带小数点的字符串，四舍五入到分）
//
// 读不出来返回 false，调用方据此拒绝（成功回调）或记「不知道」（查单）。**返回 0 与返回
// false 是两件事**：0 是一个渠道明确说了的金额，false 是「这个字段我们没读懂」。
func amountFen(raw json.RawMessage) (int64, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return 0, false
	}
	if strings.HasPrefix(text, `"`) {
		var quoted string
		if err := json.Unmarshal(raw, &quoted); err != nil {
			return 0, false
		}
		text = strings.TrimSpace(quoted)
		if text == "" {
			return 0, false
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// 既不是数字也不是数字字符串（比如 "12800分"）。当作没给，而不是猜一个。
		return 0, false
	}
	return int64(math.Round(value)), true
}

// parsePayTime 把回调里的成交通知时间翻成时间。
//
// 零值表示「渠道没给或给的东西看不懂」，调用方看到零值会退回 NOW()（见 repository/callback.go
// 里 paidAt.IsZero() 那一处）。**不能拿 0 直接建 time.Unix(0)**：那会写成 1970 年，一条 1970 年
// 的流水会让对账报表永远对不平。
//
// 形状认三种（前两种报文里没有时区信息，按东八区解析）：老系统 RecoverStuckCoffeeOrder
// 用的那个墙上时间格式、紧凑格式、RFC3339。多认两种的成本是几行，收益是一条时间戳格式与
// 查单不同的回调不会退化成 NOW()。
func parsePayTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{requestTimestampLayout, signTimestampLayout, time.RFC3339} {
		if parsed, err := time.ParseInLocation(layout, raw, callbackZone); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// callbackZone 是回调里那个不带时区的墙上时间所属的时区。
//
// 原先是 time.Local：本机跑（东八区）是对的，但服务一旦进容器就会被当成 UTC 读
// ——compose 里没给服务设 TZ，容器默认 UTC，而**运行环境缺 tzdata 时这个错法不会报错**。
// 成交通知时间会整体差 8 小时，且只在容器里错、在本机不错。
//
// 用 FixedZone 而不是 LoadLocation("Asia/Shanghai")：后者依赖运行环境里有 tzdata，
// 缺了会不报错、悄悄退回 UTC，正好落回同一个错法。中国从 1991 年起没有夏令时，固定
// +08:00 就是准的。同一取舍见 membership-service 的 model.ChargePeriod。
var callbackZone = time.FixedZone("CST", 8*3600)
