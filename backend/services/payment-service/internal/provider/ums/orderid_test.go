package ums

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 这一组守的是 orderid.go 那对函数的**两个方向**，以及它们在适配器里的接线。
//
// 那个前缀不是格式偏好，是账户规则：2026-09-21 拿真实账户打 H5，裸支付单号被银联商务的收银台
// 挡在门外（「处理失败: 无效订单号，订单号必须以3CYM开头」）。所以这一组要验的**不是**「前缀
// 加上了」——那只是出口的一半。真正承重的是另一半：**渠道按那个写法送回来的号，被解回我们的
// 支付单号**。只做出口的话，钱收了、回调认不出是哪一单，支付单永远停在待支付。
//
// 出口那半由各条路自己的用例守着（create_test / create_h5_test / query_test / operator_test
// 都断言了报文里的字面值），这里补的是入口那半——它此前没有任何用例：老的值没有前缀，
// paymentNoOf 对它是恒等的，所以那些用例在改动前后都绿，一个字节都没验到。

// TestChannelOrderIDRoundTrips 钉住这一对函数互为反函数。
func TestChannelOrderIDRoundTrips(t *testing.T) {
	protocol, err := Parse(channelConfig("https://api-mop.chinaums.com"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	const paymentNo = "PAY20260917000000000001"

	channelOrderID := protocol.channelOrderID(paymentNo)
	if channelOrderID != "3CYM"+paymentNo {
		t.Fatalf("channelOrderID = %q，want sourceCode + 支付单号", channelOrderID)
	}
	if got := protocol.paymentNoOf(channelOrderID); got != paymentNo {
		t.Fatalf("解回来 = %q，want %q——这一对不互逆的话，回调落不到任何一单上", got, paymentNo)
	}

	// 不带前缀的值原样返回。两种东西走这条路：这条改动**之前**发出去、渠道还没结清的旧单，
	// 以及别家商户的单被打到我们这个账户上。两者都不该因为「格式不认得」被拒收——前者的钱是
	// 真收了，后者要留一行痕让运维看得见（见 paymentNoOf 的注释）。
	if got := protocol.paymentNoOf(paymentNo); got != paymentNo {
		t.Fatalf("不带前缀时应当原样返回，got %q", got)
	}
}

// TestVerifyStripsTheChannelOrderIDPrefixFromACallback 是这一组的主干：一份**按渠道写法**送回来的
// 成功回调，解出来的必须是我们的支付单号。
func TestVerifyStripsTheChannelOrderIDPrefixFromACallback(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")
	body := callbackBody(t, map[string]any{
		"merOrderId":  "3CYMPAY20260917000000000001",
		"status":      "TRADE_SUCCESS",
		"totalAmount": 12800,
	})

	notification, err := New().Verify(context.Background(), signedCallback(t, config, testAppID, testSecret, body))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if notification.EventType != provider.EventSucceeded {
		t.Fatalf("EventType = %q", notification.EventType)
	}
	if notification.PaymentNo != "PAY20260917000000000001" {
		t.Fatalf("PaymentNo = %q，want 内部支付单号——解不出前缀，这一笔结算不到任何一张支付单上",
			notification.PaymentNo)
	}
	// 去重键是例外：它留渠道的原始写法。那一列记的是「渠道说它发的这条通知叫什么」，属于入站
	// 事实（见 notificationID 的注释），而按支付单找通知这张表自己有 payment_no 列。
	if notification.NotificationID != "3CYMPAY20260917000000000001:TRADE_SUCCESS" {
		t.Fatalf("NotificationID = %q，want 渠道侧的原始写法", notification.NotificationID)
	}
}

// TestVerifyReturnStripsTheChannelOrderIDPrefix 回跳那条路同样要解。
//
// 它比回调那条更早撞上这个前缀：回跳的单号会被拼进结果页地址交给用户，解不出来的话用户看到
// 的地址里挂着渠道的单号（并且调用方拿去库里找那一单会找不到）。
func TestVerifyReturnStripsTheChannelOrderIDPrefix(t *testing.T) {
	config := commKeyConfig("https://api-mop.chinaums.com")
	params := map[string]string{
		"merOrderId": "3CYMPAY20260917000000000001",
		"signType":   signTypeMD5,
	}
	signature := legacyKeyAppendSignature(params, testCommSecret, params[paramSignType])
	query := url.Values{}
	for name, value := range params {
		query.Set(name, value)
	}
	query.Set("sign", signature)

	notification, err := New().Verify(context.Background(), provider.NotificationRequest{
		ChannelCode: "ums_wechat",
		Headers:     http.Header{},
		HTTPMethod:  http.MethodGet,
		RequestPath: "/v1/payments/return/ums_wechat",
		Query:       query,
		Method:      channelMethod(config),
		Secrets:     commCredentials(),
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if notification.EventType != eventReturn {
		t.Fatalf("EventType = %q, want %q", notification.EventType, eventReturn)
	}
	if notification.PaymentNo != "PAY20260917000000000001" {
		t.Fatalf("PaymentNo = %q，want 内部支付单号", notification.PaymentNo)
	}
}

// TestQueryAcceptsAPrefixedEcho 钉住回显比对的**两个方向都认**。
//
// 渠道可能把我们发出去的那个号原样回显，也可能只回显它自己记账用的那一段——我们与它之间没有
// 文档约定是哪一种。只比字符串的话，后一种会被读成「这份应答说的是另一笔」，于是主动查单一轮
// 一轮地问，每一轮都拿到「还是不知道」：症状是积压的单永远结不掉，而日志里一句错都没有。
func TestQueryAcceptsAPrefixedEcho(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errCode":"SUCCESS","status":"TRADE_SUCCESS",` +
			`"merOrderId":"3CYMPAY20260917000000000001","targetOrderId":"T-9001","totalAmount":12800}`))
	})

	result, err := New().Query(context.Background(), queryRequestFor(channelConfig(server.URL)))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Result != provider.ResultSuccess {
		t.Fatalf("Result = %q（%s %s）——渠道按我们发出去的写法回显时必须认下来",
			result.Result, result.FailureCode, result.FailureMessage)
	}
	if result.ProviderTransactionID != "T-9001" {
		t.Fatalf("ProviderTransactionID = %q", result.ProviderTransactionID)
	}
}

// TestRefundAcceptsBothEchoShapesOfTheRefundNumber 把上一条的同一种判据搬到退款路上。
//
// 退款比查单更需要它：refundOrderId 对不上被读成「这份应答说的是另一次退款」之后，它的后果不是
// 多问一轮，而是调用方拿**同一个**退款单号再退一次。而这条路上我们发出去的是**带前缀**的那个
// （规范要求，见 orderid.go 末尾那一节），回显却不保证跟着带——两种形状都得认。
//
// 两笔各自的 merOrderId 都回显带前缀那一版，好让这条测试只盯着退款单号这一个变量。反过来的
// 那一半（merOrderId 只回显裸单号）由 TestQueryAcceptsAPrefixedEcho 钉着，两条路走的是同一个
// paymentNoOf。
func TestRefundAcceptsBothEchoShapesOfTheRefundNumber(t *testing.T) {
	const usedRefundNo = "REF20260917000000000001"

	for _, shape := range []struct {
		name     string
		echoedID string
	}{
		{"原样回显带前缀的", "3CYM" + usedRefundNo},
		{"只回显它记账用的那一段", usedRefundNo},
	} {
		t.Run(shape.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"errCode":"SUCCESS","refundStatus":"SUCCESS",` +
					`"merOrderId":"3CYMPAY20260917000000000001",` +
					`"refundOrderId":"` + shape.echoedID + `","refundAmount":"12800"}`))
			})

			result, err := New().Execute(context.Background(),
				operationRequestFor(channelConfig(server.URL), provider.OperationRefund))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Result != provider.ResultSuccess {
				t.Fatalf("Result = %q（%s %s）——回显的写法与发出去的不一致，但说的是同一笔退款",
					result.Result, result.FailureCode, result.FailureMessage)
			}
		})
	}
}

// TestRefundRejectsAnEchoAboutAnotherRefund 是上一条的反面：真要退的是**另一笔**时必须读出来。
//
// 少了这一条，把 refundEchoMismatch 整个删掉也能让上面那条全绿——而它守的正是「别把另一次退款的
// 状态记到这一笔上」。
func TestRefundRejectsAnEchoAboutAnotherRefund(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errCode":"SUCCESS","refundStatus":"SUCCESS",` +
			`"merOrderId":"3CYMPAY20260917000000000001",` +
			`"refundOrderId":"3CYMREF20260917999999999999","refundAmount":"12800"}`))
	})

	result, err := New().Execute(context.Background(),
		operationRequestFor(channelConfig(server.URL), provider.OperationRefund))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Result == provider.ResultSuccess {
		t.Fatalf("回显的是另一次退款，却判成了成功：%s", result.ResponseSummary)
	}
}
