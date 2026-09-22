package ums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

// requestTimestampLayout 是**报文里**的时间格式（带空格、无时区）。
//
// 它与签名用的 signTimestampLayout（紧凑、无分隔）不是同一个格式。老系统两处各写一个，
// 而两个都进过渠道的校验，所以照抄——统一成一个的表现是签名或参数被拒，而两边打印出来
// 的时间看上去都对。
const requestTimestampLayout = "2006-01-02 15:04:05"

// createBody 是 /v1/netpay/wx/unified-order 的报文。
//
// 字段名逐字照抄老系统 UnifiedOrder:106 的那个 map，**每一个键都无条件出现**（连 srcReserve
// 这个恒为空串的也是）：老系统用的是 map，没有 omitempty 这回事，空值照样以 `"returnUrl":""`
// 的形状发出去。这里也不加 omitempty——渠道对「字段在但是空的」与「字段不在」的处理未必一样，
// 而我们没有任何文档可对照，照抄是唯一安全的做法（与 hmacbody 那边同样的取舍）。
//
// **没有过期时间字段**：老系统的 PaymentRequest.ExpireTime 传了，但它根本没进这个 map，
// 所以这条协议的下单报文里没有它。见包注释的已知缺口。
type createBody struct {
	AppID            string `json:"appId"`
	MsgID            string `json:"msgId"`
	RequestTimestamp string `json:"requestTimestamp"`
	MerOrderID       string `json:"merOrderId"`
	// SrcReserve 恒为空串，照抄老系统（注释写着「可选」）。
	SrcReserve string `json:"srcReserve"`
	Mid        string `json:"mid"`
	Tid        string `json:"tid"`
	InstMid    string `json:"instMid"`
	// TotalAmount 的单位是**分**，而且是 JSON 数字。
	//
	// 这一处最容易搞错，因为 hmac_body 那一族发的是元、form_md5 那一族发的是字符串——三种都
	// 不一样。老系统的 PaymentRequest.TotalAmount 是 int64 分，直接塞进 map，所以这里也原样。
	TotalAmount int64  `json:"totalAmount"`
	NotifyURL   string `json:"notifyUrl"`
	ReturnURL   string `json:"returnUrl"`
	TradeType   string `json:"tradeType"`
	DomainName  string `json:"domainName"`
	SourceCode  string `json:"sourceCode"`
	SubOpenID   string `json:"subOpenId"`
	// 分账三键，见 divisionFields。它是这一份报文里**唯一有条件出现**的一组键。
	divisionFields
}

// divisionFields 是下单报文里的分账三键，小程序与 H5 两份形状共用。
//
// # 为什么这一组可以有条件地出现
//
// createBody 抬头那句「每一个键都无条件出现」说的是老系统用 map 拼出来的那些键，而**这一组
// 本来就写在老系统的 `if req.DivisionFlag` 里**（umspay_service.go:124-130）：不发分账时一个
// 都不带。所以条件出现是渠道那边既有的行为，不是这里新引入的取舍。
//
// 反过来说也不能发空子单：规范明说 `divisionFlag=true` 时 `subOrders` 不能为空，发一个
// `"divisionFlag":false` 加空数组是自找拒绝（docs/unionpay-h5-pay.md:104-107）。
//
// # 两个键名
//
//   - 子单里的是 **totalAmount 而不是 amount**：老系统内部那个 SettleDivisionOrder 叫
//     amount，进报文前换成 DivisionOrder 的 totalAmount（order_service.go:1977-1979）。
//     写成 amount 渠道会当作「这条子单没有金额」。
//   - PlatformAmount 用指针而不是 int64：平台那一份**可以是 0**（整单都分出去了），而
//     omitempty 会把 0 吞掉，于是「Σ子单 = totalAmount」这条恒等式在报文里就少一项。
type divisionFields struct {
	DivisionFlag   *bool              `json:"divisionFlag,omitempty"`
	PlatformAmount *int64             `json:"platformAmount,omitempty"`
	SubOrders      []divisionSubOrder `json:"subOrders,omitempty"`
}

// divisionSubOrder 是一条分账子单。键名照抄老系统进报文的那一版（umspay_service.go:35-39）。
type divisionSubOrder struct {
	Mid    string `json:"mid"`
	Amount int64  `json:"totalAmount"`
	Remark string `json:"remark,omitempty"`
}

// buildDivision 把调用方的分账指令翻成报文里的三个键。
//
// 没有指令（nil）**或指令里没有子单**都返回零值：那一组键一个都不出现。这里不判「子单是不是
// 空的以外还有什么毛病」——金额是调用方用 computeSettlement 算好的，恒等式在那一边保证
// （见 repository/settlement.go 的 checkSettlementIdentity），适配器不替它复算第二遍。
func buildDivision(division *provider.DivisionInstruction) divisionFields {
	if division == nil || len(division.SubOrders) == 0 {
		return divisionFields{}
	}
	flag := true
	platform := division.PlatformAmount
	subOrders := make([]divisionSubOrder, 0, len(division.SubOrders))
	for _, sub := range division.SubOrders {
		subOrders = append(subOrders, divisionSubOrder{
			Mid:    sub.Mid,
			Amount: sub.Amount,
			Remark: sub.Remark,
		})
	}
	return divisionFields{DivisionFlag: &flag, PlatformAmount: &platform, SubOrders: subOrders}
}

// createResponse 是渠道的下单应答。
//
// 只解这三个键：errCode / errMsg 是判据，miniPayRequest 是要回给客户端的东西。**整份应答不进
// 任何落点**——方案 11.5 禁止未脱敏的完整应答入库。
//
// **不读 targetOrderId**：老系统下单成功时只校验 miniPayRequest 就返回了，我们手上没有证据
// 说明这一族的应答里到底有没有那个字段、叫什么名字，所以建单时不记渠道交易号（见包注释的
// 已知缺口）。凭空读一个字段的风险是：读到一个语义不同的同名字段并把它写进
// payments.provider_transaction_id，于是**真回调反而对不上号而被拒**。
// MiniPayRequest 收成 RawMessage 而不是 map[string]any：**它必须与 errCode 分两步解码**。
//
// 直接用 map 的话，一份 `{"errCode":"SUCCESS","miniPayRequest":"拉不起来"}`（子对象类型不对）
// 会让整份应答解码失败，于是我们报出 UNPARSEABLE_RESPONSE「报文不是 json」——可那份报文是
// 合法的 json，错的只是其中一项，而运维照着这句话会去查渠道的 Content-Type。
//
// 两步解码之后，errCode 一定读得到，miniPayRequest 的形状问题由 validMiniPayRequest 报出来。
// 分类结果（unknown）不变，变的是错误串能不能指着真正出问题的那一项。
type createResponse struct {
	ErrCode string `json:"errCode"`
	ErrMsg  string `json:"errMsg"`
	// ErrInfo 是**同一个渠道里另一条协议**用的说明字段名，两个都要认。
	//
	// 不是防御性编程，是真凭据联调实测出来的：H5 那条（GET + OPEN-FORM-PARAM）失败时回的是
	// `{"errCode":"1000","errInfo":"Timestamp解析失败"}`，而 POST+JSON 那条回的是 errMsg。
	// 只读 errMsg 的后果不是崩溃，是**诊断信息静默消失**——运维在流水里只看到
	// 「provider returned http 412」加一个 errCode，而渠道明明把原因写给了我们。
	ErrInfo        string          `json:"errInfo"`
	MiniPayRequest json.RawMessage `json:"miniPayRequest"`
}

// providerMessage 取渠道写的错误说明，统一两条协议的两个字段名。
//
// 顺序是 errMsg 优先：两条同时非空时它们说的是同一件事（同一份应答的两种写法），取哪个都对；
// 而 errMsg 是小程序那条路一直在用的，先读它就不会改变既有的输出。
func (r createResponse) providerMessage() string {
	if message := strings.TrimSpace(r.ErrMsg); message != "" {
		return message
	}
	return strings.TrimSpace(r.ErrInfo)
}

// miniPayKeys 是拉起小程序支付所需的六个键，逐字照抄老系统的 ValidMiniPayRequest:215。
//
// 六个**都必须是非空字符串**：少任何一个，客户端拿着这份参数都起不了支付，而那时用户看到的
// 是「点了没反应」——错误消失在客户端 SDK 里，我们这边一条日志都没有。
var miniPayKeys = []string{"appId", "nonceStr", "package", "paySign", "signType", "timeStamp"}

// Create 实现 provider.Provider：拼报文 → 算签名 → 发出去 → 读应答。
//
// # 两条路，按 action 分派
//
// 同一个渠道行下挂着两种**协议**：小程序那条是 POST + JSON 体 + OPEN-BODY-SIG 请求头，H5 那条
// 是 GET + 参数全拼 URL + OPEN-FORM-PARAM，而且**没有应答报文**（浏览器直接跳）。分派点照
// wechatv3/create.go 的先例写在最前面，见下面那一段。
//
// # 报文体只编一次
//
// 这一族的签名原文里含 **sha256(报文体)**，所以体必须**先编好、再对它算签名、再把同一份字节
// 发出去**。这是「签什么就发什么」那条不变量唯一的实现方式：编两遍（一遍签名、一遍发送）
// 只要有一处空值判断不同，签名就永远对不上，而两边打印出来的报文看上去一模一样。
//
// # 三种「没发出去」
//
// 配置解不开、密钥是空的、需要 openid 而它没给——都在这里**本地拒**（返回 err、一个字节都
// 不发）。共同的理由是：它们都会让渠道回一句与本意无关的错（「参数不对」「签名错误」），
// 而那条错误会盖住真正的原因。
func (p *Provider) Create(ctx context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	protocol, err := Parse(req.Method.ChannelConfig)
	if err != nil {
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: %w", req.Method.ChannelCode, err)
	}

	// 分派放在凭据解析之前：一条配错了 action 的支付方式（比如把 qrcode 指到这一族）应该报出
	// 「这个 action 没实现」，而不是先报「凭据是空的」——后者会让人去凭据那一栏白找一遍。
	switch req.Method.Action {
	case provider.ActionH5:
		return p.createH5(ctx, protocol, req)
	case provider.ActionNativePay:
		// 小程序那条路：走完这个 switch 之后的全部代码，一字不改。
		//
		// 只认这一个 action，不顺手把 jump_miniapp 也收进来：这条路的产出是**微信
		// requestPayment 的六个参数**，而「跳对方小程序」的客户端拿到它们什么都做不了。
		// 多认一个 action 换来的是一笔在流水里看着完全正常的坏支付。
	default:
		// 明确报错，**不静默走某个分支**：走错分支的代价是一笔按 H5 建的、却回给小程序一份
		// h5Url 的支付，客户端拿到看不懂的东西之后什么都做不了，而流水里一切正常。
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("%w: channel %q: action %q is not implemented by provider %s (it does %s and %s)",
				provider.ErrConfigInvalid, req.Method.ChannelCode, req.Method.Action, Name,
				provider.ActionNativePay, provider.ActionH5)
	}

	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		// 空密钥是「签不了」，不是「不签」。见 provider.CreateRequest.Secrets。
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: %w", req.Method.ChannelCode, provider.ErrSecretNotConfigured)
	}

	options := protocol.options(req.Method.Params, req.Method.Action)
	openID := strings.TrimSpace(req.WalletOpenID)
	if openID == "" && protocol.requiresOpenID(options.tradeType) {
		// openid 是**付款人**，而微信的预支付单必须指定付款人。取不到就本地拒：发一份
		// subOpenId 为空的报文过去，渠道要么拒、要么更糟——把它当成某个默认用户。
		//
		// 判据是「生效的 tradeType」（params 覆盖之后的），见 requiresOpenID。
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: %w: tradeType %q needs a wallet openid but none was given",
				req.Method.ChannelCode, provider.ErrConfigInvalid, options.tradeType)
	}
	if req.Amount <= 0 {
		// 金额是权威值（见 provider.CreateRequest.Amount），它由调用方在锁内校验过。为 0 或
		// 为负说明契约被破坏了，而发一份 0 分报文过去只会换来渠道一句含糊的拒绝。
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: amount %d is not positive", req.Method.ChannelCode, req.Amount)
	}

	body, err := json.Marshal(protocol.buildBody(req, options))
	if err != nil {
		// 结构体里全是字符串与整数，Marshal 不会失败。留着这个分支是为了以后加字段时不会
		// 静默吞掉错误。
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: encode request body: %w", req.Method.ChannelCode, err)
	}

	response, err := p.post(ctx, protocol, secret, options.createPath, body)
	if err != nil {
		return protocol.transportFailure(req, err)
	}
	return protocol.interpretCreate(response.Body, response.StatusCode, response.Attempts, req), nil
}

// buildBody 把一次发起支付翻成这家渠道的报文。
//
// 每一处字段的来源都在注释里点名：这一族**没有配置驱动的字段映射**（与 form_md5 相反），
// 报文的形状是钉死的，所以「这一项从哪来」只能靠注释说清楚。
func (p *Protocol) buildBody(req provider.CreateRequest, options createOptions) createBody {
	// msgId 是渠道侧的**请求号**（老系统给它的是同一个订单号）。V2 用调用方给的幂等号
	// RequestID，它与 PaymentNo 的区别是「这一次调用」与「这一笔支付」的区别；没给就回落到
	// 支付单号——老系统两者同值，所以回落过去与它完全一致。
	msgID := strings.TrimSpace(req.RequestID)
	if msgID == "" {
		msgID = req.PaymentNo
	}
	return createBody{
		AppID: p.appID,
		MsgID: msgID,
		// 时间用渠道的墙上时间格式（带空格、无时区），逐字照抄老系统。
		RequestTimestamp: time.Now().Format(requestTimestampLayout),
		// merOrderId 是从我们的**支付单号**编出来的（加账户要求的前缀，见 orderid.go），不是
		// 订单号：回调与查单都要拿它认回支付单，这条对应关系一旦改掉，回调就落不到任何一单上。
		MerOrderID:  p.channelOrderID(req.PaymentNo),
		SrcReserve:  "",
		Mid:         p.mid,
		Tid:         p.tid,
		InstMid:     options.instMid,
		TotalAmount: req.Amount,
		// notifyUrl 由装配处按环境拼（见 service.notifyURL），不进渠道配置：同一个渠道在
		// dev / staging / prod 的回调域名不同，写进 config 就得多一套配置。
		NotifyURL: req.NotifyURL,
		// returnUrl 是支付完成后跳回的地址，纯展示参数（结算只看回调与查单），从 config 读。
		ReturnURL:  p.returnURL,
		TradeType:  options.tradeType,
		DomainName: p.domainName,
		SourceCode: p.sourceCode,
		// subOpenId 与要不要它无关：这个键在报文里**无条件出现**，支付宝那一类没有 openid 的
		// 支付方式发空串过去，与老系统一致。判断「这次需不需要 openid」是 Create 里本地拒
		// 那一段的事，不在这里。
		SubOpenID: strings.TrimSpace(req.WalletOpenID),
		// 分账随单下发。调用方给 nil 或给一个没有子单的指令，这一组键就一个都不出现。
		divisionFields: buildDivision(req.Division),
	}
}

// post 发一条下单 / 查单请求（POST + JSON 体 + OPEN-BODY-SIG 头）。
//
// 全族只有这一条出网路径：下单与查单用的是同一个头、同一套签名、同一个 Content-Type，分开
// 写两遍必然会让其中一条悄悄少一个头。
func (p *Provider) post(ctx context.Context, protocol *Protocol, secret, path string, body []byte) (*httpx.Response, error) {
	return p.client.Do(ctx, httpx.Request{
		URL: protocol.url(path),
		Header: map[string]string{
			"Content-Type":  "application/json",
			"Authorization": authorize(protocol, body, secret),
		},
		Body: body,
	})
}

// ===== H5 那条路 =====
//
// 下面是同一渠道下的第二种协议，全部落在这一节里。它与上面那一段的差别集中在四件事上：
// 方法是 GET、报文进 URL 的 content 参数、认证方式是 OPEN-FORM-PARAM、**没有应答报文**
// （渠道用 302 把浏览器送走）。报文的内容项也不同（没有 tradeType，多了 expireTime 与
// 微信 H5 那三项）。

// paramH5URL 是回给客户端的 H5 收银台地址的键名。
//
// 与 wechat_v3 那条路**逐字相同**（`{"h5Url": …}`）：客户端按 action 认这个键、不按渠道认，
// 所以两家渠道的 H5 必须回同一个键名——多一个拼法就是客户端多一个它不认识的分支。
const paramH5URL = "h5Url"

// h5CreateBody 是 H5 下单的**业务内容**——它会被编成 JSON、URL 编码之后放进查询串的 content。
//
// # 字段名逐字照抄渠道文档的业务内容表
//
// 依据是《全民付移动支付 H5支付 v20221020》的报文表（那一版文档就在仓库外的
// `开放平台H5支付/` 下，panda_serve 里**没有** H5 的实现可对照——老系统只做过小程序支付，
// 所以这一节没有任何「照抄老系统」可言，它的依据是**文档**，而文档与实现的差别只能靠真凭据
// 联调来定稿）。
//
// # 空值省略，而不是「每个键无条件出现」
//
// 这一条**与上面 createBody 相反**，两个理由：
//
//   - 报文表里除 requestTimestamp / merOrderId / mid / tid / instMid / totalAmount 之外
//     全部标着「否」（可选）。凭空发一个 `"expireTime":""` 过去，等于让渠道去解析一个空
//     的日期；
//   - content 是一段**逐字节签名**的 JSON。少发一个键换来的是「待签串与渠道那边拼出来的
//     待签串一致」这件事少一个变量——而老系统那套「每个键都发」的依据（它的 map 语义）
//     在这一条路上根本不存在。
//
// # 没有 tradeType
//
// 报文表里查无此键。H5 的四种形态是靠**接口路径**区分的（见 createPath），不是靠 tradeType，
// 所以 options 在 h5 分支里把它清空了。
type h5CreateBody struct {
	MsgID            string `json:"msgId,omitempty"`
	RequestTimestamp string `json:"requestTimestamp"`
	MerOrderID       string `json:"merOrderId"`
	SrcReserve       string `json:"srcReserve,omitempty"`
	Mid              string `json:"mid"`
	Tid              string `json:"tid"`
	InstMid          string `json:"instMid"`
	// TotalAmount 的单位同样是**分**（与小程序那条一致，见 createBody 上那一段）。
	TotalAmount int64 `json:"totalAmount"`
	// ExpireTime 是**渠道侧的**订单过期时间，格式 yyyy-MM-dd HH:mm:ss。
	//
	// 与小程序那条路的关键差别：这一项发出去之后，渠道那边的预支付单**自己会过期**。这正是
	// worker/expiry.go 里那句「不调渠道关单」在 H5 上依然成立的原因——关单能力实现了（见
	// operator.go），但过期清扫不需要它。
	ExpireTime string `json:"expireTime,omitempty"`
	NotifyURL  string `json:"notifyUrl,omitempty"`
	ReturnURL  string `json:"returnUrl,omitempty"`
	// 下面三项只有**微信 H5** 要（见 config.go 里 Protocol.sceneType）。
	SceneType  string `json:"sceneType,omitempty"`
	MerAppName string `json:"merAppName,omitempty"`
	MerAppID   string `json:"merAppId,omitempty"`
	// 分账三键。与小程序那条路**同一组键、同一个构造函数**（见 divisionFields）：
	// 分账是渠道能力，不因为下单走的是 H5 还是小程序而变。
	divisionFields
}

// createH5 走 H5 那条路：拼 content → 算签名 → 拼 URL → GET 出去（**不跟重定向**）。
//
// # 为什么不能跟重定向
//
// 这个接口的「应答」就是一次 302——渠道让我们把**浏览器**送到收银台去。跟着走的话，读回来的
// 是收银台的 HTML，而且 HTTP 状态还是 200，与「渠道建了单」看不出区别（见 httpx.Request 的
// NoFollowRedirect）。所以请求带 NoFollowRedirect，302 本身才是判据。
//
// # 三种「没发出去」
//
// 与小程序那条同源，另外多一条 createPath：H5 的四条下单路径**没有默认值**（见 config.options），
// 一条没配 createPath 的支付方式必须在这里被拒，而不是拿渠道 config 里那条小程序的路径发过去。
func (p *Provider) createH5(ctx context.Context, protocol *Protocol, req provider.CreateRequest) (provider.CreateResult, error) {
	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: %w", req.Method.ChannelCode, provider.ErrSecretNotConfigured)
	}

	options := protocol.options(req.Method.Params, req.Method.Action)
	if options.createPath == "" {
		// 四条 H5 路径（支付宝 / 微信 / 云闪付 / 微信转小程序）各自是一个接口，没有一个能当
		// 默认值。缺了就本地拒：拿一条别的路径发过去，渠道会回一句与本意无关的错（「业务类型
		// 不对」），而错误串不会告诉你少的是 createPath。
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("%w: channel %q: action %s needs endpoints.create (or payment_methods.params.createPath): "+
				"the four H5 endpoints have no default",
				provider.ErrConfigInvalid, req.Method.ChannelCode, provider.ActionH5)
	}
	if strings.TrimSpace(req.NotifyURL) == "" {
		// 空回调地址在这条路上比小程序那条更致命：H5 的用户付完之后被浏览器送走，我们这边
		// **没有任何别的成交通知**——支付单会停在 pending 直到超时关单，而钱在渠道那边。
		// 理由与 wechatv3 里那一段逐字相同，所以同样宁可在这里拒掉、一个字节都不发。
		//
		// （小程序那条路没有这条判据。它在这批之前就是那样，改它不在本刀范围内。）
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("%w: channel %q: notifyUrl is empty, the payment would never be settled",
				provider.ErrConfigInvalid, req.Method.ChannelCode)
	}
	if req.Amount <= 0 {
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: amount %d is not positive", req.Method.ChannelCode, req.Amount)
	}

	body, err := json.Marshal(protocol.buildH5Body(req, options))
	if err != nil {
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: encode request body: %w", req.Method.ChannelCode, err)
	}

	outbound := buildH5Request(protocol, options.createPath, body, secret)
	response, err := p.client.Do(ctx, httpx.Request{
		Method: http.MethodGet,
		URL:    outbound.url,
		// 没有请求头：这一族的认证参数**全部在查询串里**，签名的计算结果也不进头。
		NoFollowRedirect: true,
	})
	if err != nil {
		return protocol.transportFailure(req, err)
	}
	return protocol.interpretH5Create(response, req), nil
}

// buildH5Body 把一次发起支付翻成 H5 的业务内容。字段来源逐项对照 buildBody。
func (p *Protocol) buildH5Body(req provider.CreateRequest, options createOptions) h5CreateBody {
	// msgId 与小程序那条同源：调用方给的幂等号，没给就回落到支付单号。
	msgID := strings.TrimSpace(req.RequestID)
	if msgID == "" {
		msgID = req.PaymentNo
	}
	body := h5CreateBody{
		MsgID:            msgID,
		RequestTimestamp: time.Now().Format(requestTimestampLayout),
		MerOrderID:       p.channelOrderID(req.PaymentNo),
		Mid:              p.mid,
		Tid:              p.tid,
		InstMid:          options.instMid,
		TotalAmount:      req.Amount,
		NotifyURL:        strings.TrimSpace(req.NotifyURL),
		SceneType:        options.sceneType,
		MerAppName:       options.merAppName,
		MerAppID:         options.merAppID,
		divisionFields:   buildDivision(req.Division),
	}
	// returnUrl 优先取调用方给的（CreateRequest.ReturnURL，装配处按环境拼），回落到渠道 config
	// 里那一项。**与小程序那条路不同**：小程序只读 config，因为那条路上的客户端不跳浏览器，
	// 回跳地址无意义；H5 的收银台在浏览器里，回跳地址是这一次请求的一部分（同一个渠道在
	// dev / staging / prod 的回跳域名不同）。
	body.ReturnURL = firstNonEmpty(strings.TrimSpace(req.ReturnURL), p.returnURL)
	// 过期时间只在调用方给了的时候才发：零值格式化出来是 "0001-01-01 00:00:00"，那是一个
	// 渠道解析得了、但语义荒谬的值（预支付单在公元 1 年就过期了）。
	if !req.ExpiresAt.IsZero() {
		body.ExpireTime = req.ExpiresAt.Format(requestTimestampLayout)
	}
	return body
}

// interpretH5Create 把 H5 下单的**HTTP 应答**翻成 provider.CreateResult。
//
// # 判据是一次 3xx，不是一个报文
//
// 这个接口成功时不回体，它回一句 302 加一个 Location——那行 Location 就是收银台地址。所以
//
//	3xx + Location 非空  → ResultSuccess，PayParams{"h5Url": Location}
//	其余一律              → 不是成功
//
// 键名 h5Url 与 wechat_v3 逐字相同（客户端按 action 认、不按渠道认，见那个包里那一行注释）。
//
// # 「302 还是 200 + HTML」是待确认项
//
// 规范没有把「成功」明说成「302」，这一条是从「把链接输入浏览器进行跳转」那句话推出来的。
// **用真凭据联调时第一件要确认的就是它**：如果真实渠道回的是 200 加一段带着收银台地址的
// HTML，那么这个分支要改，而 `2xx 无 Location` 那条 ResultUnknown 的兜底正好保证了那时我们
// 不会把钱判丢——支付单进「结果不明」，由既有的查单探针去问（见 service/create.go 的
// settleByQuery）。
//
// # 4xx 与 5xx 的分档照抄小程序那条
//
// 4xx 是渠道在协议层就拒了（路径不对、签名不对、商户号不认识），它没有建单 → ResultFailed，
// 用户可以立刻换一种方式付。5xx 与「2xx/3xx 却不给 Location」一律 ResultUnknown：对面到底
// 建没建单我们不知道，**绝不能当失败**。
//
// 失败码取状态码本身（`HTTP_400`），只有「状态码是好的、内容不对」那一档才是
// `H5_UNEXPECTED_RESPONSE`——排查时先看的就是状态码，把它藏进一个自造的词里没有好处。
func (p *Protocol) interpretH5Create(response *httpx.Response, req provider.CreateRequest) provider.CreateResult {
	result := provider.CreateResult{
		Result:     provider.ResultUnknown,
		HTTPStatus: response.StatusCode,
		Attempts:   response.Attempts,
		ResponseSummary: map[string]any{
			"httpStatus":  response.StatusCode,
			"baseURLHost": hostOf(p.baseURL),
		},
	}

	location := strings.TrimSpace(response.Header.Get("Location"))
	if isHTTP3xx(response.StatusCode) && location != "" {
		result.Result = provider.ResultSuccess
		result.PayParams = map[string]string{paramH5URL: location}
		// 摘要里只记跳转目标的**主机名**，不记整条 URL：它带着渠道侧的会话标识（真凭据联调时
		// 会看到），而这一栏是要落库的。
		result.ResponseSummary["h5Host"] = hostOf(location)
		return result
	}

	// 走到这里就不是成功。报文（如果有）只用来取渠道的错误码与说明——H5 的失败应答形状没有
	// 任何参照（老系统没有这条路、文档也没写），所以这里只做一次**尽力而为**的解码，读不到
	// 就什么也不记，绝不因此改判据。
	describeH5Failure(response.Body, &result)

	if response.StatusCode >= 400 {
		// 4xx 与 5xx 分开：前一个是渠道**明确拒了**这次调用（它没有建单），后一个是对面自己出
		// 了故障（它建没建单我们不知道）。失败码用状态码本身，与小程序那条同源——排查时先看
		// 的就是它。
		result.FailureCode = fmt.Sprintf("HTTP_%d", response.StatusCode)
		if result.FailureMessage == "" {
			result.FailureMessage = fmt.Sprintf("provider returned http %d", response.StatusCode)
		}
		if isHTTP4xx(response.StatusCode) {
			result.Result = provider.ResultFailed
		}
		return result
	}
	// 剩下的就是 2xx 或 3xx 却没有 Location：**渠道受理了，但没告诉我们收银台在哪儿**。
	result.FailureCode = "H5_UNEXPECTED_RESPONSE"
	if result.FailureMessage == "" {
		result.FailureMessage = fmt.Sprintf(
			"provider returned http %d without a Location (expected a 302 to the cashier)", response.StatusCode)
	}
	return result
}

// describeH5Failure 尽力从失败应答里取出渠道的错误码与说明，写进摘要。
//
// **它不参与判成败**：读不出来就什么也不写（见 interpretH5Create 里那一段）。上限 200 字符是
// 因为有 H5 渠道在失败时回一整页 HTML，而这一栏要落库——把 64 KiB 的 HTML 写进
// payment_provider_calls.response_summary 是在用一条诊断信息换一次磁盘事故。
//
// 只在**确实是 JSON**时才取值：报文不是 JSON（HTML、空体）时一律当没有，不去里面猜。
func describeH5Failure(body []byte, result *provider.CreateResult) {
	var response createResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return
	}
	if code := strings.TrimSpace(response.ErrCode); code != "" {
		result.ResponseSummary["providerCode"] = code
	}
	if message := response.providerMessage(); message != "" {
		result.FailureMessage = truncate(message, failureMessageLimit)
		result.ResponseSummary["providerMessage"] = truncate(message, failureMessageLimit)
	}
}

// failureMessageLimit 是写进流水/错误串的渠道说明文字上限。见 describeH5Failure。
const failureMessageLimit = 200

// truncate 把一段文本截到 n 个字符（按 rune 截，不把一个汉字劈成两半）。
func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// hostOf 取一条 URL 的主机名，取不出来返回空串。它只用于写摘要，所以不报错。
func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// interpretCreate 把渠道的下单应答翻成 provider.CreateResult。
//
// # 判据是两层，别把它们混起来
//
//	HTTP 2xx + errCode == SUCCESS   → 这次**调用**它收下了并给了答复
//	miniPayRequest                  → 客户端能不能真的把支付拉起来
//
// 老系统把非 2xx 与 errCode != SUCCESS 分在两处判，但都用 Ambiguous 一个 bool 收口（:170、:203）。
// 这里保留那个两分：**含糊码不能当失败**。
//
// # 含糊码（老系统 UMSPayError.Ambiguous）
//
//	errCode == ""（应答里压根没有这个字段，等于我们没读懂）
//	errCode == "DUP_ORDER"        （同一单号重复下单）
//	errCode == "ORDER_PROCESSING" （渠道还在处理上一笔）
//
// 三种一律 ResultUnknown。它们**都不能当失败**：渠道可能已经建单了，标失败会让用户去付第二遍，
// 而第一笔若成了就是重复收款。老系统对这三种也是「不能直接用同一订单号重试」。
//
// 其余非 SUCCESS 的 errCode（商户号不存在、签名错误、参数不合法）是渠道明确拒了这次调用，
// 它没有建单——记 ResultFailed，用户可以换一种方式付。
//
// # errCode == SUCCESS 之后还要看 miniPayRequest
//
// 缺失、不是对象、六个键里少一个非空字符串 → 同样是 ResultUnknown（老系统也判 Ambiguous，
// 理由同上：errCode 说成功意味着渠道**可能已经收了单**，只是没给我们能用的支付参数）。
func (p *Protocol) interpretCreate(body []byte, status, attempts int, req provider.CreateRequest) provider.CreateResult {
	result := provider.CreateResult{
		Result:     provider.ResultUnknown,
		HTTPStatus: status,
		Attempts:   attempts,
		ResponseSummary: map[string]any{
			"httpStatus": status,
			// 渠道地址的**主机名**进摘要，是为了让「沙箱单打到了生产渠道上」这类事故在流水里
			// 看得见。这一格以前叫 mode（沙箱/生产是渠道行上的一列），那一列随渠道表一起没了
			// ——今天「打的是哪一边」这件事完全由 baseURL 决定（联调时把它指到假服务端，见
			// .env.example 里 PAYMENT_UMS_BASE_URL 那一段），所以摘要里该记的也是它。
			//
			// 只记主机名不记整条地址：路径部分带着这一版的接口路径，而地址整体是部署事实，
			// 流水这一栏该回答的是「打到了哪儿」，不是「我们部署在哪儿」。
			"baseURLHost": hostOf(p.baseURL),
		},
	}

	var response createResponse
	if err := json.Unmarshal(body, &response); err != nil {
		// 报文读不懂。**不能当失败**：对面可能已经建了单，只是回了一句我们解不开的话。
		result.FailureCode = "UNPARSEABLE_RESPONSE"
		result.FailureMessage = "provider response is not json"
		if isHTTP4xx(status) {
			// 4xx 是例外：渠道在协议层就拒了这次请求（报文不合法、签名不对、商户号不认识），
			// 它没有建单。这个分支有用——配置写错时用户能立刻换一种方式付。
			result.Result = provider.ResultFailed
		}
		return result
	}
	result.ResponseSummary["providerCode"] = response.ErrCode
	if message := response.providerMessage(); message != "" {
		result.ResponseSummary["providerMessage"] = message
	}

	if !isHTTP2xx(status) {
		// HTTP 层就没过。状态码先于报文：一个 502 配一份写着 SUCCESS 的报文是自相矛盾的东西。
		result.FailureCode = fmt.Sprintf("HTTP_%d", status)
		result.FailureMessage = errMessage(response.providerMessage(), fmt.Sprintf("provider returned http %d", status))
		if isHTTP4xx(status) {
			result.Result = provider.ResultFailed
		}
		return result
	}

	if response.ErrCode != errCodeSuccess {
		if ambiguousErrCode(response.ErrCode) {
			// 见上面「含糊码」那一段：渠道可能已经建单了。
			result.FailureCode = "AMBIGUOUS_PROVIDER_CODE"
			result.FailureMessage = errMessage(response.providerMessage(), "provider did not confirm the order")
			return result
		}
		result.Result = provider.ResultFailed
		result.FailureCode = "provider_declined"
		result.FailureMessage = errMessage(response.providerMessage(), "provider declined the order")
		return result
	}

	payParams, ok := validMiniPayRequest(response.MiniPayRequest)
	if !ok {
		// errCode 说成功、却没有能用的支付参数：钱可能已经在渠道那边挂了单，而客户端什么都
		// 拉不起来。**不能当失败**——那会让用户去付第二遍。
		result.FailureCode = "INVALID_MINI_PAY_REQUEST"
		result.FailureMessage = "provider returned no usable miniPayRequest"
		return result
	}
	result.Result = provider.ResultSuccess
	result.PayParams = payParams
	return result
}

// ambiguousErrCode 判断一个 errCode 是不是「不能当失败」的那三个。
//
// 单独一个函数而不是内联：这个判断会被读成「渠道拒了没有」，而它真正的含义是「我们不知道
// 渠道建单了没有」。写成一处的名字，是为了让下一次有人想往里加一个码时先停下来想一遍。
func ambiguousErrCode(errCode string) bool {
	switch strings.TrimSpace(errCode) {
	case "":
		// 应答里没有 errCode。老系统把它算进 Ambiguous（:197），这里照抄：没有判据字段
		// 不等于「渠道说失败」，只等于「这份应答我们没读懂」。
		return true
	case "DUP_ORDER", "ORDER_PROCESSING":
		return true
	}
	return false
}

// validMiniPayRequest 校验并取出小程序拉起支付所需的六个键，逐字照抄老系统
// ValidMiniPayRequest:215 的判据（六个键都是非空字符串）。
//
// 返回一个新的 map，只装这六个键：渠道若在 miniPayRequest 里多塞了字段，那些字段**不该**
// 跟着进客户端（service 会把这个 map 原样合并进回给客户端的支付参数）。
//
// 判据里**不 TrimSpace**：`" "` 在老系统那边算「非空」而在这里被判不合法，会让我们把一个
// 渠道确实给了的值当成没给。照抄原判据，宁可放行一个空白值让它去客户端那边失败得明明白白。
//
// 入参是**还没解码的**那块 json（见 createResponse.MiniPayRequest）：对象解不开与六个键不齐
// 在这里是同一种结局（unknown + INVALID_MINI_PAY_REQUEST），但理由不同，所以解不开时直接
// 返回 false 而不是硬塞一个空 map 进去。
func validMiniPayRequest(raw json.RawMessage) (map[string]string, bool) {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false
	}
	if len(object) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(miniPayKeys))
	for _, key := range miniPayKeys {
		value, ok := object[key].(string)
		if !ok || value == "" {
			return nil, false
		}
		out[key] = value
	}
	return out, true
}

// errMessage 取渠道给的说明文字，空则用兜底。
//
// 渠道的错误说明文字进错误串是可以的：它是渠道给运营看的话，不会含我们的密钥（密钥是我们
// 发出去的，不是它回过来的）。
func errMessage(message, fallback string) string {
	if text := strings.TrimSpace(message); text != "" {
		return text
	}
	return fallback
}

// transportFailure 把一次出网失败翻成 provider.CreateResult。
//
// 逐字复用 hmacbody / form_md5 那条判断（见那两个包里的同名方法）：同一句「请求失败了」下面
// 藏着两种后果完全不同的情况。
//
//   - 没发出去（ErrNotSent）：我们自己的问题，支付单**不该**被推成失败。
//   - 发出去了、没等到应答：**钱可能已经收了**。重试会再建一张单，标失败会让用户付过的钱
//     对不上账。唯一正确的动作是等回调，或者拿商户单号去查单。
func (p *Protocol) transportFailure(req provider.CreateRequest, err error) (provider.CreateResult, error) {
	attempts := 1
	var httpErr *httpx.Error
	// **判据是「能不能证明没发出去」，不是「错的是哪一类」**：只有 httpx 明确标了 NotSent
	// 的那条路才算没发出去。
	notSent := !errors.As(err, &httpErr) || httpErr.NotSent
	if errors.As(err, &httpErr) {
		attempts = httpErr.Attempts
	}
	if notSent {
		return provider.CreateResult{
			Result:          provider.ResultUnknown,
			Attempts:        attempts,
			FailureCode:     "NOT_SENT",
			FailureMessage:  err.Error(),
			ResponseSummary: map[string]any{"baseURLHost": hostOf(p.baseURL)},
		}, fmt.Errorf("channel %q: create request was not sent: %w", req.Method.ChannelCode, err)
	}
	return provider.CreateResult{
		Result:          provider.ResultTimeout,
		Attempts:        attempts,
		FailureCode:     "TIMEOUT",
		FailureMessage:  err.Error(),
		ResponseSummary: map[string]any{"baseURLHost": hostOf(p.baseURL)},
	}, nil
}
