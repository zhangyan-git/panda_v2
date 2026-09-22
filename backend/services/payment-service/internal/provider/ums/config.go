package ums

import (
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 渠道侧的默认值，全部照抄老系统写死的那几处。
//
// 老系统把它们写死在代码里（UnifiedOrder:108 的 instMid、QueryOrder:266 的路径、
// BuildUMSPayRequest 里的 TradeType），照抄而不是自己定一套：这些是**渠道侧的约定**，
// 换一个值就是另一个接口，而我们没有渠道文档可对照，唯一的证据就是那几行代码。
const (
	// defaultBaseURL 是银联商务全民付的**生产**根地址。测试环境另有一个 IP 端口的地址
	// （见渠道文档），联调时把 PAYMENT_UMS_BASE_URL 指过去、或者指到本地假服务端。
	//
	// 它属于**适配器**而不是部署配置：一个协议的默认网关地址是这份协议的常识，不是这家公司
	// 的部署事实。放这里也意味着 `.env` 里留空是一个**安全的**选择——不填就是生产，
	// 而不是「什么都没配，发到空地址去」。
	defaultBaseURL    = "https://api-mop.chinaums.com"
	defaultCreatePath = "/v1/netpay/wx/unified-order"
	defaultQueryPath  = "/v1/netpay/query"
	// defaultInstMid 是机构商户号。老系统两个副本（下单、退款）都写死 "MINIDEFAULT"。
	//
	// 它可配而不是钉死：微信小程序以外的渠道形态（比如主扫/被扫）用另一个 instMid，
	// 而 payment_methods.params 能覆盖它（见 createOptions）。
	defaultInstMid = "MINIDEFAULT"
	// defaultTradeType 是微信小程序支付的取值，老系统 BuildUMSPayRequest:1839 写死 "MINI"。
	defaultTradeType = "MINI"
	// defaultH5InstMid 是 H5 那条路上的机构商户号。渠道文档《全民付移动支付 H5支付》的业务内容
	// 表里写死 H5DEFAULT——它与小程序那条的 MINIDEFAULT 是**两个不同的取值**，报错一个是
	// 「业务类型不对」。
	//
	// 它不作为 config 里那一项（instMid）的默认值：一个渠道行同时挂小程序与 H5 两条支付方式时，
	// 渠道 config 上只能填一个值。所以这一项**按 action 选**（见 options），要改走 params.instMid。
	defaultH5InstMid = "H5DEFAULT"
)

// 可以被 payment_methods.params 覆盖的键名。
//
// 用常量而不是散在代码里的字符串字面量：这些名字会出现在**运营填的 JSON** 与**我们的代码**
// 两处，写错一个的表现是「配置配了但不生效」——静默回落到默认值，而默认值对微信小程序恰好
// 是对的，于是错到第二次接支付宝才暴露。
const (
	paramTradeType  = "tradeType"
	paramCreatePath = "createPath"
	paramInstMid    = "instMid"
	// 下面三个是**微信 H5** 那一条路上的必填项，见 Protocol.sceneType。
	paramSceneType  = "sceneType"
	paramMerAppName = "merAppName"
	paramMerAppID   = "merAppId"
)

// defaultOpenIDTradeTypes 是「渠道必须拿到 openid 才能下单」的 tradeType 默认集合。
//
// 默认给 MINI 与 JSAPI：这两个是微信小程序支付的取值，而微信的预支付单必须指定付款人。
// ALIPAY / 云闪付不在其中——它们各自要的不是 openid（支付宝要 buyer_id，云闪付多数场景连
// 用户标识都不需要），硬要求它们带 openid 会让那两条支付方式一条都发不出去。
//
// 它是**可配**的：渠道形态变了（比如换成服务商模式的子商户小程序），生效的值可能不同，
// 而改一个配置比发一次版便宜。
var defaultOpenIDTradeTypes = []string{"MINI", "JSAPI"}

// Protocol 是一份解析好、校验过的协议声明。
//
// 与 form_md5 不同，这里的声明**不是「一族渠道的骨架」**：它是一条具体协议（银联商务全民付）
// 的参数表。报文的形状、签名的拼法、成功判据都是写死的，配置里只有地址、商户号与那几个
// 渠道方给的标识。把一条钉死的协议拆成一堆可配的旋钮，只会让「配错了」看上去像「协议变了」。
type Protocol struct {
	// baseURL 是渠道的根地址（默认见 defaultBaseURL），**不带路径前缀**——这一族的接口
	// 路径是绝对的 /v1/netpay/…，所以 url 只是把结尾的斜杠抹掉再拼。
	//
	// 它在这次收口里升了一级：从前「沙箱与生产」是渠道行上的一个 status，现在就是这一个
	// 值——打的是哪一边，看地址。流水摘要里记的也是它的主机名（见 interpretCreate）。
	baseURL string
	// appID 与 secretRef 是配套的：appID 进待签串与每一条报文，**不是秘密**（它在
	// Authorization 头里明文发出去），所以住在 config 里；appKey 走凭据槽。
	appID     string
	secretRef string
	// commSecretRef 是**第二把凭据**（通讯密钥）的槽名，**入站**验签用它，可选。
	//
	// 这一族有两把密钥，方向不同：
	//
	//   - appKey（secretRef）—— 出站：我们算签名给渠道验。下单、查单、退款都走它；
	//   - 通讯密钥（commSecretRef）—— 入站：渠道算签名给我们验。H5 的支付结果通知与结果页
	//     回跳走它（见 keyappend.go）。
	//
	// 后者**可选**而不是必填：一条只跑小程序那条路的渠道根本收不到 key 拼接签名的报文
	// （它的通知带 Authorization 头，走 appKey），逼它填一把用不上的密钥，只会让运营在
	// 「这里要填什么」上卡住。而**需要它的那条路是 fail closed 的**：槽名没配或槽是空的，
	// H5 的回调一律拒收并报 ErrSecretNotConfigured（见 commSecret），绝不降级成跳过验签。
	commSecretRef string
	// mid / tid 是渠道侧的商户号与终端号。
	mid string
	tid string
	// sourceCode 与 domainName 是**渠道侧登记值**：老系统硬编码了另一个主体的
	// （"3CYM" 与 "www.bjqtkjyxgs.com"），那是别家商户在银联商务登记的信息。
	//
	// **不给默认值**：给了默认值的话，一家还没登记的渠道会拿着别人的来源编号把报文发出去，
	// 而渠道那边的表现可能是受理成功——钱进了别人的商户号。所以这两个必须由运营填。
	//
	// # sourceCode 还管着商户订单号的**形状**（2026-09-21 实测）
	//
	// 这个账户要求 merOrderId 以 sourceCode 开头：拿裸支付单号打 H5 时，银联商务的收银台直接
	// 回「无效订单号，订单号必须以3CYM开头」。所以它不只是报文里的一个字段，还是我们生成渠道
	// 单号时的前缀（见 orderid.go）。**换一个 sourceCode 就等于换一种单号写法**，而历史支付单
	// 仍带着旧前缀——退款、关单、查单都按当时那个号去认，所以这个值不能随手改。
	//
	// 顺带记一笔：这里说「3CYM 是别家主体的」，与我们实测的这个账户要 3CYM 前缀**不矛盾**——
	// 现行配置（.env）把它填成 3CYM，账户也认它。到底是我们沿用了那个主体的登记、还是这个号
	// 本来就通用，没有证据，也不影响代码：一切照配置走。
	sourceCode string
	domainName string
	// createPath / queryPath 是接口路径。都给了默认值（老系统写死的那两个），而且可以由
	// payment_methods.params 覆盖 createPath——一个渠道下的三条支付方式可能落在不同的
	// 下单接口上（微信小程序走 wx/unified-order，支付宝走另一个）。
	createPath string
	queryPath  string
	// refundPath / refundQueryPath / closePath 是**对已有支付单的三条操作接口**的路径，
	// 默认值见 operator.go 那组常量。
	//
	// 与前两条一样可配，理由却不同：那两条是「一个渠道可能落在不同接口上」（微信小程序与支付宝
	// 的下单路径不同），这三条是「同一件事故在渠道那边有过两条路径」。退款查询尤其如此——
	// 规范与官方样例是 `/v1/netpay/refund-query`，而老系统打的是
	// `/v1/netpay/trade/refund/query`。默认跟规范走，另一条留成可配的值。
	refundPath      string
	refundQueryPath string
	closePath       string
	// instMid / tradeType 是「一个支付方式一个值」的那两项，默认值照抄老系统；支付方式可以用
	// params 覆盖它们（见 options）。
	instMid   string
	tradeType string
	// returnURL 是支付完成后跳回的地址，可选。
	//
	// 它进报文（老系统无条件发这个键，值来自 PaymentRequest.ReturnURL），但**不影响我们这边
	// 的结算**：钱到没到只看回调与查单。所以它是纯展示参数，没配就发空串。
	returnURL string
	// sceneType / merAppName / merAppID 是**微信 H5** 那一条路上的三项，渠道文档标为必填：
	// 应用类型（IOS_SDK / AND_SDK / IOS_WAP / AND_WAP）、应用名称、应用标识（苹果传 bundle id、
	// 安卓传包名、手机网站传首页 URL）。支付宝 H5 与云闪付那两条不需要它们——文档对 merAppId
	// 明确写着「支付宝H5支付参数无效」。
	//
	// 与 tradeType 一样是「一个支付方式一个值」，可以被 params 覆盖：同一个渠道行下挂微信 H5
	// 与支付宝 H5 两条支付方式时，只有前者该带这三项。
	sceneType  string
	merAppName string
	merAppID   string
	// openIDTradeTypes 是「必须带 openid」的 tradeType 集合，见 requiresOpenID。
	openIDTradeTypes []string
}

// Parse 解析并校验一份渠道 config。
//
// 所有错误都包着 provider.ErrConfigInvalid，并且**路径是完整的**（`config.sign.secretRef`）：
// 配置是一棵树，只说「secretRef 没填」会让人去别的段里找。
func Parse(config map[string]any) (*Protocol, error) {
	cfg := provider.Config(config)

	// baseURL 是**唯一一个不填就用默认值**的账户值：它有一个正确的取值（生产地址），
	// 而「没填」与「填了生产地址」在行为上完全一样——所以要求部署把它抄一遍只是多一次
	// 抄错的机会。测试地址（沙箱、本地假服务端）反过来是**必须显式填**的那一个，那正是
	// 我们要的：不填就打到生产，填了就是有意为之，`.env` 里看得见。
	//
	// 其余五项（appId / mid / tid / sourceCode / domainName）没有默认值可给：它们是**这家
	// 公司在渠道侧登记的标识**，凭空编一个等于替别家商户发报文。
	baseURL := textOr(cfg.String("baseURL"), defaultBaseURL)
	appID, err := cfg.RequireString("appId")
	if err != nil {
		return nil, err
	}
	// mid / tid 必填而不是可选：它们进每一条报文，缺一个的表现是渠道回一句「商户号不存在」
	// 或者「终端号非法」，而那时你翻遍配置也看不出少的是哪一个——错误串在这里就点它的名。
	mid, err := cfg.RequireString("mid")
	if err != nil {
		return nil, err
	}
	tid, err := cfg.RequireString("tid")
	if err != nil {
		return nil, err
	}
	// sourceCode / domainName 必填，理由见 Protocol 上那一段（给了默认值就是替别家商户发报文）。
	sourceCode, err := cfg.RequireString("sourceCode")
	if err != nil {
		return nil, err
	}
	domainName, err := cfg.RequireString("domainName")
	if err != nil {
		return nil, err
	}
	// secretRef 必填：这个协议族的每一条报文都要签名，没有「不需要密钥」的形态。
	//
	// 它今天**总是**由装配处写死成 catalog 里那个常量（`appKey`）——运营不再有机会填它，
	// 也就不再有机会填错。仍然从配置里读、仍然必填，是因为这一族的第二条协议将来可能声明
	// 另一个槽名，而适配器不该假定只有一个。
	secretRef, err := cfg.RequireString("sign.secretRef")
	if err != nil {
		return nil, err
	}

	openIDTradeTypes := textListOr(cfg.StringList("openIdTradeTypes"), defaultOpenIDTradeTypes)

	return &Protocol{
		baseURL:   baseURL,
		appID:     appID,
		secretRef: secretRef,
		// 第二把凭据可选，理由见 Protocol.commSecretRef。**不填就不报错**：只跑小程序那条路
		// 的渠道用不上它，而拿它做必填会把一条能用的渠道变成配不出来的渠道。
		commSecretRef: strings.TrimSpace(cfg.String("sign.commSecretRef")),
		mid:           mid,
		tid:           tid,
		sourceCode:    sourceCode,
		domainName:    domainName,
		createPath:    textOr(cfg.String("endpoints.create"), defaultCreatePath),
		queryPath:     textOr(cfg.String("endpoints.query"), defaultQueryPath),
		// 三条操作接口的路径都不由 payment_methods.params 覆盖：它们是**渠道级**的事实
		// （同一家机构的两条协议共用同一批操作接口），而 params 是支付方式级的东西。
		// 只有下单路径按支付方式分岔，因为那是四条真正不同的业务接口。
		refundPath:      textOr(cfg.String("endpoints.refund"), defaultRefundPath),
		refundQueryPath: textOr(cfg.String("endpoints.refundQuery"), defaultRefundQueryPath),
		closePath:       textOr(cfg.String("endpoints.close"), defaultClosePath),
		instMid:         textOr(cfg.String("instMid"), defaultInstMid),
		tradeType:       textOr(cfg.String("tradeType"), defaultTradeType),
		returnURL:       strings.TrimSpace(cfg.String("returnUrl")),
		// 这三项不填就是空串：它们只有微信 H5 那一条路要，而「没配」与「这条渠道不跑微信 H5」
		// 在我们这边是同一件事。空值在报文里被省略（见 h5CreateBody），渠道那边少了必填项时
		// 会自己拒——比我们本地凭空拼一个默认应用名发过去诚实。
		sceneType:        strings.TrimSpace(cfg.String("sceneType")),
		merAppName:       strings.TrimSpace(cfg.String("merAppName")),
		merAppID:         strings.TrimSpace(cfg.String("merAppId")),
		openIDTradeTypes: openIDTradeTypes,
	}, nil
}

// url 拼出真实的请求地址。
//
// 照抄老系统的 `s.BaseURL + "/v1/netpay/…"`，只是把结尾的斜杠抹掉：运营在后台填地址时带不带
// 结尾斜杠是不确定的，两边都带会拼出 `//v1/netpay/…`，有些网关把它当另一个路径、直接 404。
//
// path 必须带前导斜杠（config 里那一栏的格式，默认值就是这么写的）。
func (p *Protocol) url(path string) string {
	return strings.TrimRight(p.baseURL, "/") + path
}

// createOptions 是一次下单真正要用的那几个「一个支付方式一个值」的参数。
//
// 单独一个结构体而不是三个参数：它们必须**一起**生效，分开传的写法迟早会出现「调用方传了
// tradeType 忘了传 instMid」，而那两种报文渠道都不会明确拒——它只会以某一种方式受理，
// 于是错的那一笔静静地走了另一条通道。
type createOptions struct {
	createPath string
	instMid    string
	tradeType  string
	// 下面三个只有 action='h5' 那条路会用，见 h5CreateBody。
	sceneType  string
	merAppName string
	merAppID   string
}

// options 把 config 里的默认值与 payment_methods.params 里的覆盖合起来。
//
// **params 优先**：那三个值描述的是「这条支付方式走哪个接口、以什么形态走」，而支付方式是
// 运营为每一次收款挑的那个东西（用户点的是「支付宝」还是「微信」）。config 上的默认值是
// 「这个渠道大多数情况怎么走」，params 上的才是这一次的事实。
//
// 读 params 是**下单路径专属**的：回调路径上 Method.Params 是空的（我们不知道用户当初选的
// 是哪条支付方式，见 provider.NotificationRequest），所以这一族的下单路径与回调路径读的
// 东西不同——回调只读 config，这正好也是它能在验签之前就算出签名对不对的原因。
//
// 空值等于没配：`params["tradeType"] == ""` 与「没这个键」在运营眼里是同一件事。
//
// # action 决定的是**默认值那一层**
//
// 小程序与 H5 是同一渠道下的两种协议，它们的默认值不同（instMid 一个是 MINIDEFAULT、一个是
// H5DEFAULT），而 createPath 在 H5 上没有默认值：那四条路径（支付宝 / 微信 / 云闪付 / 微信转
// 小程序）各自是一个接口，「默认走哪一条」这个问题没有合理答案，随手挑一条的后果是把一笔
// 支付宝的支付打到微信的接口上——渠道回一句与本意无关的错，而配置看上去全都填了。
//
// 所以 H5 那一支把 createPath 清空，由 createH5 本地拒（见那里的说明）。**params 仍然最优先**，
// 两种 action 下都是。
func (p *Protocol) options(params map[string]string, action provider.Action) createOptions {
	options := createOptions{
		createPath: p.createPath,
		instMid:    p.instMid,
		tradeType:  p.tradeType,
		sceneType:  p.sceneType,
		merAppName: p.merAppName,
		merAppID:   p.merAppID,
	}
	if action == provider.ActionH5 {
		options.createPath = ""
		options.instMid = defaultH5InstMid
		// H5 的业务内容里**没有 tradeType 这一项**（渠道文档的报文表里查无此键），带上它等于
		// 发一个渠道不认识的参数。留空比带一个看上去合理的默认值诚实。
		options.tradeType = ""
	}
	if value := strings.TrimSpace(params[paramTradeType]); value != "" {
		options.tradeType = value
	}
	if value := strings.TrimSpace(params[paramCreatePath]); value != "" {
		options.createPath = value
	}
	if value := strings.TrimSpace(params[paramInstMid]); value != "" {
		options.instMid = value
	}
	if value := strings.TrimSpace(params[paramSceneType]); value != "" {
		options.sceneType = value
	}
	if value := strings.TrimSpace(params[paramMerAppName]); value != "" {
		options.merAppName = value
	}
	if value := strings.TrimSpace(params[paramMerAppID]); value != "" {
		options.merAppID = value
	}
	return options
}

// requiresOpenID 判断一个**生效的** tradeType 是不是必须带 openid。
//
// 比的是生效值（params 覆盖之后）而不是 config 默认值：一条把 tradeType 配成 ALIPAY 的支付
// 方式不该因为渠道默认是 MINI 就被要求带 openid。
//
// 用 EqualFold 而不是相等：这个值在 params 与 config 两处都由人手填，`mini` 与 `MINI` 在
// 填的人看来是同一件事，而判错方向是不安全的——漏判会让我们发一份没有 openId 的报文过去，
// 渠道要么拒（用户看到一个说不清的错），要么更糟：把它当成某个默认用户。
func (p *Protocol) requiresOpenID(tradeType string) bool {
	for _, candidate := range p.openIDTradeTypes {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(tradeType)) {
			return true
		}
	}
	return false
}

// textOr 取一个配置里的文本，空则用默认值。
func textOr(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

// textListOr 取一个配置里的列表，空则用默认值。
//
// 返回的是**副本**：默认值那个切片是包级的，直接交出去意味着调用方（或将来某个测试）改一个
// 元素就改了全进程的默认值，而那种错误表现为「某个渠道莫名其妙开始要求 openid」。
func textListOr(value, fallback []string) []string {
	if len(value) > 0 {
		return value
	}
	return append([]string(nil), fallback...)
}
