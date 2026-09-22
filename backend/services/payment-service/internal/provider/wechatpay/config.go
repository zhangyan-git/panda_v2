// Package wechatpay 是「微信支付 APIv2 + XML 报文 + MD5 签名」这一族的适配器。
//
// 它今天只服务一件事：**委托代扣**（连续包月的签约、查约、解约、代扣）。这条链路在
// payment-service 的存在理由写在 migrations/payment/002 的文件头里：「微信委托代扣的签约、
// 扣款、解约归 payment-service；会员怎么续、什么时候该扣，由 membership-service 决定并传进来」。
//
// # 它为什么与 wechatv3 不是同一族
//
// 微信有两套完全不兼容的对公接口：APIv3（JSON + RSA 签名 + 平台证书 + AES-GCM 回调）与
// APIv2（XML + MD5 签名 + 回调明文）。**委托代扣只在 APIv2 上**，所以这一族不是「微信 v3 的
// 另一个入口」，是另一份协议。两者共用的只有商户号与商户证书。
//
// # 签名口径（逐条对齐老系统 panda_serve 的 wechat_entrust_service.go:766）
//
//	按参数名 ASCII 升序 → 剔掉空值、剔掉 sign 自己 → "k=v" 用 "&" 连
//	→ 末尾拼 "&key=<APIv2 密钥>" → MD5 → **hex 大写** → **不做 URL 编码**
//
// 这一串在 platform/signing 里是 spec 的一个取值（见 signSpec），不是这里重写的一遍：
// 老系统四处签名骨架相同、参数不同，那份包注释就是为这件事写的。
//
// # 三个接口的参数集**互不相同**，逐字照抄，不要「统一」
//
//	纯签约（小程序）：appid/mch_id/plan_id/contract_code/request_serial/
//	                  contract_display_account/notify_url/timestamp —— **8 个**，
//	                  **不含 nonce_str、不含 version**（含 nonce_str 微信回 SIGN_ERROR）
//	查签约 /papay/querycontract：多一个 version=1.0，**不含 nonce_str**
//	解约 /papay/deletecontract：多 version=1.0 **与** nonce_str
//	代扣 /pay/pappayapply：多 nonce_str、spbill_create_ip、trade_type=PAP、notify_url
//	预扣费通知 /papay/pap_pay_apply：多 nonce_str、deduct_date，**没有 notify_url / ip**
//
// 「哪个接口带哪几个字段」是渠道侧的事实，不是风格问题：多带一个 nonce_str 的签约请求会被
// 微信以 SIGN_ERROR 拒掉（老系统那一行注释就是这么写的）。所以这四张参数表分别写死在各
// 调用方法里，而不是抽一个「公共参数」函数出来——那个函数必然要为某一族多带或少带一个字段，
// 而那时错误的表现是「签名错误」，看不出是字段集的锅。
//
// # 出站四个接口里只有两个要商户证书
//
//	要（双向 TLS）：/pay/pappayapply（代扣）、/papay/deletecontract（解约）
//	不要：         /papay/querycontract（查约）、/papay/pap_pay_apply（预扣费通知）
//
// 与老系统逐条一致（它那四个方法里只有前两个用 tlsClient）。证书与私钥是**文件路径**配置，
// 不是凭据槽：槽是给「一把密钥」的，而这两个是商户平台下发的 PEM 文件。没配时那两条路
// **明确拒绝**，绝不降级成不带证书发出去——不带证书的 pappayapply 会得到一句 401 或
// 「商户证书未上传」，而那笔扣款到底有没有发生，我们再也说不清。
//
// # 与老系统的对照：照抄的、以及有意不照抄的
//
// 照抄：签名口径、四张参数表、URL 路径、判据（return_code / result_code 两层）、
// `querycontract` 的三个「等于没签」错误码与「-25 RESULT NULL」的处置、纯签约的参数集、
// 跳转微信官方签约小程序的两个固定值。
//
// 不照抄（每一条都写在对应的方法上）：
//
//   - **老系统的续费回调用 out_trade_no 反查订阅**，而它扣款时传的 out_trade_no 是
//     流水 id、回调里的却是另一套，两边不同源 → 查不到 → 回 FAIL → 微信无限重推。
//     V2 的关联键只有一个（contract_code），两边同源，见 provider.go 的 Verify。
//   - **老系统只有进程内互斥锁**，多副本会重复扣款。V2 的幂等是库级的
//     （payment_agreement_charges 的 UNIQUE (agreement_id, biz_period)），这一族不持锁。
//   - **老系统连续失败 3 次直接解约、没有退避**。V2 只把失败次数记下来（列已经在库上），
//     什么时候解约是下一刀的事。
package wechatpay

import (
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// Name 是它在 provider.Registry 里的注册名，也是 catalog 里那条渠道的 Channel.Provider。
const Name = "wechat_pay"

// 渠道侧的常量。路径与根地址逐字照抄老系统（wechat_entrust_service.go:240/397/456/511/599/713）。
const (
	// defaultBaseURL 是微信支付的**生产**根地址，与 APIv3 同址不同路径。
	//
	// 留空即生产：它是这份协议的常识，不是这家公司的部署事实（与 ums 那边同一条取舍）。
	// 本地假服务端与联调环境通过 config 里的 baseURL 覆盖。
	defaultBaseURL = "https://api.mch.weixin.qq.com"

	pathQueryContract  = "/papay/querycontract"
	pathDeleteContract = "/papay/deletecontract"
	pathPAPPayApply    = "/pay/pappayapply"
	pathPreDeduction   = "/papay/pap_pay_apply"
)

// 小程序纯签约的跳转目标：**微信官方的签约小程序**，不是我们自己的。
//
// 服务端只负责算出签名与参数，用户在微信那个小程序里点「同意并签约」，签完之后微信跳回
// 我们的小程序（`wx.navigateToMiniProgram` 的返回）并向 notify_url 推 change_type=ADD。
// 所以签约这件事从头到尾没有一次「我们发起的请求」——这也是鉴权那条路上没有它签名的原因。
//
// 跳转目标的 **appid 是配置**（Protocol.signMiniProgramAppID，环境变量
// WECHAT_PAY_SIGN_MINI_PROGRAM_APP_ID），不再写死在这里。
//
// 它原先的理由是「微信侧的固定值，换一个值等于跳去另一个小程序，而那个小程序不存在第二个」。
// 那说的是**取值**只有一个，推不出**它可以公开**：它是一串能定位到具体主体的标识，与商户号
// 同一性质，写进源码就是把它发给了每一个拿到仓库的人。协议固定 ≠ 可以提交进仓库——取值仍然
// 只有那一个，但它归部署配置管。签下这条判据的是 GitHub 的密钥扫描（commit 4920ce71）。
//
// **path 仍然写死**：那是一个页面路径，不含任何主体标识，换一个值只是跳到同一小程序里的
// 另一页，泄不出任何东西。
const SignMiniProgramPath = "pages/index/index"

// 响应里的状态码，照抄微信 APIv2 的约定。
const (
	returnCodeSuccess = "SUCCESS"
	resultCodeSuccess = "SUCCESS"
)

// ContractState 是微信侧的签约状态（querycontract 应答里的 contract_state）。
type ContractState string

const (
	// ContractActive：已签约。这一份协议在微信那边是活的，可以扣款。
	ContractActive ContractState = "0"
	// ContractTerminated：未签约或已解约。它同时是「用户从没签过」与「用户已经解约」——
	// 微信不区分这两种，我们也不该试图区分（见 QueryContract 里那三个错误码）。
	ContractTerminated ContractState = "1"
	// ContractPending：签约进行中。支付中签约刚完成时查询接口会短暂返回它。
	ContractPending ContractState = "9"
)

// Protocol 是一份解析好、校验过的协议声明。
//
// 与 ums 的 Protocol 同一种东西：它是一条**具体协议**（微信委托代扣 APIv2）的参数表，不是
// 「一族渠道的骨架」。报文形状、签名口径、成功判据全部写死在代码里，配置里只有账户值与证书
// 路径——把一条钉死的协议拆成一堆可配的旋钮，只会让「配错了」看上去像「协议变了」。
type Protocol struct {
	// appID 是小程序的 appid（老系统取 wechat.miniprogram.appid），它进每一条待签串。
	//
	// 它**不是**跳转签约那个小程序的 id（那是 SignMiniProgramAppID）：签约的发起方是我们，
	// 用户去微信那个小程序里确认，所以待签串里出现的是**我们**的 appid。
	appID string
	// mchID 是微信支付商户号。老系统把它明文写在 config.yaml 里；V2 从环境变量来
	// （WECHAT_PAY_MCH_ID，见 catalog），真值只在 .env 里——它与 signMiniProgramAppID
	// 是同一类东西：能定位到具体主体的标识，不进仓库。
	mchID string
	// baseURL 是根地址，默认见 defaultBaseURL，**不带结尾斜杠**。
	baseURL string
	// signMiniProgramAppID 是**跳转目标**那个小程序的 appid（微信官方的签约小程序），
	// 由客户端拿着它调 `wx.navigateToMiniProgram`。它**不是** appID：待签串里出现的是我们
	// 自己的 appid，这个只是用户去哪儿点「同意」。
	//
	// 必填。缺了它算出来的参数表导不了跳，而失败发生在用户手机上（点了没反应），不在日志里，
	// 所以让它在 Parse 就撞墙（catalog 那边按环境变量名点名）。
	signMiniProgramAppID string
	// secretRef 是 APIv2 密钥的槽名（32 位）。**签名与验签用的是同一把**——微信 APIv2 的
	// 回调就是用这把密钥按同一套算法签的，所以这一族只有一个槽。
	secretRef string
	// certFile / keyFile 是商户证书与私钥的 PEM 路径（apiclient_cert.pem / apiclient_key.pem）。
	//
	// 两个都填了才算配齐：只填一个与都不填等价（那两条要证书的路会明确拒绝），不去猜
	// 「另一个是不是有默认路径」。它们是**文件路径**而不是凭据槽，理由见包注释。
	certFile string
	keyFile  string
}

// Parse 解析并校验一份渠道 config。
//
// 错误都包着 provider.ErrConfigInvalid，路径是完整的（`config.sign.secretRef`）。
func Parse(config map[string]any) (*Protocol, error) {
	cfg := provider.Config(config)

	appID, err := cfg.RequireString("appId")
	if err != nil {
		return nil, err
	}
	mchID, err := cfg.RequireString("mchId")
	if err != nil {
		return nil, err
	}
	signMiniProgramAppID, err := cfg.RequireString("signMiniProgramAppId")
	if err != nil {
		return nil, err
	}
	secretRef, err := cfg.RequireString("sign.secretRef")
	if err != nil {
		return nil, err
	}

	return &Protocol{
		appID:                appID,
		mchID:                mchID,
		signMiniProgramAppID: signMiniProgramAppID,
		baseURL:              textOr(cfg.String("baseURL"), defaultBaseURL),
		// 证书两项**都不填是合法的**：一个只跑纯签约（签约参数在本地算、不发请求）的部署
		// 一个证书都不需要。要证书的那两条路自己 fail closed，见 certClient。
		secretRef: secretRef,
		certFile:  strings.TrimSpace(cfg.String("certFile")),
		keyFile:   strings.TrimSpace(cfg.String("keyFile")),
	}, nil
}

// url 拼出真实的请求地址。
//
// 抹掉结尾斜杠与 ums 那边同款：运营填地址时带不带结尾斜杠是不确定的，两边都带会拼出
// `//papay/querycontract`，有些网关把它当另一个路径、直接 404。
func (p *Protocol) url(path string) string {
	return strings.TrimRight(p.baseURL, "/") + path
}

// hasCert 判断这一份配置能不能走双向 TLS 的那两条路。
func (p *Protocol) hasCert() bool {
	return p.certFile != "" && p.keyFile != ""
}

// textOr 取一个配置里的文本，空则用默认值。
func textOr(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}
