// Package catalog 是这份代码里认识的**全部**支付方式与渠道。
//
// # 为什么是一张表，不是两张数据库表
//
// 早先支付方式是数据：`payment_methods` 一行一个方式、`payment_channels` 一行一个渠道，
// 渠道那行还挂着一份 15–30 个键的协议声明（签名算法、字段大小写、成功码、时间戳格式），
// 由运营在后台一栏一栏地填。那个形状是为**同时接五家第三方渠道**准备的：丰选万联、优联、
// 首创饭卡、北方工业饭卡、家园消费金各有自己的一套收单协议，把差异做成配置才不用为每一家
// 发一次版。
//
// 这个仓库今天只有四种支付方式，其中三家方根本不是「第三方渠道」：
//
//	咖啡豆支付        钱在我们自己的账户库里（account-service 的豆账本），没有第三方
//	银联商务—小程序    同一个商户号下的第一条协议（POST + JSON + OPEN-BODY-SIG）
//	银联商务—H5        同一个商户号下的第二条协议（GET + 参数全拼 + OPEN-FORM-PARAM）
//	取货码            钱在这台设备的咖啡余额里，也不经过支付服务（见下面那段）
//
// 前三条里有两条共用同一个商户号、同一把密钥、同一套账户值——它们不是运营数据，是代码；
// 剩下那一条压根没有第三方。所以「渠道 + 协议族 + 配置键」那一摊被收成了这张表。
//
// # 六个 code，三个方式
//
// （用户能选的支付方式一共四种，第四种取货码**不在这张表里**，理由见下面那段。）
//
// 银联商务的 H5 不是一条接口，是**四条下单路径**（支付宝 / 微信 / 云闪付 / 微信转小程序），
// 每条路径都要写进报文的 createPath，而且没有合理的默认值——随手挑一条当默认，会把一笔
// 支付宝的支付打到微信的接口上，渠道回一句与本意无关的错，而配置看上去全都填了。
// 所以 H5 在收银台上是四个选项、四个 code，但它们共用同一条渠道。
//
// # 取货码为什么**不在**这张表里
//
// 取货码（厂商回调 POST /v1/openapi/device/pickup）走的是刷卡机那条路的形状：钱不在渠道
// 那儿，扣成功就是成功，没有 pending 可等，订单直接落 paid。那条路今天就不经过支付服务
// ——它由 partner-service 验签、order-service 建单，见 order-service 的
// internal/service/create_pickup.go（刷卡机那条同形状的路在同一个目录的 create_device.go）。
// 它进的是 `orders.payment_method` 的词表，不是这里。
//
// # 第七个 code 也不在这张表里：wechat_papay
//
// 微信**直连**委托代扣（连续包月的签约）是一条协议通道，不是收单通道——选中它不会有任何
// 一笔钱进来，`Create` 会明确拒绝（见 wechatpay 包）。它进目录是因为签约也要「渠道 + 配置 +
// 动作」这样一条记录，而那张表就是这里。它**不是第七个支付方式**：用户永远选不到它，
// order-service 也永远不会传这个 code。
//
// 写在同一个文件里而不是另起一张表，是因为它与上面那几条共用同一套东西：同一个 Registry、
// 同一套 SecretEnv 解析、同一个 Method 形状。分开的代价是这套装配逻辑要有第二份实现，
// 而两份实现必然有一处会走偏。
//
// # 将来再加一条第三方渠道
//
// 这一步是**刻意留着的**：新增一条优联式的渠道 = 写一个实现 provider.Provider 的包 +
// 在下面加一个 Channel + 加几条 Method + 在 .env 里加一组变量。不需要恢复那两张表，
// 也不需要恢复后台的配置页。
package catalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/ums"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/wechatpay"
)

var (
	// ErrMethodNotFound：客户端传了一个目录里没有的 code。
	ErrMethodNotFound = errors.New("payment method is not found")
	// ErrChannelNotFound：一条支付方式指向的渠道不在目录里。
	ErrChannelNotFound = errors.New("payment channel is not found")
	// ErrChannelIncomplete：渠道在目录里，但它这次部署没配齐（见 Channel.MissingEnv）。
	ErrChannelIncomplete = errors.New("payment channel is missing required configuration")
)

// 两个凭据槽的名字。它们是**适配器自己声明的**（`sign.secretRef` / `sign.commSecretRef`
// 的取值），由这里在拼配置树时写死，不再由运营填——让槽名可配只会多一种「配置里写的名字
// 查无此槽」的错配。
const (
	slotAppKey  = "appKey"
	slotCommKey = "commKey"
	// slotAPIv2Key 是微信直连委托代扣那把 APIv2 密钥（32 位）。**签名与验签共用它**：
	// 微信 APIv2 的回调就是用同一把密钥、按同一套算法签的，所以这一族只有一个槽
	// （见 wechatpay.Provider.SecretSlots）。
	slotAPIv2Key = "apiV2Key"
)

// 支付方式的 code。它们是**对外契约**的一部分，改一个值等于改一次客户端：
//
//   - order-service 发起支付时传的就是它（CreatePayment 的 paymentMethod）；
//   - 它落进 payments.payment_method，后台的支付单列表按它筛；
//   - 支付成功事件里带给 order-service，写进 orders.payment_method。
//
// 所以它们是**一次性契约**：改名等于改一次客户端，而且会让历史支付单与新支付单看起来是
// 两种东西——009 把老 `payment_methods.code` 原样搬进了 `payments.payment_method`，那一列
// 今天只在展示与筛选时用。老库里若有一个这里没有的 code（那套种子连目录一起删了，没法再
// 逐个核对），它不会报错，只会在后台列表上显示成空名字。
const (
	// CodeCoffeeBean 是咖啡豆支付：账户出资，钱从用户的豆账本走。
	CodeCoffeeBean = "coffee_bean"
	// CodeUMSMiniappWechat 是银联商务的小程序支付（微信）。
	CodeUMSMiniappWechat = "ums_miniapp_wechat"

	// 银联商务 H5 的四条下单路径，一条一个 code。
	CodeUMSH5Alipay        = "ums_h5_alipay"
	CodeUMSH5Wechat        = "ums_h5_wechat"
	CodeUMSH5Upqr          = "ums_h5_upqr"
	CodeUMSH5WechatMinipay = "ums_h5_wechat_minipay"

	// CodeWechatPapay 是微信**直连**委托代扣（连续包月的签约）。
	//
	// 它**不是一条支付方式**：选中它不会有任何一笔钱进来，Create 会明确拒绝
	// （见 wechatpay.Provider.Create）。它进目录是因为签约也要一条「渠道 + 配置 + 动作」的
	// 记录，而那张表就是目录——真到代扣落地时，出钱的仍然是这条通道（微信侧扣，不经过我们
	// 的收银台），所以它永远不会有 native_pay / h5 那几种形态。
	CodeWechatPapay = "wechat_papay"
)

// ChannelCodeUMS 是银联商务这条渠道的代码，也是回调路径里的那一段
// （/v1/payments/callback/ums）。
//
// 它与 provider 的注册名（ums.Name）今天相等，但**是两个字段**：渠道代码回答「钱从哪个
// 商户号来」，注册名回答「谁来发这份报文」。将来接一条同族渠道（同一个适配器、另一个
// 商户号）时，两个值就会分岔——而这正是回调路径不能用注册名当段的原因。
const ChannelCodeUMS = ums.Name

// ChannelCodeWeChatPay 是微信直连委托代扣那条通道的代码，也是回调路径里的那一段
// （签约通知落在 /v1/payments/agreement-notify/wechat_pay）。
//
// 与 ChannelCodeUMS 同理：今天它等于 provider 的注册名，但那是巧合，不是约束——同一份
// APIv2 报文格式可以服务第二个商户号，那时注册名还是 wechat_pay，渠道码得另起一个。
const ChannelCodeWeChatPay = wechatpay.Name

// Channel 是一条渠道：一个适配器 + 它这一次部署用的那份配置。
type Channel struct {
	// Code 是回调路径里的那一段。
	Code string
	// Name 是这个渠道给人看的名字（后台的支付单列表与详情按它显示「走的哪条渠道」）。
	//
	// 它从前是 payment_channels 里的一列，由运营起名。今天它跟着渠道一起写在代码里：一条
	// 渠道接不接是发版决定的，那么「它叫什么」也不该是另一次部署的输入。
	Name string
	// Provider 是适配器在注册表里的名字。
	Provider string
	// Config 是适配器 Parse 要吃的那棵树（地址、账户值、协议声明）。**不放凭据**。
	Config provider.Config
	// SecretEnv 是「槽名 → 环境变量名」。
	//
	// 注意它存的是**名字**，不是值：值在签名那一刻由装配处 os.Getenv 现读，与
	// PAYMENT_MANUAL_SECRET 走的是同一条路。凭据层的规矩是「明文只允许短暂存在于
	// service 层栈上」——把值放进这个结构体，等于让它跟着进程活一整个生命周期，而这个
	// 结构体会被日志打印。
	SecretEnv map[string]string
	// MissingEnv 是构造这条渠道时**缺的必填环境变量名**。非空表示这条渠道没配齐。
	//
	// 它不阻止服务启动（一个只跑咖啡豆支付的部署不该因为没接银联商务就起不来），
	// 但选中这条渠道下任何支付方式时会被明确拒掉，错误串里点名缺的是哪几个变量。
	MissingEnv []string
	// Settlement 表示这条渠道**能把钱分出去**：发起支付时随下单报文下发子单
	// （provider.DivisionInstruction），成功应答就是分账成功。
	//
	// 它是渠道自己的属性，所以写在渠道上，而不是在结算那边硬编码一串渠道名：加一条渠道的人
	// 在这一行里必须回答「它能不能分账」，回答错了的表现是配置出来的接收方永远分不到钱。
	// 今天只有银联商务为真——微信那条路是委托代扣签约，适配器里没有一处拼分账字段。
	Settlement bool
}

// Method 是一个支付方式。
type Method struct {
	// Code 见文件顶部那组常量。
	Code string
	// Name 是收银台上显示的名字。
	Name string
	// Description 是给客户端与订单详情看的一句话。
	Description string
	// Action 决定客户端怎么调起支付，取值见 provider.Action。
	//
	// **客户端只 switch 它、不 switch Code**：这是「新增一个同形态的支付方式不用改客户端」
	// 的全部依据。加第七个取值意味着要同步改客户端。
	Action provider.Action
	// Params 是这条路上「一个支付方式一个值」的那几项，合并给客户端、也喂给适配器。
	//
	// 今天只有 H5 那四条用它（createPath 与微信的三项应用标识）。**不放密钥**：这份
	// 参数会原样出现在发起支付的响应里（见 service.clientPayParams）。
	Params map[string]string
	// ChannelCode 是这条方式落在哪条渠道上。**空表示这条路上没有第三方**——今天只有
	// 咖啡豆支付是空的，它走 account-service 扣余额。
	ChannelCode string
}

// Catalog 是构造好的目录。它没有并发保护：构造只发生在装配处（cmd/main.go），
// 服务起来之后只读。
type Catalog struct {
	methods  map[string]Method
	order    []string
	channels map[string]*Channel
}

// Config 是构造目录要的部署配置。
//
// 它是一组**账户值**，不是密钥：这里一个 appKey 都没有。银联商务那两把密钥由装配处在
// 签名那一刻按变量名取（见 Channel.SecretEnv）。
type Config struct {
	// BaseURL 为空时由 ums 包回落到生产地址（那个默认值属于适配器，不属于部署配置）。
	UMSBaseURL    string
	UMSAppID      string
	UMSMID        string
	UMSTID        string
	UMSSourceCode string
	UMSDomainName string
	// 下面三项只影响微信 H5 那两条路，留空是合法的（见 platform/config 里 PaymentUMSSceneType
	// 的注释）。
	UMSSceneType  string
	UMSMerAppName string
	UMSMerAppID   string

	// WeChatPay* 是微信直连委托代扣那条通道的账户值与证书路径，全部来自 .env。
	//
	// **留空是合法的**：一个不卖连续包月的部署用不到这一族，与上面那组同一条理由。缺项会在
	// 选中这条通道时被点名（见 wechatPayChannel 的 missing）。
	//
	// CertPath / KeyPath 是**文件路径**，不是凭据：apiclient_cert.pem 与 apiclient_key.pem
	// 由微信商户平台下发，装进容器的是文件本身。它们不进 SecretEnv（那是给「一把字符串
	// 密钥」的），缺了只让需要证书的两条路拒绝，纯签约照样能跑。
	WeChatPayBaseURL  string
	WeChatPayAppID    string
	WeChatPayMchID    string
	WeChatPayCertPath string
	WeChatPayKeyPath  string
	// WeChatPaySignMiniProgramAppID 是跳转目标那个小程序的 appid（微信官方的签约小程序）。
	// 与 WeChatPayAppID 是两个不同的 id，见 platform/config 里那个字段的注释。
	WeChatPaySignMiniProgramAppID string
}

// FromConfig 用部署配置构造目录。
func FromConfig(cfg Config) *Catalog {
	channel := umsChannel(cfg)
	payChannel := wechatPayChannel(cfg)
	methods := []Method{
		{
			Code:        CodeCoffeeBean,
			Name:        "咖啡豆支付",
			Description: "用账户里的咖啡豆直接支付。",
			Action:      provider.ActionAccount,
			// ChannelCode 留空：这条路上没有第三方，钱在我们自己的账户库里。
		},
		{
			Code:        CodeUMSMiniappWechat,
			Name:        "微信小程序（银联商务）",
			Description: "银联商务全民付小程序支付，在小程序内直接调起微信支付。需要用户已绑定微信。",
			Action:      provider.ActionNativePay,
			// Params 空是**有意的**：这条路上那三个值（createPath / instMid / tradeType）
			// 都有唯一的正确取值，而且是 ums 包里的默认值（见 ums/config.go 顶部那组常量）。
			// H5 那四条**必须**写 createPath，是因为那边没有默认值——两者是同一个道理的两面：
			// 有唯一的正确取值就写进代码，多于一个才写成数据。
			ChannelCode: ChannelCodeUMS,
		},
		{
			Code:        CodeUMSH5Alipay,
			Name:        "支付宝（银联商务 H5）",
			Description: "银联商务全民付 H5 收银台，跳转到支付宝。",
			Action:      provider.ActionH5,
			Params: map[string]string{
				"createPath": "/v1/netpay/trade/h5-pay",
			},
			ChannelCode: ChannelCodeUMS,
		},
		{
			Code:        CodeUMSH5Wechat,
			Name:        "微信（银联商务 H5）",
			Description: "银联商务全民付 H5 收银台，跳转到微信支付。",
			Action:      provider.ActionH5,
			// 微信那两条路额外要三项**应用标识**（渠道文档标为必填）。它们不是渠道给的
			// 账户值，是这个应用自己的标识（苹果传 bundle id、安卓传包名、手机网站传首页
			// URL），所以从部署配置来、只挂在这两条方式上。不填时不带，由渠道自己拒——
			// 比我们本地凭空拼一个默认应用名发过去诚实。
			Params:      wechatH5Params("/v1/netpay/wxpay/h5-pay", cfg),
			ChannelCode: ChannelCodeUMS,
		},
		{
			Code:        CodeUMSH5Upqr,
			Name:        "云闪付（银联商务 H5）",
			Description: "银联商务全民付 H5 收银台，走银联云闪付。",
			Action:      provider.ActionH5,
			Params: map[string]string{
				"createPath": "/v1/netpay/uac/order",
			},
			ChannelCode: ChannelCodeUMS,
		},
		{
			Code:        CodeUMSH5WechatMinipay,
			Name:        "微信转小程序（银联商务 H5）",
			Description: "银联商务全民付 H5 收银台，在微信里转起小程序支付。",
			Action:      provider.ActionH5,
			Params:      wechatH5Params("/v1/netpay/wxpay/h5-to-minipay", cfg),
			ChannelCode: ChannelCodeUMS,
		},
		{
			Code:        CodeWechatPapay,
			Name:        "微信委托代扣（连续包月）",
			Description: "在微信里授权商户按月自动扣款，用于连续包月的会员套餐。签约不收款，扣款由商户按周期发起。",
			// jump_miniapp 是**唯一**贴得上这个动作的取值：整个流程是用户跳进微信官方的
			// 签约小程序点「同意并签约」，我们这边一行代码都不在用户的手机上跑。客户端只
			// switch Action，所以它不需要认识 wechat_papay 这个 code——这正是 action 这一层
			// 存在的理由（见 Method.Action 的注释）。
			Action:      provider.ActionJumpMiniapp,
			ChannelCode: ChannelCodeWeChatPay,
			// Params 空是**有意的**：这条通道没有「随方式走的参数」——跳转目标（微信官方
			// 签约小程序的 appid）是**渠道**的配置（WECHAT_PAY_SIGN_MINI_PROGRAM_APP_ID，
			// 见 channelWeChatPay 与 wechatpay.Parse），不是方式的参数。
			//
			// 它原先写死在这里的适配器里，理由是「微信侧的固定值、不存在第二个小程序」。
			// 取值确实只有一个，但那推不出它可以进源码：它是能定位到具体主体的标识，与商户号
			// 同一性质（GitHub 密钥扫描在 commit 4920ce71 上标了它）。
		},
	}

	built := &Catalog{
		methods: make(map[string]Method, len(methods)),
		order:   make([]string, 0, len(methods)),
		channels: map[string]*Channel{
			channel.Code:    channel,
			payChannel.Code: payChannel,
		},
	}
	for _, method := range methods {
		if _, exists := built.methods[method.Code]; exists {
			// 重复的 code 会让「用户点的是哪一个」变成一个不确定的事实，宁可在装配处就炸。
			panic(fmt.Sprintf("catalog: payment method %q is declared twice", method.Code))
		}
		built.methods[method.Code] = method
		built.order = append(built.order, method.Code)
	}
	return built
}

// Method 按 code 取一个支付方式。未知的 code 是**调用方的错**（客户端传了一个我们没实现
// 的方式，或者代码回滚到了不认新 code 的版本），不是「这个方式被停用了」。
func (c *Catalog) Method(code string) (Method, error) {
	if c == nil {
		return Method{}, fmt.Errorf("%w: catalog is not configured", ErrMethodNotFound)
	}
	method, ok := c.methods[strings.TrimSpace(code)]
	if !ok {
		return Method{}, fmt.Errorf("%w: %q", ErrMethodNotFound, code)
	}
	return method, nil
}

// 这里从前还有一个 Methods()（「给后台的筛选下拉用」）。那个下拉随支付方式与渠道那一页一起
// 删了，今天没有任何调用方——列全部支付方式的接口**不存在**，也别照旧名字加回来：谁要拿到
// 这六个 code（比如给收银台发一份「能选哪几种」），那是一件要先想清楚的事，不是补一个
// getter。

// SettlementChannels 列出**能把钱分出去**的渠道，按 Code 排序。
//
// 它与上面那个被删掉的 Methods() 不是一回事：那个要列的是「用户能选哪几种支付方式」，答案取决于
// 上线了什么；这个要列的是「分账能挂在哪条渠道上」，答案取决于**哪条渠道的下单报文里能带子单**
// ——是渠道自身的能力，已经写在 Channel.Settlement 上了。后台的分账账户表单拿它当渠道下拉的取值
// 来源，所以它只回能给用户看的三样，不回 Config 与 SecretEnv。
func (c *Catalog) SettlementChannels() []Channel {
	if c == nil {
		return nil
	}
	channels := make([]Channel, 0, len(c.channels))
	for _, channel := range c.channels {
		if channel.Settlement {
			channels = append(channels, *channel)
		}
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].Code < channels[j].Code })
	return channels
}

// Channel 按渠道代码取一条渠道。
//
// 渠道代码为空（账户出资那条路）返回 nil, nil：**没有渠道不是错误**，账户出资合法地没有
// 渠道可查。调用方据此分岔。
func (c *Catalog) Channel(code string) (*Channel, error) {
	if strings.TrimSpace(code) == "" {
		return nil, nil
	}
	if c == nil {
		return nil, fmt.Errorf("%w: catalog is not configured", ErrChannelNotFound)
	}
	channel, ok := c.channels[strings.TrimSpace(code)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrChannelNotFound, code)
	}
	return channel, nil
}

// ChannelFor 取一条支付方式落在哪条渠道上。
func (c *Catalog) ChannelFor(method Method) (*Channel, error) {
	return c.Channel(method.ChannelCode)
}

// umsChannel 把部署配置里的银联商务账户值拼成一条渠道。
//
// 拼出来的那棵树是 ums.Parse 认识的形状，键名与当初 payment_channels.config 里存的一模一样
// （baseURL / appId / mid / tid / sourceCode / domainName / sign.secretRef / sign.commSecretRef）。
// 适配器因此一行都不用改——它不知道这棵树今天是代码拼的还是从库里读的。
//
// 缺的必填项按**环境变量名**收集，不按这里的键名：读这条错误的人手里拿着的是 .env 或部署
// 清单，不是这棵树。
func umsChannel(cfg Config) *Channel {
	required := []struct{ key, value, env string }{
		{"appId", cfg.UMSAppID, "PAYMENT_UMS_APP_ID"},
		{"mid", cfg.UMSMID, "PAYMENT_UMS_MID"},
		{"tid", cfg.UMSTID, "PAYMENT_UMS_TID"},
		{"sourceCode", cfg.UMSSourceCode, "PAYMENT_UMS_SOURCE_CODE"},
		{"domainName", cfg.UMSDomainName, "PAYMENT_UMS_DOMAIN_NAME"},
	}
	var missing []string
	config := provider.Config{
		// baseURL 不进 missing：它有一个正确的默认值，由 ums 包在 Parse 时套上
		// （见 ums/config.go 的 defaultBaseURL）。
		"baseURL": strings.TrimSpace(cfg.UMSBaseURL),
		// 两个槽名由**代码**写死，不再由运营填：它们是适配器自己声明的名字，装配处照它们
		// 去 SecretEnv 里查环境变量名。让槽名可配只会多一种「配置里写的名字查无此槽」的错配。
		"sign": map[string]any{
			"secretRef":     slotAppKey,
			"commSecretRef": slotCommKey,
		},
	}
	for _, item := range required {
		value := strings.TrimSpace(item.value)
		if value == "" {
			missing = append(missing, item.env)
		}
		config[item.key] = value
	}

	return &Channel{
		Code:     ChannelCodeUMS,
		Name:     "银联商务全民付",
		Provider: ums.Name,
		Config:   config,
		// 存的是**环境变量名**，不是密钥值。
		SecretEnv: map[string]string{
			slotAppKey:  envUMSAppKey,
			slotCommKey: envUMSCommKey,
		},
		MissingEnv: missing,
		// 分账只有它这一条：三个键拼在下单报文里（见 provider/ums/create.go 的 divisionFields），
		// 支付成功即分账成功。
		Settlement: true,
	}
}

// wechatPayChannel 把部署配置里的微信直连账户值拼成一条渠道。
//
// 形状与 umsChannel 逐条对应：那棵树是 wechatpay.Parse 认识的形状
// （baseURL / appId / mchId / certFile / keyFile / sign.secretRef），缺的必填项按**环境变量名**
// 收集——读这条错误的人手里拿着的是 .env，不是这棵树。
//
// # 两条不做成必填的理由
//
//   - baseURL：它有唯一的正确取值（生产根地址），由 wechatpay 包在 Parse 时套上。留空即生产，
//     与 ums 的 baseURL 同一条取舍；本地假服务端通过它覆盖。
//   - certFile / keyFile：**只有两条路要证书**（代扣 /pay/pappayapply 与解约
//     /papay/deletecontract），而纯签约一次出网请求都没有。把证书做成必填，等于让「今天只
//     做签约」的部署因为一个它用不到的文件而起不来。缺了它们时，那两条路在适配器里**明确
//     拒绝**（见 wechatpay.Client.certClient），不是降级成不带证书发出去。
func wechatPayChannel(cfg Config) *Channel {
	// 与 umsChannel 一样，这里**只查账户值**：APIv2 密钥是凭据，不进 Config（见上面那段
	// 常量注释），因此它也不在 missing 里——密钥没配的症状是签约请求回一句
	// ErrSecretNotConfigured 并点名槽名，比启动时盲报一句「配置不全」精确。
	required := []struct{ key, value, env string }{
		{"appId", cfg.WeChatPayAppID, envWeChatPayAppID},
		{"mchId", cfg.WeChatPayMchID, envWeChatPayMchID},
		// 跳转目标（微信官方那个签约小程序）也做成必填：纯签约这条路的价值就是让客户端跳过去，
		// 缺了它算出来的参数表是**导不了跳**的，而失败会发生在用户手机上（点了没反应），
		// 不在日志里。它曾经写死在代码里，见 WeChatPaySignMiniProgramAppID 的注释。
		{"signMiniProgramAppId", cfg.WeChatPaySignMiniProgramAppID, envWeChatPaySignAppID},
	}
	var missing []string
	config := provider.Config{
		"baseURL": strings.TrimSpace(cfg.WeChatPayBaseURL),
		// 槽名由**代码**写死，不由运营填——与上面那棵树的 sign 段同一个理由。
		"sign": map[string]any{"secretRef": slotAPIv2Key},
		// 证书两项照抄路径。它们**不进 missing**，理由见上面的函数注释。
		"certFile": strings.TrimSpace(cfg.WeChatPayCertPath),
		"keyFile":  strings.TrimSpace(cfg.WeChatPayKeyPath),
	}
	for _, item := range required {
		value := strings.TrimSpace(item.value)
		if value == "" {
			missing = append(missing, item.env)
		}
		config[item.key] = value
	}

	return &Channel{
		Code:     ChannelCodeWeChatPay,
		Name:     "微信支付（直连委托代扣）",
		Provider: wechatpay.Name,
		Config:   config,
		// 存的是**环境变量名**，不是密钥值。
		SecretEnv:  map[string]string{slotAPIv2Key: envWeChatPayAPIKey},
		MissingEnv: missing,
	}
}

// 微信直连那组环境变量的**名字**（不是值）。理由与上面两把银联商务密钥逐字相同：值只在
// 签名那一刻 os.Getenv，不进任何会被打印的结构体。
const (
	// envWeChatPayAppID 是签约请求以哪个小程序的名义发出（见 platform/config 里
	// WeChatPayAppID 的注释：它与 WECHAT_MINIAPP_APP_ID 部署上是同一个值，但**不复用**）。
	envWeChatPayAppID = "WECHAT_PAY_APP_ID"
	// envWeChatPayMchID 是微信支付商户号。
	envWeChatPayMchID = "WECHAT_PAY_MCH_ID"
	// envWeChatPaySignAppID 是跳转目标那个小程序（微信官方的签约小程序）的 appid。
	// 它与 envWeChatPayAppID 不是同一个值，别合并。
	envWeChatPaySignAppID = "WECHAT_PAY_SIGN_MINI_PROGRAM_APP_ID"
	// envWeChatPayAPIKey 是 APIv2 密钥（32 位），签名与验签共用。
	envWeChatPayAPIKey = "WECHAT_PAY_API_V2_KEY"
)

// wechatH5Params 拼微信 H5 那两条路的 params：下单路径 + 三项应用标识。
//
// 三项都是**空则不带**（不是带一个空串）：渠道对缺的必填项会回一句「少了必填项」，而带了
// 一个空的应用名，它可能受理下来——那会让一条没配好的渠道看上去能跑。
func wechatH5Params(createPath string, cfg Config) map[string]string {
	params := map[string]string{"createPath": createPath}
	for key, value := range map[string]string{
		"sceneType":  cfg.UMSSceneType,
		"merAppName": cfg.UMSMerAppName,
		"merAppId":   cfg.UMSMerAppID,
	} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			params[key] = trimmed
		}
	}
	return params
}

// 两把银联商务密钥的**环境变量名**（不是值）。
//
// 它们在这里而不是在 platform/config 的结构体里：那个结构体会被打印、会被 %+v 到日志里，
// 密钥进去之后离泄漏只差一次调试。这里放名字，装配处在签名那一刻 os.Getenv——与
// PAYMENT_MANUAL_SECRET 走的是同一条路。
const (
	// envUMSAppKey 是出站签名那把（我们算给渠道验）。
	envUMSAppKey = "PAYMENT_UMS_APP_KEY"
	// envUMSCommKey 是入站验签那把（渠道算给我们验，H5 的回调与结果页回跳走它）。
	envUMSCommKey = "PAYMENT_UMS_COMM_KEY"
)
