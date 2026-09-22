package ums

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// queryBody 是 /v1/netpay/query 的报文，逐字照抄老系统 QueryOrder:258 的那个 map。
//
// **不含 appId**：老系统这条报文里没有它，而签名头照旧用 appId 签。这是这一族里最容易被
// 「补全」错的一处——多一个 appId 字段不会让签名失败（待签串不覆盖字段集），只会让渠道判
// 参数不合法，而那时你会去查签名。
//
// 也不含 instMid / tradeType / notifyUrl：查单按商户单号认这一笔，其余参数都是下单时定下的。
type queryBody struct {
	MsgID            string `json:"msgId"`
	RequestTimestamp string `json:"requestTimestamp"`
	MerOrderID       string `json:"merOrderId"`
	Mid              string `json:"mid"`
	Tid              string `json:"tid"`
}

// queryResponse 是查单应答。
//
// 比 createResponse 多几个字段，因为它们正是查单要回答的问题：这一笔现在什么状态、渠道侧的
// 交易号是多少。字段名照抄老系统 RecoverStuckCoffeeOrder:1990 读的那几个键。
type queryResponse struct {
	ErrCode string `json:"errCode"`
	ErrMsg  string `json:"errMsg"`
	Status  string `json:"status"`
	// MerOrderID 是渠道回显的商户单号。它与我们发出去的那个对不上时**不能**采信这份应答
	// （见 interpretQuery 里那一段）。
	MerOrderID    string `json:"merOrderId"`
	TargetOrderID string `json:"targetOrderId"`
	TransactionID string `json:"transactionId"`
	// TotalAmount 与回调一样可能以数字或字符串到达，所以走同一个解码器。
	TotalAmount json.RawMessage `json:"totalAmount"`
}

// Query 实现 provider.Querier：拿商户单号去渠道问这一笔到底成没成。
//
// 这条渠道有这个接口（老系统 QueryOrder → POST /v1/netpay/query），而它对 V2 是**必需品**
// 而不是锦上添花：这一族的下单接口会超时（httpx 的 10 秒），超时之后我们手上只有两个都不对
// 的选项——标失败（可能钱已经收了）、或者等超时关单（用户付过的那笔会被关掉）。去问渠道是
// 唯一有结论的出路，见 provider.Querier 与 service 里 ErrProviderResultUncertain 的注释。
//
// 查单**照旧签名**：老系统的 QueryOrder 走 doUMSPost，与下单同一条出网路径。这里也走
// Provider.post，所以两条路不可能在签名上漂移。
func (p *Provider) Query(ctx context.Context, req provider.QueryRequest) (provider.CreateResult, error) {
	protocol, err := Parse(req.Method.ChannelConfig)
	if err != nil {
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: %w", req.Method.ChannelCode, err)
	}
	secret := req.Secrets.Get(protocol.secretRef)
	if secret == "" {
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: %w", req.Method.ChannelCode, provider.ErrSecretNotConfigured)
	}

	// msgId 是「这一次查询」的请求号，与支付单号无关：老系统给它的是 `QUERY` + 纳秒。
	// 照抄这个前缀——渠道侧的对账与排查按它认「这是一条查单请求」。
	//
	// merOrderId 走的是与下单同一个编码（见 orderid.go）：查单按这个号认那一笔，编码与发起
	// 时不一致的话，问的是一张渠道那边不存在的单——而它的回答会是「查无此单」，被我们读成
	// 「还是不知道」。这条路的症状因此格外安静：主动查单每一轮都在问，却永远问不出结论。
	body, err := json.Marshal(queryBody{
		MsgID:            "QUERY" + strconv.FormatInt(time.Now().UnixNano(), 10),
		RequestTimestamp: time.Now().Format(requestTimestampLayout),
		MerOrderID:       protocol.channelOrderID(req.PaymentNo),
		Mid:              protocol.mid,
		Tid:              protocol.tid,
	})
	if err != nil {
		// 五个字符串的 Marshal 不会失败；留着这个分支是为了以后加字段时不会静默吞掉错误。
		return provider.CreateResult{Result: provider.ResultUnknown},
			fmt.Errorf("channel %q: encode query body: %w", req.Method.ChannelCode, err)
	}

	response, err := p.post(ctx, protocol, secret, protocol.queryPath, body)
	if err != nil {
		return protocol.queryTransportFailure(req, err)
	}
	return protocol.interpretQuery(response.Body, response.StatusCode, response.Attempts, req), nil
}

// queryTransportFailure 把一次查单出网失败翻成 provider.CreateResult。
//
// 与下单那条路的差别只有一处，但它是**方向性的**：这里一律 ResultUnknown，而且带上原始
// error。查单失败**不等于支付失败**——它只等于「我们还是不知道」，调用方据此维持既有的处置
// （支付单停在 created，由超时关单收走）。标 failed 会把一笔可能已收的钱从账上抹掉。
//
// 返回 error 而不是吞掉它，是为了让调用方能记一条 payment_provider_calls 并区分「这个渠道
// 根本没配查单」与「查了但对面没答」（见 hmacbody 里同样的取舍）。
func (p *Protocol) queryTransportFailure(req provider.QueryRequest, err error) (provider.CreateResult, error) {
	return provider.CreateResult{
		Result:          provider.ResultUnknown,
		FailureCode:     "QUERY_NOT_ANSWERED",
		FailureMessage:  err.Error(),
		ResponseSummary: map[string]any{"baseURLHost": hostOf(p.baseURL)},
	}, fmt.Errorf("channel %q: query request failed: %w", req.Method.ChannelCode, err)
}

// interpretQuery 把查单应答翻成 provider.CreateResult。
//
// # 与 interpretCreate 的差别是**语义上的**
//
//	下单  「这次调用成不成？」→ errCode 说成功就等于渠道收了单
//	查单  「这一笔现在什么结局？」→ 只有 TRADE_SUCCESS 那一组词才意味着钱收了
//
// 所以这里**不看 errCode 之外的任何「像成功」的信号**：errCode 不是 SUCCESS 一律「还是不知道」，
// 而状态词表照抄老系统 RecoverStuckCoffeeOrder:2016 的那个 switch——
//
//	TRADE_SUCCESS / SUCCESS / success → 明确成功
//	TRADE_CLOSED                      → 明确失败（订单那边同步置为已关闭）
//	WAIT_BUYER_PAY / NEW_ORDER / 其余 → **不明**（仍未结）
//
// # 「不明」是这里的默认，不是兜底
//
// 把不认识的词判成 failed，会让一笔其实成了的单被标失败、给订单发一条 payment.failed，而用户
// 手上的钱是真的扣了。反过来判成 success 更糟：调用方会把它推进 pending（那一步的含义是
// 「钱收了、在等回调」）。两者都比「再等一会儿」差，而「再等一会儿」的代价只是一次超时关单。
func (p *Protocol) interpretQuery(body []byte, status, attempts int, req provider.QueryRequest) provider.CreateResult {
	result := provider.CreateResult{
		Result:      provider.ResultUnknown,
		HTTPStatus:  status,
		Attempts:    attempts,
		FailureCode: "query_inconclusive",
		ResponseSummary: map[string]any{
			"httpStatus":  status,
			"baseURLHost": hostOf(p.baseURL),
		},
	}

	var response queryResponse
	if err := json.Unmarshal(body, &response); err != nil {
		// 读不懂的应答 = 没有结论，与「查单超时」同一条路。
		result.FailureMessage = "provider response is not json"
		return result
	}
	result.ResponseSummary["providerCode"] = response.ErrCode
	if response.ErrMsg != "" {
		result.ResponseSummary["providerMessage"] = response.ErrMsg
	}

	if !isHTTP2xx(status) || response.ErrCode != errCodeSuccess {
		// 协议层就没过（HTTP 非 2xx，或者 errCode 不是 SUCCESS）。**不当失败**：渠道这边没有
		// 给我们关于这一笔的任何事实，而「查单失败」与「支付失败」是两件事。
		result.FailureMessage = errMessage(response.ErrMsg, "provider did not answer the query")
		return result
	}

	// 渠道回显的商户单号对不上：这份应答说的是**另一笔**。老系统同样处置
	// （RecoverStuckCoffeeOrder:2012 `responseOrderNo != order.OrderNo` 直接转人工重试），
	// 而这里的处置是「还是不知道」——绝不能拿别人的状态去改这一单。
	//
	// 两边都先解回内部支付单号再比（见 orderid.go）：渠道可能原样回显我们发的前缀单号，也可能
	// 只回显它自己记账用的那一段，两种写法说的是同一笔，只比字符串会把后者读成「对不上」。
	echoed := strings.TrimSpace(response.MerOrderID)
	if echoed != "" && p.paymentNoOf(echoed) != req.PaymentNo {
		result.FailureCode = "query_order_mismatch"
		result.FailureMessage = "provider answered about a different merchant order"
		return result
	}

	providerStatus := strings.TrimSpace(response.Status)
	result.ResponseSummary["providerStatus"] = providerStatus
	result.ProviderTransactionID = firstNonEmpty(response.TargetOrderID, response.TransactionID)
	if amount, ok := amountFen(response.TotalAmount); ok && amount > 0 {
		// 金额进摘要而不是进某个字段：CreateResult 没有金额这一栏（它是一次**调用**的描述，
		// 不是一笔**支付**的记录），而排查「对面说的金额和我们记的差多少」时它是第一个要看的。
		result.ResponseSummary["totalAmount"] = amount
	}

	switch {
	case isSucceededStatus(providerStatus):
		result.Result = provider.ResultSuccess
		result.FailureCode = "query_reported_success"
		result.FailureMessage = ""
		return result
	case strings.EqualFold(providerStatus, statusTradeClosed):
		// 明确失败。**这一条可以信**：渠道说的是「这笔没有成」，与上面那些含糊状态不同，
		// 调用方据此落 failed 并让用户换一种方式付。
		result.Result = provider.ResultFailed
		result.FailureCode = "query_reported_failure"
		result.FailureMessage = errMessage(response.ErrMsg, "provider reported "+statusTradeClosed)
		return result
	default:
		// WAIT_BUYER_PAY / NEW_ORDER / 不认识的词：都还没结。
		result.FailureMessage = fmt.Sprintf("provider status=%q is not final", providerStatus)
		return result
	}
}

// firstNonEmpty 取第一个非空的值，全空返回空串。
//
// 查单应答里的渠道交易号有几个候选名（targetOrderId / transactionId），老系统按这个顺序取。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
