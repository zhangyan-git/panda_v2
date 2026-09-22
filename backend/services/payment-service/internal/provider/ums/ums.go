// Package ums 是「appId + 时间戳 + 随机串 + 报文摘要 → HMAC-SHA256 → base64」这一族收单渠道的
// 适配器。今天它里面只有一家：银联商务全民付（老系统里叫 umspay）。
//
// 签名形状逐字照抄老系统的两个副本（internal/app/miniapp/services/umspay_service.go:225 与
// internal/app/openapi/services/umspay_service.go:64，两处除变量名外完全一致）：
//
//	timestamp = time.Now().Format("20060102150405")
//	nonce     = strconv.FormatInt(time.Now().UnixNano(), 10)
//	strToSign = appId + timestamp + nonce + sha256hex(body)   // body 是**原始报文字节**
//	sig       = base64Std( HMAC-SHA256(strToSign, appKey) )
//	Authorization: OPEN-BODY-SIG AppId="…", Timestamp="…", Nonce="…", Signature="…"
//
// 它值得单独成一个协议族（而不是 form_md5 / hmac_body 的一个 config）：签名在**请求头**里、
// 摘要覆盖**报文体**、凭证有自己的一套 `OPEN-BODY-SIG` 语法，三样都与那两族不同。而待签串里
// **没有方法与路径**——这一点比 hmac_body 松（那边签的是「这条报文是发给哪个接口的」），所以
// 一份报文被搬到另一个接口上重放，这一族拦不住。
//
// # 同一个渠道下的第二种协议：H5
//
// 上面那套（`POST + JSON 体 + OPEN-BODY-SIG 请求头`）是小程序那条路。H5 那条路**共用同一把
// appKey、同一个 mid/tid，但协议形状完全不同**：`GET + 参数全拼在 URL 上 + OPEN-FORM-PARAM`，
// 而且没有应答报文（浏览器直接 302 到收银台）。
//
// 「302 还是 200 + HTML」这个待确认项已定稿：2026-09-20 拿沙箱测试凭据把四条路径各打了一遍，
// **四条都回 302 + Location**（收银台地址），没有一条回 200 + HTML。
//
// 所以它不是「加一个 createPath」，是同一渠道下的第二种协议。分派点是 Create 里的
// `switch req.Method.Action`（H5 走 createH5、原生那条走原来的代码），默认值按 action 选
// （见 Protocol.options）。四条 H5 下单路径是**数据**，必须由 payment_methods.params 的
// createPath 逐条指出——它们没有合理默认值。
//
// 出站那两套签名的待签串**同形**（`appId + 时间戳 + 随机串 + sha256 hex(报文体)`），差的是
// 参数往哪儿放：一套进 Authorization 头，一套进查询串（见 signedQuery / buildH5Request）。
//
// # 入站有**第三套**签名
//
// 回调与结果页回跳用的是另一套：参数按名字排序拼串、末尾**直接接上通讯密钥**、再取 MD5 或
// SHA256（见 keyappend.go）。它不是出站那两套中的任何一套，方向也相反——出站是我们算给渠道
// 验，这一套是渠道算给我们验。**两把密钥**：appKey 出站用，通讯密钥入站用。
//
// 这一套同样没有任何文档或老系统实现可以对照（规范只说「有一个参与签名的随机字段」，
// 连签名参数叫什么都没写死），所以拿真实凭据联调时要确认的第一件事就是它。
//
// # 一个渠道挂三个支付方式
//
// 微信小程序 / 支付宝 / 云闪付三条 payment_methods 挂在同一个渠道行上，差别只有三个值
// （tradeType / createPath / instMid），由 payment_methods.params 覆盖 config 里的默认值，
// 见 Protocol.options。老系统靠 PaymentRequest.TradeType 这个字段传，V2 对应的位置就是
// params——接第二个支付方式是插一行数据，不是发一次版。
//
// # 与老系统的对照：哪些是照抄，哪些是推断
//
// 照抄（逐字，包括报文里的键名与「每个键都无条件出现」）：
//
//   - 签名与 Authorization 头的拼法（两个副本）；
//   - 下单报文（UnifiedOrder:104）与应答判据（errCode / miniPayRequest 两层）；
//   - 查单报文（QueryOrder:258，**不含 appId**）与状态词表（RecoverStuckCoffeeOrder:1986）；
//   - 回调的字段与「只处理成功、其余跳过」的处置（HandlePayNotify:2242）。
//
// 推断（老系统没有对应实现，**必须拿真实凭据实测确认**）：
//
//   - **回调验签**。老系统的 ParseNotify:375 是个空壳——注释写着「验签（可选）」，代码只有一句
//     json.Unmarshal；真正的回调入口 UMSPayNotify 同样不验签（它把 JSON 或表单解成 map 就直接
//     处理）。所以这一族的回调验签在本仓库**没有任何老系统参照**，它是按出站签名做的对称
//     实现。拿到真实凭据后第一件要确认的事就是：渠道的回调到底带不带 Authorization 头、
//     以及它是不是按同一套算法签的。**在那之前，这个 Verify 是推断，不是事实。**
//
//   - **成功回调的金额必填**。见 notify.go 里 normalizeCallback 上那一段注释。
//
//   - **H5 回调与结果页回跳的那套签名**。完整签名规则只对结果页写了（规范 §1.9.3），对支付
//     结果通知只说了「有一个参与签名的随机字段」。我们按同一套实现（keyappend.go），并补了
//     三条规范没写的判断：签名参数自己要从待签串里摘掉、hex 的大小写归一后再比、报文按内容
//     而不是 Content-Type 认 JSON/form。三条都标了理由，拿到真实回调后逐条确认。
//
//   - **回跳带的是哪些参数**。只知道必有 `merOrderId` 与 `sign`（规范原文），其余一概不知。
//     所以那一路只读单号，其余参数一个都不要（多读一个就多一个「渠道改了字段名」的塌方点）。
//
//   - **退款 / 退款查询 / 关单三条操作接口的报文**。老系统虽然做过退款，但它打的是**另一条
//     退款查询路径**（`/v1/netpay/trade/refund/query`），与这一版规范的 `/v1/netpay/refund-query`
//     不是同一版接口，所以这三条的字段集照抄的是官方样例（`开放平台H5支付/H5Pay/` 下那三份
//     Java）。两处已知分歧写在 operator.go 里：退款路径的默认值、以及退款查询报文里多带的
//     refundOrderId（样例没有、老系统有）。
//
//     2026-09-20 拿沙箱测试凭据实测（假单号）：三条都得到 `200 + errCode=NO_ORDER`，
//     说明路径、报文形状与签名都被收下了。其中**退款查询那条路径的分歧已定稿**——
//     `/v1/netpay/refund-query` 是业务层应答，老系统那条 `/v1/netpay/trade/refund/query`
//     直接 404。剩下的 refundOrderId 语义分歧要一笔真退过两次的支付才能结（见 operator.go）。
//
// # 已知缺口（写在这里，免得被当成漏了）
//
//   - **分账只有同步那一条**。下单报文里的 divisionFlag / platformAmount / subOrders 三个
//     参数（老系统照着 BuildUMSPayRequest:1833 现算）已经加在 createBody / h5CreateBody 上
//     （见 create.go 的 divisionFields）：有指令就三个键一起发，没有就一个都不发。**没做的
//     是异步分账**（asynDivisionFlag）与它后面那一步子单确认（/sub-orders-confirm）；也没有
//     查分账与冲正——规范只写了怎么发，没写渠道把分账结果怎么带回来，所以口径与老系统一致：
//     支付成功即视作分账成功（见 repository/callback.go 的 succeedSettlementInTx）。
//     **真要补的是那几条路，不是「把这三个字段加回 createBody」——那个已经做了。**
//   - **退款 / 退款查询 / 关单实现了，但服务层不调**（见 operator.go）。它们是 provider.Operator
//     这条**可选接口**的实现：报文、签名、判据、测试都在，缺的是上游——payment_refunds 的
//     写入方只能是订单侧的售后单，那一条聚合还没建。担保撤销 / 担保完成 / 异步分账确认
//     **连词表里都没有**（payment_provider_calls.operation 的 CHECK 里没有这三个值），
//     接它们要先改迁移，不是在这里加一个 case。
//   - **退款那条路拿不到渠道侧的退款单号**。官方样例与规范都只给了请求侧的字段名，应答里
//     哪个键是「渠道侧的退款单号」我们没有证据，所以 OperationResult.ProviderRefundID 留空
//     （回显回来的 refundOrderId 进应答摘要）。凭空读一个同名字段的风险同下面那一段——
//     写进 payment_refunds.provider_refund_id 的会是一个语义不同的值。要收口得先拿到一份
//     真实退款应答。
//   - **建单时不记渠道交易号**。老系统的下单应答里有没有 targetOrderId，我们手上没有证据
//     （UnifiedOrder 只校验 miniPayRequest 就返回了），所以 CreateResult.ProviderTransactionID
//     留空。后果是回调那一道「渠道交易号对不上就拒」的防线（repository/callback.go）在这一族
//     上是空转的——它只在两侧都非空时才比较。要收口得先拿到一份真实应答。
//   - **下单超时没有渠道侧的过期时间**。老系统的 PaymentRequest.ExpireTime 传了，但它根本没进
//     报文（UnifiedOrder 的 map 里没有这一项），所以本族也不发过期时间字段；CreateRequest 的
//     ExpiresAt 在这一族里只用于我们自己的超时关单。
package ums

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

// Name 是它在 provider.Registry 里的注册名，也是 catalog 里那条渠道的 Channel.Provider
// （回调路径里的那一段是 Channel.Code，今天两者同值、但是两个字段，见 catalog 的说明）。
const Name = "ums"

// 回调的应答体，形状照抄老系统的回调入口回答的那两个（order_handler.go:1118 成功回
// `c.JSON(200, {"msg":"SUCCESS"})`，失败回 `c.JSON(500, {"msg":"FAIL","error":…})`）。
//
// 失败那一份**不含 error 字段**：老系统把内部错误串回了渠道，而那条串既没用又是信息泄漏
// （见 provider.Ack 的注释）。
const (
	okAckBody   = `{"msg":"SUCCESS"}`
	failAckBody = `{"msg":"FAIL"}`
)

// headerAuthorization 是这一族唯一的协议凭证载体，取值不可配。
const headerAuthorization = "Authorization"

// authorizationScheme 是 Authorization 头的协议前缀，照抄老系统。
const authorizationScheme = "OPEN-BODY-SIG"

// signTimestampLayout 是**签名**用的时间格式（紧凑、无分隔）。
//
// 与报文里的 requestTimestamp（`2006-01-02 15:04:05`）不是同一个格式，这是渠道的约定：
// 照抄老系统，别顺手把两个统一成一个——统一之后签名对不上，而两边打印出来的时间看上去都对。
const signTimestampLayout = "20060102150405"

// errCodeSuccess 是渠道应答里表示「这次调用受理了」的 errCode。
//
// 它与「这一笔支付成没成」是两件事（见 interpretCreate）：errCode 说的是这次调用渠道收下了，
// 支付结果在 miniPayRequest / status 里。
const errCodeSuccess = "SUCCESS"

// 渠道的交易状态词表，逐字照抄老系统那两处 switch（RecoverStuckCoffeeOrder:2016 与
// HandlePayNotify:2298）。
//
// **成功有三个写法**（TRADE_SUCCESS / SUCCESS / success）：渠道在查单应答与回调里各用了一个，
// 而大小写都不统一。三个词必须都认——少认一个的表现是「钱收了但我们这边停在 pending」。
const (
	statusTradeSuccess = "TRADE_SUCCESS"
	statusSuccess      = "SUCCESS"
	statusSuccessLower = "success"
	// statusTradeClosed 是老系统里唯一一个「明确没成」的词，只有查单那条路会遇到。
	statusTradeClosed = "TRADE_CLOSED"
)

// Provider 实现 provider.Provider（外加 SecretSlotter、Querier 与 HeaderRecorder）。
type Provider struct {
	client *httpx.Client
}

// New 构造适配器。出网客户端由它自己持有：一个渠道一次调用只发一个请求，连接池的收益远小于
// 「每个适配器各自设超时」的可读性。
func New() *Provider { return &Provider{client: httpx.New(httpx.DefaultTimeout)} }

// Name 实现 provider.Provider。
func (*Provider) Name() string { return Name }

// SecretSlots 实现 provider.SecretSlotter：这一族最多**两把**密钥，名字都来自配置
// （`sign.secretRef` 与 `sign.commSecretRef`）。
//
// 第一把是 appKey（出站签名），第二把是通讯密钥（入站验签），方向相反，见 Protocol 上那一段。
// 第二把**配了才报**：没配说明这条渠道收不到 key 拼接签名的报文（只跑小程序那条路），那时
// 报一个没有值的槽名只会让装配处去找一把不存在的密钥。而 H5 的回调仍然会 fail closed——
// 判据在 commSecret 里，不在这个方法的返回值里。
//
// **解析失败时返回空切片**而不是报错，理由与 form_md5 / hmac_body 逐字相同：这个方法没有错误
// 可返回，而它唯一的调用方（装配处）拿到空切片会退回渠道行的 secret_ref。配置坏掉这件事会在
// 紧随其后的 Create / Verify 里以一条完整得多的错误爆出来。
func (*Provider) SecretSlots(method provider.Method) []string {
	protocol, err := Parse(method.ChannelConfig)
	if err != nil {
		return nil
	}
	slots := []string{protocol.secretRef}
	if protocol.commSecretRef != "" {
		slots = append(slots, protocol.commSecretRef)
	}
	return slots
}

// RecordableHeaders 实现 provider.HeaderRecorder：**一个头都不留**。
//
// provider.HeaderRecorder 的注释把话说死了：「**签名头不在其中**：哪怕是截断的前 8 位，它也是
// 『这条报文有有效签名』的一份凭证」。而这一族的协议凭证**全部装在 Authorization 一个头里**
// ——AppId / Timestamp / Nonce / Signature 四段是同一个字符串，摘不出「只留时间戳留签名」的
// 那一刀：要么整条留（等于把签名留档），要么一个都不留。这里选后者。
//
// 这一条与 provider.go 里「银联商务同理（Timestamp + Nonce）」那句是**冲突的**，这里说明
// 为什么以接口注释为准：那句话的前提是时间戳与随机串各占一个头（微信 APIv3 的形状），
// 而本族把它们拼进了签名头本身，所以它描述的那两个可留档的头在这些报文里不存在。
func (*Provider) RecordableHeaders() []string { return nil }

// Ack 实现 provider.Provider：按这条渠道的约定应答一次回调。
//
// 形状是照抄的：老系统的回调入口成功回 `HTTP 200 {"msg":"SUCCESS"}`、处理失败回
// `HTTP 500 {"msg":"FAIL"}`（order_handler.go:1118 与 :1110）。也就是说「拒绝回非 2xx」
// 在这一族里不是我们的发明，而是渠道侧的既有行为。
//
// 即便如此，理由仍要写清楚（它与 hmac_body 那条逐字相同）：**回 2xx 等于告诉渠道「这条通知
// 我收下了、别再投」**。我们拒掉一条通知的原因里有一大类是**暂时性**的（库连不上、上一次
// 投递还在处理中），那种情况下不重投的代价是：用户的钱确实扣了，而我们这边那笔支付单停在
// pending 直到超时关单——收钱不出货。回非 2xx 把那个风险换成「渠道可能多投几次」，而重投撞上
// payment_notifications 的唯一键，一个字段都不会改。
//
// **不看 method**：应答形状是渠道级的（渠道行上的 config），与支付方式无关；而回调路径上
// Method.Params 本来就是空的（见 provider.NotificationRequest 的注释）。
func (*Provider) Ack(_ provider.Method, accepted bool) provider.Ack {
	if accepted {
		return provider.Ack{Status: http.StatusOK, ContentType: "application/json", Body: []byte(okAckBody)}
	}
	return provider.Ack{Status: http.StatusBadRequest, ContentType: "application/json", Body: []byte(failAckBody)}
}

// Payload 拼出待签串：appId + 时间戳 + 随机串 + **报文体的 sha256（小写 hex）**。
//
// 导出它是**故意的**，与 manual.Sign / hmacbody.Payload 同一个考虑：适配器的签名算法必须有一处
// 权威实现，让后台的「试跑」、联调脚本与测试用的是同一份，而不是各自照文档抄一遍——抄错一次
// 的表现是「签名错误」，而那时你分不清是服务错了还是你的脚本错了。
//
// 三个入参都是**原样的**，不做任何整理。老系统的 sha256Hex 是 `fmt.Sprintf("%x", h.Sum(nil))`，
// 也就是**小写** hex——签名原文经不起一个大小写的差别。
func Payload(appID, timestamp, nonce string, body []byte) string {
	digest := sha256.Sum256(body)
	return appID + timestamp + nonce + hex.EncodeToString(digest[:])
}

// Sign 用 appKey 算一次签名，返回 **base64 标准编码**（不是 hex——这一族与 hmac_body 相反）。
func Sign(appID, timestamp, nonce string, body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(Payload(appID, timestamp, nonce, body)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// authorizationHeader 拼出 Authorization 头的值，逐字照抄老系统的那句 Sprintf。
//
// 引号、空格、逗号的位置都是协议的一部分：老系统的两个副本为了对齐这一串，连变量名都没改。
func authorizationHeader(appID, timestamp, nonce, signature string) string {
	return fmt.Sprintf(`OPEN-BODY-SIG AppId="%s", Timestamp="%s", Nonce="%s", Signature="%s"`,
		appID, timestamp, nonce, signature)
}

// authorize 生成一次出站调用要用的 Authorization 头。
//
// 下单与查单共用这一条路径（老系统也是：两个接口都走 buildUMSAuthHeader）。分开写两遍的写法
// 必然会让其中一条悄悄少签一段，而那种错只有在联调时才会暴露。
func authorize(protocol *Protocol, body []byte, secret string) string {
	timestamp := time.Now().Format(signTimestampLayout)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	return authorizationHeader(protocol.appID, timestamp, nonce,
		Sign(protocol.appID, timestamp, nonce, body, secret))
}

// formParamScheme 是 **GET 型接口**的认证方式取值，逐字照抄规范《认证流程》第 4 节：
//
//	authorization=OPEN-FORM-PARAM&appId=…&timestamp=…&nonce=…&content=…&signature=…
//
// 它与 OPEN-BODY-SIG 是同一套签名算法（见 Payload / Sign），差的只是**这些参数往哪儿放**：
// 那一套装在 Authorization 头里、签的是 POST 的报文体；这一套一律进 URL 查询串，而 content
// 本身就是一个「被 URL 编码过的 JSON 字符串」。
const formParamScheme = "OPEN-FORM-PARAM"

// signedQuery 把一次 GET 调用的五个认证参数拼成查询串。
//
// # 签名算在**未编码**的 content 上
//
// 规范原文的签名定义是
//
//	URLEncoder(Base64_Encode(HmacSHA256(appId + timestamp + nonce + SHA256_HEX(content), AppKey)))
//
// 其中 content 是「String 类型的业务报文内容」本身（规范给的例子就是一段明文 JSON），编码
// 只发生在把 signature 放进 URL 那一步。所以这里的入参 content 是**原始字节**，签名在调用方
// 已经算好（见 buildH5Request）。先把 content 编码再签会得到一个渠道验不过的签名，而两边
// 打印出来的串只差几个 %XX——这正是这条路上最难用眼睛发现的一种错。
//
// # 为什么 URL 编码这一步也算协议的一部分
//
// base64 里的 "+" "/" "=" 进了查询串就是另一层含义：不编码的话 "+" 会被对面解成空格，
// 签名当场变样。用 url.QueryEscape 而不是 PathEscape，是因为规范要的是 java.net.URLEncoder
// 那一套（空格编码成 "+"、"/"→%2F、"+"→%2B、"="→%3D），QueryEscape 与它逐字相同。
//
// appId / timestamp / nonce 也一并编码：它们今天是纯 ASCII（应用标识、14 位数字、随机串），
// 编码对它们是个恒等变换；万一渠道发的 appId 里有一个 "+"，不编码就会变成一个空格。
func signedQuery(appID, timestamp, nonce string, content []byte, signature string) string {
	return "authorization=" + formParamScheme +
		"&appId=" + url.QueryEscape(appID) +
		"&timestamp=" + url.QueryEscape(timestamp) +
		"&nonce=" + url.QueryEscape(nonce) +
		"&content=" + url.QueryEscape(string(content)) +
		"&signature=" + url.QueryEscape(signature)
}

// h5Request 是一次拼好的 H5 下单请求，外加算它时的那两个中间量。
//
// 中间量是给后台「试跑」用的：运营要跟渠道对的是**待签串**与**签名**（URL 里的 content 与
// signature 都是编码过的，编码过的串没法直接对着看）。生产路径只取 url。
type h5Request struct {
	url       string
	payload   string
	signature string
}

// buildH5Request 拼出 H5 下单的完整地址。
//
// 生产路径与后台试跑共用它，理由与 authorize 上那一段逐字相同：签名一旦有两份实现，
// 迟早有一处悄悄少签一段，而那种错只在联调时暴露。
func buildH5Request(protocol *Protocol, path string, content []byte, secret string) h5Request {
	// 时间与随机串现取，与 authorize 里那两行同源：签名原文里含它们，用固定值算出来的签名
	// 只对一份不存在的请求有效。
	timestamp := time.Now().Format(signTimestampLayout)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	signature := Sign(protocol.appID, timestamp, nonce, content, secret)
	return h5Request{
		url: protocol.url(path) + "?" +
			signedQuery(protocol.appID, timestamp, nonce, content, signature),
		payload:   Payload(protocol.appID, timestamp, nonce, content),
		signature: signature,
	}
}

// authorizationFields 是 Authorization 头拆出来的四段。
type authorizationFields struct {
	appID     string
	timestamp string
	nonce     string
	signature string
}

// parseAuthorization 把 `OPEN-BODY-SIG AppId="…", Timestamp="…", Nonce="…", Signature="…"`
// 拆回四段。
//
// 头缺失、前缀不对、少任何一段都返回错误，调用方一律当**签名不对**（ErrSignatureMismatch）：
// 一条没有（或解不出）签名头的请求不存在一份我们该认下来的签名。这里也**不做宽容解析**——
// 宽松只会让「渠道换了头的写法」在「验签通过」的表象下溜过去。
//
// 按 "," 切分是安全的：四段的值分别是我们自己的 appId、纯数字的时间戳与随机串、base64 的
// 签名，没有一段会含逗号（base64 标准表里没有它）。写一个处理引号内逗号的通用解析器是在给
// 一个不存在的需求写代码。
func parseAuthorization(raw string) (authorizationFields, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, authorizationScheme) {
		return authorizationFields{}, fmt.Errorf("authorization header is not %s", authorizationScheme)
	}
	rest := strings.TrimSpace(strings.TrimPrefix(raw, authorizationScheme))
	values := make(map[string]string, 4)
	for _, segment := range strings.Split(rest, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(segment), "=")
		if !found {
			return authorizationFields{}, fmt.Errorf("authorization header has an unreadable segment %q",
				strings.TrimSpace(segment))
		}
		values[strings.ToLower(strings.TrimSpace(name))] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	fields := authorizationFields{
		appID:     values["appid"],
		timestamp: values["timestamp"],
		nonce:     values["nonce"],
		signature: values["signature"],
	}
	if fields.appID == "" || fields.timestamp == "" || fields.nonce == "" || fields.signature == "" {
		return authorizationFields{}, fmt.Errorf("authorization header is missing one of AppId/Timestamp/Nonce/Signature")
	}
	return fields, nil
}

// isSucceededStatus 判断渠道的状态词是不是「钱收了」。
//
// 三个写法都认，见 statusTradeSuccess 那一组常量。它同时被下单、查单与回调三条路用：老系统也
// 是在三处各写了一遍同样的字符串（两处 switch 加一处 if），抄成一处是为了让「少认一个写法」
// 这种错只可能犯一次。
func isSucceededStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case statusTradeSuccess, statusSuccess, statusSuccessLower:
		return true
	}
	return false
}

// isHTTP2xx 判断 HTTP 状态码是不是 2xx。
func isHTTP2xx(status int) bool { return status >= 200 && status < 300 }

// isHTTP3xx 判断是不是 3xx。
//
// 对 **H5 下单**来说这是**成功**那一档：那个接口的成功"应答"就是一句 302 加一个 Location，
// 渠道让我们把浏览器送到收银台去（见 create.go 的 interpretH5Create）。
func isHTTP3xx(status int) bool { return status >= 300 && status < 400 }

// isHTTP4xx 判断是不是 4xx。
//
// 它单独一个函数是因为这个判断背着一个语义：「渠道在协议层拒了，没有建单」。见 interpretCreate
// 里那两个分支——4xx 可以安全地当失败，其余非 2xx（尤其是 5xx）不行。
func isHTTP4xx(status int) bool { return status >= 400 && status < 500 }
