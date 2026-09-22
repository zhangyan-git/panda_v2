package ums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

// 本文件是 provider.Operator 的实现：对一张**已经建好的**支付单发起一次渠道侧操作
// ——退款 / 退款查询 / 关单。
//
// # 三条路共用同一套出网与认证
//
// 全是 `POST + JSON 体 + OPEN-BODY-SIG 请求头`（规范 §5 那张表），也就是**小程序下单那条路**
// 的同一套形状（`POST + JSON + Authorization 头`）。所以它们走 Provider.post，与下单、查单
// 是同一条出网路径——签名只有一份实现。
//
// **不是** H5 那套 OPEN-FORM-PARAM：那套只服务 GET 型接口（H5 下单），这三条都是 POST。
// 一个渠道同时挂着小程序与 H5 两条支付方式时，三条操作接口也是同一个地址、同一套认证，
// 差别只在报文里的 instMid（见 operationOptions）。
//
// # 报文的依据是官方样例，不是老系统
//
// 字段集逐字照抄 `开放平台H5支付/H5Pay/` 下那三份 Java 样例（Refund.java、RefundQuery.java、
// Close.java）里的报文类。老系统 panda_serve 里虽然也有一份退款实现，但它打的是**另一条退款
// 查询路径**（`/v1/netpay/trade/refund/query`，见下面那组常量），两者不是同一版接口，所以
// 有冲突的地方一律以**这一版规范与样例**为准，并在这里把冲突写下来。
//
// # 谁在调它
//
// 退款与退款查询这两条路由 service/refund.go 调：order-service 的售后单审核通过时同步调
// CreateRefund 建出这张退款单（幂等键就是 after_sale_no），应答没有定论的那些由
// worker/refund.go 定期问出结局。这条链的另一头（售后单 → 订单状态）在 order-service。
//
// 关单那条路**今天仍然没有调用方**，而且它不该被 worker/expiry.go 用上：H5 那条路的报文里
// 发了 expireTime，渠道侧那张预支付单自己会过期（见 h5CreateBody 上那一段）。
//
// # 不在这里的三个操作
//
// 担保撤销（/v1/netpay/secure-cancel）与担保完成（/v1/netpay/secure-complete）：我们从不发
// secureTransaction=true 的单，这两个接口**没有输入可操作**；而且 payment_provider_calls 的
// operation CHECK 里也没有这两个值。
//
// 异步分账确认（/v1/netpay/sub-orders-confirm）：它对应的是**异步**分账（asynDivisionFlag）——
// 先建单、N 天后再来确认子单。我们走的是同步那条（divisionFlag，随下单一次下发，见 create.go
// 的 divisionFields），发出去就是终局，没有子单要确认。真要做异步那条路时，这里要接的是
// asynDivisionFlag 的下单分支，不是先加这个 case。
//
// 这四个操作连词表里都没有，接它们要先改迁移，不是在这里加一个 case。

// 三条操作接口的路径，默认值取自上面那三份官方样例的头注释，可以在渠道 config 里覆盖
// （endpoints.refund / endpoints.refundQuery / endpoints.close）。
//
// **退款查询这一条的默认值与老系统不同**：老系统打的是 `/v1/netpay/trade/refund/query`，
// 而这一版规范（§5 的接口表）与官方样例都是 `/v1/netpay/refund-query`。两条路径在渠道那边
// 未必都开着，而我们对这一版协议的其余实现（H5 下单）也是照这一版规范做的，所以默认跟规范走，
// 老系统那条路径留成一个可配的值。**拿真凭据联调时要确认的就是这一条**：两条路径哪条通。
const (
	defaultRefundPath      = "/v1/netpay/refund"
	defaultRefundQueryPath = "/v1/netpay/refund-query"
	defaultClosePath       = "/v1/netpay/close"
)

// 退款状态词表，照抄规范 §6：SUCCESS / FAIL / PROCESSING / UNKNOWN。
//
// 四个词分成两档：前两个**有定论**（钱退到了 / 渠道明确说退不成），后两个**没有**——
// 规范原文说它们「要再查」，所以它们既不是失败也不是成功，唯一的出路是退款查询
// （见 interpretRefund 里那一段）。
//
// 这四个词**不放进 isSucceededStatus**：那组常量是**交易状态**的词表（TRADE_SUCCESS / SUCCESS /
// success），两者虽然都有 SUCCESS 这个词，却是两个字段、两套取值。混在一起的表现是「退款应答
// 里但凡有个像成功的词就算退成了」，而退款判错的代价是钱。
const (
	refundStatusSuccess    = "SUCCESS"
	refundStatusFail       = "FAIL"
	refundStatusProcessing = "PROCESSING"
	refundStatusUnknown    = "UNKNOWN"
)

// refundBody 是 /v1/netpay/refund 的报文，字段集与顺序照抄官方样例 Refund.java 的 H5PayBody。
//
// **没有 appId**：样例的报文里没有它（与 queryBody 那一条同源），而签名头照旧用 appId 签。
// 老系统那份退款实现多发了 appId，那是它与这一版样例的另一处分歧——同样以样例为准。
//
// 样例里还有 srcReserve / platformAmount / subOrders 三项：前两项在样例里恒为空（因而恒不
// 出现），第三项是分账子单。分账不做（见本文件抬头），所以三项都不声明。
type refundBody struct {
	MsgID            string `json:"msgId"`
	RequestTimestamp string `json:"requestTimestamp"`
	MerOrderID       string `json:"merOrderId"`
	InstMid          string `json:"instMid"`
	Mid              string `json:"mid"`
	Tid              string `json:"tid"`
	// RefundAmount 在样例里是**字符串**（`"refundAmount":"1"`），不是 JSON 数字。
	//
	// 这一族的金额有三种形状：下单是数字、查单应答两种都认、 refund 请求是字符串。照抄样例
	// 发字符串——渠道那边的解析器未必两种都收，而这一处错了的表现是一句关于参数的拒绝，
	// 看不出是金额的写法问题。
	RefundAmount  string `json:"refundAmount"`
	RefundOrderID string `json:"refundOrderId"`
	// RefundDesc 是退款说明，进渠道的账单备注。可选，空则不发（样例同样是空则不发）。
	RefundDesc string `json:"refundDesc,omitempty"`
}

// refundQueryBody 是 /v1/netpay/refund-query 的报文，字段集照抄官方样例 RefundQuery.java 的
// RefundQueryBody。
//
// **refundOrderId 是样例里没有、而我们加上的一项**，这是本文件里唯一一处「比样例多」：
//
//   - 不加它，这次查询能按的只有 merOrderId。一笔支付退过两次时，那份应答说的是**哪一次**？
//     规范没有说，而猜错的代价是拿另一次退款的状态去改这一笔（可能把一笔 PROCESSING 记成
//     成功）。老系统那份（打另一条路径的）实现是带 refundOrderId 的；
//   - 加它的风险是「这一版接口不认这个参数」。那种情况下渠道要么忽略它（我们会在回显校验里
//     发现回显对不上，于是这份应答**不采信**，见 interpretQueryRefund），要么直接拒（我们
//     一样拿不到结论）。两个后果都只是「还是不知道」，不是「记错了一笔」。
//
// 两个方向的坏后果不对称，所以加上它，并把这个分歧标成待确认项。
//
// # 联调实测（2026-09-20，沙箱 + 真实测试凭据）
//
// 带上这条路径的默认值 /v1/netpay/refund-query 与这个参数，用**假单号**打过去得到的是
// `200 + errCode=NO_ORDER「无法找到指定的订单」`——一句业务层的答复，不是「参数不合法」。
// 这说明路径与报文形状都被收下了。
//
// **但这不足以把上面那个分歧结掉**：单号本来就查不到，渠道多半在参数校验之前就返回了。
// 要真正确认「一笔支付退过两次时这份应答说的是哪一次」，得有一笔真实的、退过两次的支付。
type refundQueryBody struct {
	MsgID            string `json:"msgId"`
	RequestTimestamp string `json:"requestTimestamp"`
	MerOrderID       string `json:"merOrderId"`
	InstMid          string `json:"instMid"`
	Mid              string `json:"mid"`
	Tid              string `json:"tid"`
	RefundOrderID    string `json:"refundOrderId,omitempty"`
}

// closeBody 是 /v1/netpay/close 的报文，字段集照抄官方样例 Close.java。
//
// （样例里那个报文类叫 SecureCompleteBody——是复制来的类名，字段集是 close 的：msgId /
// requestTimestamp / mid / tid / instMid / merOrderId。别被这个名字带走去接担保完成。）
type closeBody struct {
	MsgID            string `json:"msgId"`
	RequestTimestamp string `json:"requestTimestamp"`
	MerOrderID       string `json:"merOrderId"`
	InstMid          string `json:"instMid"`
	Mid              string `json:"mid"`
	Tid              string `json:"tid"`
}

// refundResponse 是退款与退款查询两条路的应答。
//
// 两条路共用一个结构体：应答的字段是同一批，而**判据不同**（退款发起问「这次调用收下了没有、
// 退款的结局是什么」，退款查询本来就只问后者）。分开写两份的结果是两份都会跟着渠道漂。
type refundResponse struct {
	ErrCode string `json:"errCode"`
	ErrMsg  string `json:"errMsg"`
	// MerOrderID / RefundOrderID 是渠道回显的两个单号，用来确认这份应答说的是**同一笔**。
	// 见 interpretQueryRefund 里那一段回显校验。
	MerOrderID    string `json:"merOrderId"`
	RefundOrderID string `json:"refundOrderId"`
	// RefundStatus 是退款状态，取值见上面那组常量。
	//
	// **它才是退款成没成的判据**，errCode 只说明这次调用被受理了。老系统只看 errCode（
	// refund_service.go:1000 那一带），而规范 §6 明说 PROCESSING / UNKNOWN 要再查——照它那样
	// 处置，一笔还在渠道那边处理中的退款会被记成「已退款」。
	RefundStatus string `json:"refundStatus"`
	// RefundAmount 与其余几条路一样可能以数字或字符串到达，走同一个解码器。
	RefundAmount json.RawMessage `json:"refundAmount"`
}

// closeResponse 是关单应答。只有两个判据字段：关单不问「关掉之后怎么样」，问的是
// 「这张预支付单还开着吗」——errCode 就是回答。
type closeResponse struct {
	ErrCode string `json:"errCode"`
	ErrMsg  string `json:"errMsg"`
}

// Execute 实现 provider.Operator：按操作分派到三条路之一。
//
// # 分派在凭据解析之前
//
// 与 Create 里那个 switch 同源：一个这一族没实现的操作应该报出「这个操作没实现」，而不是
// 先报「凭据是空的」——后者会让人去凭据那一栏白找一遍。
//
// # 返回值分工
//
// 与 provider.Provider.Create 逐字相同：result 描述**这次尝试**，err 只在「这次调用根本没
// 发出去」时非 nil（配置解不开、密钥是空的、参数缺了）。渠道拒绝了是 result 里的一个取值。
func (p *Provider) Execute(ctx context.Context, req provider.OperationRequest) (provider.OperationResult, error) {
	protocol, err := Parse(req.Method.ChannelConfig)
	if err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: %w", req.Method.ChannelCode, err)
	}

	switch req.Operation {
	case provider.OperationRefund:
		return p.refund(ctx, protocol, req)
	case provider.OperationQueryRefund:
		return p.queryRefund(ctx, protocol, req)
	case provider.OperationClose:
		return p.close(ctx, protocol, req)
	default:
		// 这一族确实有做退款/退款查询/关单的能力，只是这一个操作不在其中。**报一个专门的
		// 错误**而不是 ResultFailed：调用方对两者的处置不同——前者是配置问题（这一族的
		// Operator 被挂上了一个它没有的操作），后者是「渠道拒了这笔操作」。
		return provider.OperationResult{Result: provider.ResultUnknown},
			fmt.Errorf("%w: channel %q: operation %q is not implemented by provider %s (it does %s, %s and %s)",
				provider.ErrOperationNotSupported, req.Method.ChannelCode, req.Operation, Name,
				provider.OperationRefund, provider.OperationQueryRefund, provider.OperationClose)
	}
}

// refund 向渠道发起一笔退款。
//
// # 退款单号由调用方给，而且每次必须不同
//
// 规范 §5 原文：重复送同一对 `merOrderId + refundOrderId` 时，渠道会**把上一次那张退货单
// 原样回给我们**，而不是再退一次。那是幂等、不是错误，但把「退第二次」误当成「已经退过了」
// 就会少退钱。所以这里不生成退款单号（也不复用支付单号拼一个），只透传调用方给的
// OperationRequest.RefundNo——**生成它的地方必须知道这是第几次退**，而那是上游聚合的事。
//
// msgId 与 refundOrderId 同值（老系统也是这么填的），它是渠道侧的请求号：同一笔退款重试时
// 它不变，渠道据此认出「这是同一个请求」，而不是一笔新的退款。
//
// # 两个单号都要带 sourceCode 前缀（见 orderid.go 末尾那一节）
//
// refundOrderId 是**账单号**，规范明说它要遵循商户订单号生成规范；merOrderId 这里指的是
// **要退的那一笔支付**，编码必须与发起时逐字一致——对不上时渠道会认为我们在退一张它那边不
// 存在的单，而它的回答会是「查无此单」。两者都由 channelOrderID 一处编码。
func (p *Provider) refund(ctx context.Context, protocol *Protocol, req provider.OperationRequest) (provider.OperationResult, error) {
	if err := requireOperationInputs(req, true, true); err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown}, err
	}
	secret, err := operationSecret(protocol, req)
	if err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown}, err
	}

	refundOrderID := protocol.channelOrderID(strings.TrimSpace(req.RefundNo))
	options := protocol.operationOptions(req)
	body, err := json.Marshal(refundBody{
		MsgID:            refundOrderID,
		RequestTimestamp: time.Now().Format(requestTimestampLayout),
		MerOrderID:       protocol.channelOrderID(req.PaymentNo),
		InstMid:          options.instMid,
		Mid:              protocol.mid,
		Tid:              protocol.tid,
		RefundAmount:     strconv.FormatInt(req.Amount, 10),
		RefundOrderID:    refundOrderID,
		RefundDesc:       strings.TrimSpace(req.Reason),
	})
	if err != nil {
		// 结构体里全是字符串，Marshal 不会失败。留着这个分支是为了以后加字段时不会静默吞掉错误。
		return provider.OperationResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: encode refund body: %w", req.Method.ChannelCode, err)
	}

	response, err := p.post(ctx, protocol, secret, protocol.refundPath, body)
	if err != nil {
		return protocol.operationTransportFailure(req, err)
	}
	return protocol.interpretRefund(response.Body, response.StatusCode, response.Attempts, req), nil
}

// queryRefund 问渠道一笔退款到底退成了没有。
//
// 它是退款这条路上**唯一能给出结论**的调用：退款应答里的 PROCESSING / UNKNOWN 说明渠道还在
// 处理，而规范 §6 明说这两者要再查。
//
// msgId 用 `REFUNDQUERY` + 纳秒，与查单那条路的 `QUERY` + 纳秒同一种约定（那是「这一次调用」
// 的请求号，与退款单号无关）。
func (p *Provider) queryRefund(ctx context.Context, protocol *Protocol, req provider.OperationRequest) (provider.OperationResult, error) {
	if err := requireOperationInputs(req, true, false); err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown}, err
	}
	secret, err := operationSecret(protocol, req)
	if err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown}, err
	}

	options := protocol.operationOptions(req)
	body, err := json.Marshal(refundQueryBody{
		MsgID:            "REFUNDQUERY" + strconv.FormatInt(time.Now().UnixNano(), 10),
		RequestTimestamp: time.Now().Format(requestTimestampLayout),
		MerOrderID:       protocol.channelOrderID(req.PaymentNo),
		InstMid:          options.instMid,
		Mid:              protocol.mid,
		Tid:              protocol.tid,
		// 与 refund() 发出去的那个值必须逐字相同（见 orderid.go 末尾那一节）：渠道拿它找退款单，
		// 两边编码不一致时它会把「有这单」答成「查无此单」。
		RefundOrderID: protocol.channelOrderID(strings.TrimSpace(req.RefundNo)),
	})
	if err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: encode refund query body: %w", req.Method.ChannelCode, err)
	}

	response, err := p.post(ctx, protocol, secret, protocol.refundQueryPath, body)
	if err != nil {
		return protocol.operationTransportFailure(req, err)
	}
	return protocol.interpretQueryRefund(response.Body, response.StatusCode, response.Attempts, req), nil
}

// close 关掉渠道侧那张还没付的预支付单。
//
// # 它不需要退款单号，也不需要金额
//
// 关单的输入只有一个「哪一单」。金额在这条路上没有意义：单据还没付，能关就说明一分钱都没收。
//
// # 已经关过的单
//
// 规范的错误码表里有 `OPERATION_NOT_ALLOWED`（订单已关闭）。**它没有被当成成功**（见
// interpretClose）：那个码的字面意思不止一种，「已经关了」与「这一单付过了、不许关」都会
// 落到它上面，而后者记成「已关单」会让一笔真收了的钱被当成没付。不确定就维持现状，
// 由调用方（今天还没有）去查单定夺。
func (p *Provider) close(ctx context.Context, protocol *Protocol, req provider.OperationRequest) (provider.OperationResult, error) {
	if err := requireOperationInputs(req, false, false); err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown}, err
	}
	secret, err := operationSecret(protocol, req)
	if err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown}, err
	}

	options := protocol.operationOptions(req)
	body, err := json.Marshal(closeBody{
		MsgID:            "CLOSE" + strconv.FormatInt(time.Now().UnixNano(), 10),
		RequestTimestamp: time.Now().Format(requestTimestampLayout),
		MerOrderID:       protocol.channelOrderID(req.PaymentNo),
		InstMid:          options.instMid,
		Mid:              protocol.mid,
		Tid:              protocol.tid,
	})
	if err != nil {
		return provider.OperationResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: encode close body: %w", req.Method.ChannelCode, err)
	}

	response, err := p.post(ctx, protocol, secret, protocol.closePath, body)
	if err != nil {
		return protocol.operationTransportFailure(req, err)
	}
	return protocol.interpretClose(response.Body, response.StatusCode, response.Attempts, req), nil
}

// operationOptions 取这一次操作要用的那批「一个支付方式一个值」的参数。
//
// **只看 instMid**：操作报文里没有 tradeType 与 createPath（三条路各自一个固定路径，见上面那组
// 常量），而 instMid 必须与**当初建这一单时**用的那个一致——H5 建的单是 H5DEFAULT，小程序建的
// 单是 MINIDEFAULT，报错一个，渠道的表现是「查无此单」。
//
// 所以它从 req.Method 上取，与下单那条路走**同一个** options：支付方式告诉了我们这一单是哪条
// 协议建的，params 上的覆盖也照旧生效。凭空取渠道配置的默认值（MINIDEFAULT）会让所有 H5 单
// 的退款都打到别处去。
func (p *Protocol) operationOptions(req provider.OperationRequest) createOptions {
	return p.options(req.Method.Params, req.Method.Action)
}

// operationSecret 取出这一次操作用的 appKey。
//
// 三条操作接口都是**出站**签名（我们算给渠道验），所以用的是第一把密钥——与下单、查单同一把。
// 入站那把通讯密钥（commSecretRef）与这里无关。
func operationSecret(protocol *Protocol, req provider.OperationRequest) (string, error) {
	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		// 空密钥是「签不了」，不是「不签」。见 provider.CreateRequest.Secrets。
		return "", fmt.Errorf("channel %q: %w", req.Method.ChannelCode, provider.ErrSecretNotConfigured)
	}
	return secret, nil
}

// requireOperationInputs 拒绝三种「本地就知道发出去也没用」的输入。
//
// 三种都在这里拒（一个字节都不发）而不是让渠道去拒，理由与 Create 里那一段逐字相同：渠道回的
// 会是一句与本意无关的错（「参数不对」「查无此单」），而那条错误会盖住真正的原因——运维照着它
// 会去查渠道的配置，而问题其实在调用方少传了一个字段。
//
// **支付单号三种操作都必需**：退款与关单都按商户单号认这一笔，空着发过去等于问渠道「随便哪一单
// 怎么样」。
func requireOperationInputs(req provider.OperationRequest, needsRefundNo, needsAmount bool) error {
	if strings.TrimSpace(req.PaymentNo) == "" {
		return fmt.Errorf("%w: channel %q: operation %s needs a payment number",
			provider.ErrConfigInvalid, req.Method.ChannelCode, req.Operation)
	}
	if needsRefundNo && strings.TrimSpace(req.RefundNo) == "" {
		// 退款单号不是「我们这边的编号」那么简单：它是渠道判幂等的那个键（见 refund 里那一段）。
		// 空着发过去，渠道要么拒，要么把它当成一个默认值——后者会让**第二笔退款**撞上第一笔
		// 的幂等键，然后把上一次的结果当成这一次的结局回给我们。
		return fmt.Errorf("%w: channel %q: operation %s needs a refund number",
			provider.ErrConfigInvalid, req.Method.ChannelCode, req.Operation)
	}
	if needsAmount && req.Amount <= 0 {
		// 金额是权威值（见 provider.CreateRequest.Amount）。退 0 分或退负数说明契约被破坏了，
		// 而渠道那边对负数的处理我们一无所知。
		return fmt.Errorf("%w: channel %q: refund amount %d is not positive",
			provider.ErrConfigInvalid, req.Method.ChannelCode, req.Amount)
	}
	return nil
}

// operationTransportFailure 把一次操作出网失败翻成 provider.OperationResult。
//
// 与查单那条路（queryTransportFailure）同源：**一律 ResultUnknown，而且带上原始 error**。
// 这三种操作都是「发出去了就可能有后果」的调用——退款可能已经到了渠道那边、关单可能已经把
// 预支付单关了。标失败会让调用方去做一件已经做过的事：再退一次（重复退款），或者以为单还
// 开着（用户付了一笔我们以为关掉了的单）。
//
// 与查单那条路的一处差别：这里**区分「没发出去」与「发出去了没等到应答」**，两者都进摘要，
// 因为运维要能看出这次操作到底有没有可能落到渠道上（Attempts 与 httpx 的 NotSent 是唯一
// 的判据）。
func (p *Protocol) operationTransportFailure(req provider.OperationRequest, err error) (provider.OperationResult, error) {
	attempts := 1
	var httpErr *httpx.Error
	notSent := !errors.As(err, &httpErr) || httpErr.NotSent
	if errors.As(err, &httpErr) {
		attempts = httpErr.Attempts
	}
	return provider.OperationResult{
			Result:          provider.ResultUnknown,
			FailureCode:     "OPERATION_NOT_ANSWERED",
			FailureMessage:  err.Error(),
			Attempts:        attempts,
			ResponseSummary: map[string]any{"baseURLHost": hostOf(p.baseURL), "notSent": notSent},
		},
		fmt.Errorf("channel %q: %s request failed: %w", req.Method.ChannelCode, req.Operation, err)
}

// interpretRefund 把退款应答翻成 provider.OperationResult。
//
// # 三层，缺一不可
//
//	HTTP 2xx + errCode == SUCCESS   → 这次**调用**它收下了
//	refundStatus                    → 这笔退款**退成了没有**
//	refundOrderId 回显              → 这份应答说的是不是**我们这一笔**
//
// 第二层是本文件里最要紧的一处：老系统只看 errCode（refund_service.go:1000 那一带把
// errCode == SUCCESS 直接当成退款成功），而规范 §6 写着 PROCESSING / UNKNOWN 要再查。
// **PROCESSING 被记成成功就是「钱还没退、账上已经退了」**——用户来问的时候，我们手上没有
// 一笔待处理的退款，只有一条写着成功的流水。
//
//	refundStatus == SUCCESS → ResultSuccess
//	refundStatus == FAIL    → ResultFailed（渠道明确说退不成，可以换一条路退）
//	其余（PROCESSING / UNKNOWN / 空 / 不认识的词）→ ResultUnknown，出路是退款查询
//
// # 含糊码
//
// 非 SUCCESS 的 errCode 分两类，判据与下单那条路**共用同一个** ambiguousErrCode
// （DUP_ORDER / ORDER_PROCESSING / 应答里压根没有 errCode）：那三种下渠道有没有受理这次退款
// 是不知道的，标失败会让调用方拿**同一个退款单号**再退一次——而渠道对同一对
// merOrderId + refundOrderId 是幂等的，于是第二次会拿回第一张退货单，两边都以为退了。
func (p *Protocol) interpretRefund(body []byte, status, attempts int, req provider.OperationRequest) provider.OperationResult {
	result := newOperationResult(status, attempts, req, p.baseURL, "refund_inconclusive")

	var response refundResponse
	if err := json.Unmarshal(body, &response); err != nil {
		// 报文读不懂。**不能当失败**：对面可能已经受理了这笔退款，只是回了一句我们解不开的话。
		result.FailureCode = "UNPARSEABLE_RESPONSE"
		result.FailureMessage = "provider response is not json"
		if isHTTP4xx(result.HTTPStatus) {
			// 4xx 是例外：渠道在协议层就拒了这次请求（报文不合法、签名不对、商户号不认识），
			// 它没有受理。这个分支有用——配置写错时调用方能立刻知道该换一条路退。
			result.Result = provider.ResultFailed
		}
		return result
	}
	describeRefundResponse(&result, response)

	if !isHTTP2xx(result.HTTPStatus) {
		// HTTP 层就没过。状态码先于报文：一个 502 配一份写着 SUCCESS 的报文是自相矛盾的东西。
		result.FailureCode = fmt.Sprintf("HTTP_%d", result.HTTPStatus)
		result.FailureMessage = errMessage(response.ErrMsg, fmt.Sprintf("provider returned http %d", result.HTTPStatus))
		if isHTTP4xx(result.HTTPStatus) {
			result.Result = provider.ResultFailed
		}
		return result
	}

	if response.ErrCode != errCodeSuccess {
		if ambiguousErrCode(response.ErrCode) {
			// 见上面「含糊码」那一段：渠道可能已经受理了这笔退款。
			result.FailureCode = "AMBIGUOUS_PROVIDER_CODE"
			result.FailureMessage = errMessage(response.ErrMsg, "provider did not confirm the refund")
			return result
		}
		result.Result = provider.ResultFailed
		result.FailureCode = "provider_declined"
		result.FailureMessage = errMessage(response.ErrMsg, "provider declined the refund")
		return result
	}

	if mismatch := p.refundEchoMismatch(response, req); mismatch != "" {
		result.FailureCode = "refund_order_mismatch"
		result.FailureMessage = mismatch
		return result
	}

	switch strings.ToUpper(strings.TrimSpace(response.RefundStatus)) {
	case refundStatusSuccess:
		result.Result = provider.ResultSuccess
		result.FailureCode = "refund_reported_success"
		result.FailureMessage = ""
		return result
	case refundStatusFail:
		// 明确失败。**这一条可以信**：渠道说的是「这笔退款没有成」，调用方据此可以再退一次。
		result.Result = provider.ResultFailed
		result.FailureCode = "refund_reported_failure"
		result.FailureMessage = errMessage(response.ErrMsg, "provider reported the refund failed")
		return result
	default:
		// PROCESSING / UNKNOWN（refundStatusProcessing / refundStatusUnknown），以及空串与不认识
		// 的词，都落在这里：**渠道还没给出结论**。绝不能当成功，也不当失败——出路只有一个，
		// 就是拿这一对被问的那个 refundOrderId 去调退款查询。
		result.FailureMessage = fmt.Sprintf("provider refundStatus=%q is not final", response.RefundStatus)
		return result
	}
}

// interpretQueryRefund 把退款查询应答翻成 provider.OperationResult。
//
// # 它与发起退款那条路的差别只有一处，但它决定了这一整条路的形状
//
//	发起退款  「这次调用成不成 + 退款有没有结局」
//	退款查询  「这笔退款现在什么结局」
//
// 所以协议层没过时**一律 ResultUnknown**（与 interpretQuery 逐字相同）：渠道没有给我们关于
// 这笔退款的任何事实，而「查询失败」与「退款失败」是两件事。标失败会让调用方去重退一笔可能
// 已经退成的钱。
//
// 状态词的处置与 interpretRefund 相同，只是换了个 FailureCode 前缀：这个结论来自查询，
// 而不是一次新的发起（它进 payment_provider_calls，两件事在里面长得一模一样就没法排查了）。
func (p *Protocol) interpretQueryRefund(body []byte, status, attempts int, req provider.OperationRequest) provider.OperationResult {
	result := newOperationResult(status, attempts, req, p.baseURL, "refund_inconclusive")

	var response refundResponse
	if err := json.Unmarshal(body, &response); err != nil {
		// 读不懂的应答 = 没有结论，与「查询超时」同一条路。
		result.FailureCode = "UNPARSEABLE_RESPONSE"
		result.FailureMessage = "provider response is not json"
		return result
	}
	describeRefundResponse(&result, response)

	if !isHTTP2xx(result.HTTPStatus) || response.ErrCode != errCodeSuccess {
		result.FailureMessage = errMessage(response.ErrMsg, "provider did not answer the refund query")
		return result
	}

	if mismatch := p.refundEchoMismatch(response, req); mismatch != "" {
		result.FailureCode = "refund_order_mismatch"
		result.FailureMessage = mismatch
		return result
	}

	switch strings.ToUpper(strings.TrimSpace(response.RefundStatus)) {
	case refundStatusSuccess:
		result.Result = provider.ResultSuccess
		result.FailureCode = "query_reported_success"
		result.FailureMessage = ""
		return result
	case refundStatusFail:
		result.Result = provider.ResultFailed
		result.FailureCode = "query_reported_failure"
		result.FailureMessage = errMessage(response.ErrMsg, "provider reported the refund failed")
		return result
	default:
		// 还是 PROCESSING / UNKNOWN，或者一个我们不认识的词。渠道还没给出结论，调用方维持原状。
		result.FailureMessage = fmt.Sprintf("provider refundStatus=%q is not final", response.RefundStatus)
		return result
	}
}

// interpretClose 把关单应答翻成 provider.OperationResult。
//
// 判据只有 errCode 一层（关单不问「关掉之后怎么样」，问的是「这张单还开着吗」），三个分支与
// 退款那条路的前半段逐字同源：
//
//	errCode == SUCCESS           → ResultSuccess
//	含糊码（DUP_ORDER 等）        → ResultUnknown
//	其余非 SUCCESS               → ResultFailed
//
// # OPERATION_NOT_ALLOWED 落在最后那一档，这是**故意的**
//
// 规范的错误码表把它注成「订单已关闭」，但同一个码在别处也可能表示「这一单付过了、不许关」。
// 把它当成功等于我们替渠道认定「这张单关掉了」，而如果真相是后者，一笔真收了的钱会被当成
// 没付。不确定就维持现状——调用方（今天还没有）拿查单去定夺，那是有结论的那条路。
func (p *Protocol) interpretClose(body []byte, status, attempts int, req provider.OperationRequest) provider.OperationResult {
	result := newOperationResult(status, attempts, req, p.baseURL, "close_inconclusive")

	var response closeResponse
	if err := json.Unmarshal(body, &response); err != nil {
		// 与其余几条路一致：读不懂的应答不构成「关掉了」，但是 4xx 可以当失败（协议层就拒了）。
		result.FailureCode = "UNPARSEABLE_RESPONSE"
		result.FailureMessage = "provider response is not json"
		if isHTTP4xx(result.HTTPStatus) {
			result.Result = provider.ResultFailed
		}
		return result
	}
	result.ResponseSummary["providerCode"] = response.ErrCode
	if response.ErrMsg != "" {
		result.ResponseSummary["providerMessage"] = response.ErrMsg
	}

	if !isHTTP2xx(result.HTTPStatus) {
		result.FailureCode = fmt.Sprintf("HTTP_%d", result.HTTPStatus)
		result.FailureMessage = errMessage(response.ErrMsg, fmt.Sprintf("provider returned http %d", result.HTTPStatus))
		if isHTTP4xx(result.HTTPStatus) {
			result.Result = provider.ResultFailed
		}
		return result
	}

	if response.ErrCode != errCodeSuccess {
		if ambiguousErrCode(response.ErrCode) {
			result.FailureCode = "AMBIGUOUS_PROVIDER_CODE"
			result.FailureMessage = errMessage(response.ErrMsg, "provider did not confirm the close")
			return result
		}
		result.Result = provider.ResultFailed
		result.FailureCode = "provider_declined"
		result.FailureMessage = errMessage(response.ErrMsg, "provider declined the close")
		return result
	}

	result.Result = provider.ResultSuccess
	result.FailureCode = "close_accepted"
	result.FailureMessage = ""
	return result
}

// newOperationResult 造一份「什么都还没断定」的结果，三条操作的解释函数共用。
//
// 默认值是 ResultUnknown：这些路里的每一条在拿到证据之前，正确的答案都是「不知道」。
// 让每条路各自字面量地写一遍零值，改判据时漏掉一处的表现是某条路默认判成功。
//
// inconclusive 是「还没断定」这个结局的 FailureCode，由调用方给（退款那条路是
// `refund_inconclusive`，关单是 `close_inconclusive`）——**它必须在默认值里就有**：
// 这个结论进 payment_provider_calls，空着一栏意味着运维在流水里看到一条既不是成功也不是
// 失败的记录，而没有任何文字说明它为什么悬着。与查单那条路的 `query_inconclusive` 同源。
func newOperationResult(status, attempts int, req provider.OperationRequest, baseURL, inconclusive string) provider.OperationResult {
	return provider.OperationResult{
		Result:      provider.ResultUnknown,
		HTTPStatus:  status,
		Attempts:    attempts,
		FailureCode: inconclusive,
		ResponseSummary: map[string]any{
			"httpStatus": status,
			// 渠道地址的主机名进摘要，是为了让「沙箱单打到了生产渠道上」这类事故在流水里
			// 看得见（与下单那条路一致，理由见 create.go 的 interpretCreate）。
			"baseURLHost": hostOf(baseURL),
		},
	}
}

// describeRefundResponse 把退款应答里的几个事实写进脱敏摘要。
//
// 摘要里**只有渠道给的字**（错误码、状态词、单号、金额），没有我们发出去的报文，也没有任何
// 凭据——与其余几条路的口径一致。
//
// RefundAmount 进摘要而不是进某个字段：OperationResult 没有金额这一栏（它描述的是一次
// **调用**，不是一笔**退款**的记录），而排查「渠道说的退款金额和我们记的差多少」时它是第一个
// 要看的。
func describeRefundResponse(result *provider.OperationResult, response refundResponse) {
	result.ResponseSummary["providerCode"] = response.ErrCode
	if response.ErrMsg != "" {
		result.ResponseSummary["providerMessage"] = response.ErrMsg
	}
	result.ResponseSummary["providerRefundStatus"] = response.RefundStatus
	if id := strings.TrimSpace(response.RefundOrderID); id != "" {
		result.ResponseSummary["providerRefundOrderId"] = id
	}
	if amount, ok := amountFen(response.RefundAmount); ok && amount > 0 {
		result.ResponseSummary["refundAmount"] = amount
	}
}

// refundEchoMismatch 检查应答回显的两个单号是不是我们问的那一笔，不是则说明哪里对不上。
//
// 两个方向都要看：
//
//   - **merOrderId 对不上**：这份应答说的是另一笔支付。老系统查单那条路上有同样的判据
//     （RecoverStuckCoffeeOrder 里 `responseOrderNo != order.OrderNo` 直接转人工）。
//   - **refundOrderId 对不上**：这份应答说的是同一笔支付上的**另一次退款**。这一条是本文件
//     独有的，也是 refundQueryBody 带上 refundOrderId 的理由——退款查询的报文里若渠道不认
//     那个参数，回显很可能就是「最近一次退款」，而把另一次的状态记到这一笔上，轻则重复退款、
//     重则少退一笔钱。
//
// 空值**不算对不上**：渠道可以不回显（我们与它之间没有文档约定它一定回）。空着时这份应答的
// 单号信息量是零，但状态词本身仍然是我们问的那一笔的——它由别的字段（merOrderId）锚定。
func (p *Protocol) refundEchoMismatch(response refundResponse, req provider.OperationRequest) string {
	// 与查单那条路逐字同一种比法（见 query.go）：两边都先解回内部支付单号再比，因为渠道
	// 可能回显我们发的前缀单号，也可能只回显它自己记账用的那一段。
	if echoed := strings.TrimSpace(response.MerOrderID); echoed != "" && p.paymentNoOf(echoed) != req.PaymentNo {
		return "provider answered about a different merchant order"
	}
	// refundOrderId 同理：我们发出去的是**带前缀**的那个（见 refund()），但这条渠道的回显既可能
	// 原样带回、也可能只回它记账用的那一段，所以两边都先解回内部退款单号再比。直接拿回显去比
	// 我们手里的裸单号会把**每一次成功**都读成「对不上」——而它的后果是调用方拿同一个退款单号
	// 再退一次。
	if echoed := strings.TrimSpace(response.RefundOrderID); echoed != "" &&
		p.paymentNoOf(echoed) != p.paymentNoOf(strings.TrimSpace(req.RefundNo)) {
		return "provider answered about a different refund order"
	}
	return ""
}
