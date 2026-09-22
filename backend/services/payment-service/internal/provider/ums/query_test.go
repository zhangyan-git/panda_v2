package ums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

func queryRequestFor(config map[string]any) provider.QueryRequest {
	return provider.QueryRequest{
		PaymentNo: "PAY20260917000000000001",
		OrderNo:   "ORD20260917000000000001",
		RequestID: "req-1",
		Method:    channelMethod(config),
		Secrets:   credentialsFor(testSecret),
	}
}

// TestQuerySendsASignedBodyWithoutAppID 钉死这一族最容易被「补全」错的一处：**查单报文里
// 没有 appId**，而签名头照旧用它签。
//
// 多一个 appId 字段不会让签名失败（待签串不覆盖字段集），只会让渠道判参数不合法——而那时
// 你会去查签名。
func TestQuerySendsASignedBodyWithoutAppID(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errCode":"SUCCESS","status":"TRADE_SUCCESS",` +
			`"merOrderId":"PAY20260917000000000001","targetOrderId":"T-9001","totalAmount":12800}`))
	})

	result, err := New().Query(context.Background(), queryRequestFor(channelConfig(server.URL)))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Result != provider.ResultSuccess {
		t.Fatalf("Result = %q（%s %s）", result.Result, result.FailureCode, result.FailureMessage)
	}
	if result.ProviderTransactionID != "T-9001" {
		t.Fatalf("ProviderTransactionID = %q，回调要靠它对上号", result.ProviderTransactionID)
	}
	if result.ResponseSummary["totalAmount"] != int64(12800) {
		t.Fatalf("totalAmount 该按分记进摘要：%v", result.ResponseSummary["totalAmount"])
	}

	request := server.verifySignature(t, testSecret)
	if request.method != http.MethodPost {
		t.Fatalf("method = %q, want POST（老系统走同一条 doUMSPost 出网）", request.method)
	}
	if request.path != defaultQueryPath {
		t.Fatalf("path = %q, want %q", request.path, defaultQueryPath)
	}

	var sent map[string]any
	if err := json.Unmarshal(request.body, &sent); err != nil {
		t.Fatalf("解报文：%v（%s）", err, request.body)
	}
	if _, ok := sent["appId"]; ok {
		t.Fatalf("查单报文里**不该有 appId**：%s", request.body)
	}
	// 查单按这个号认那一笔，所以它的编码必须与下单时**逐字一致**（见 orderid.go）。不一致的
	// 症状很安静：渠道答「查无此单」，被我们读成「还是不知道」，主动查单每轮都在问却永远问不出
	// 结论。
	if sent["merOrderId"] != "3CYMPAY20260917000000000001" {
		t.Fatalf("merOrderId = %v，want 与下单同一个编码", sent["merOrderId"])
	}
	if sent["mid"] != testMid || sent["tid"] != testTid {
		t.Fatalf("mid/tid 该取配置：%s", request.body)
	}
	// msgId 是「这一次查询」的请求号，与支付单号无关：老系统给它的是 QUERY + 纳秒。
	msgID, _ := sent["msgId"].(string)
	if !strings.HasPrefix(msgID, "QUERY") || len(msgID) <= len("QUERY") {
		t.Fatalf("msgId = %q，该是 QUERY 前缀的请求号", msgID)
	}
	// 报文里也不该出现下单专有的那几个字段。
	for _, absent := range []string{"instMid", "tradeType", "notifyUrl", "totalAmount"} {
		if _, ok := sent[absent]; ok {
			t.Fatalf("查单报文里不该有 %s：%s", absent, request.body)
		}
	}
}

// TestQueryClassification：**与下单不同的那两条**。
//
// 同一份应答，两个方向要回答的问题不同：下单问「这次调用成不成」，查单问「这一笔现在什么
// 结局」。所以 errCode 只是入场券，真正的判据是 status。
func TestQueryClassification(t *testing.T) {
	successPrefix := `{"errCode":"SUCCESS","merOrderId":"PAY20260917000000000001",`
	cases := []struct {
		name     string
		status   int
		body     string
		want     provider.Result
		wantCode string
	}{
		{
			// FailureCode 在成功时也非空，是 provider.Querier 点名要的：**要能看出这个结论
			// 来自查单，而不是一次新的发起**（它进 payment_provider_calls，
			// 两件事在里面长得一模一样就没法排查了）。它不会污染 payments.failure_code——
			// 成功那条路走的是 markPending，那儿压根没读失败字段。
			name: "查到了成功", status: 200,
			body: successPrefix + `"status":"TRADE_SUCCESS","targetOrderId":"T-1"}`,
			want: provider.ResultSuccess, wantCode: "query_reported_success",
		},
		{
			// 老系统那个 switch 里三个写法都算成功（三处各抄了一遍，这里抄成一处）。
			name: "大写 SUCCESS", status: 200,
			body: successPrefix + `"status":"SUCCESS"}`,
			want: provider.ResultSuccess, wantCode: "query_reported_success",
		},
		{
			name: "小写 success", status: 200,
			body: successPrefix + `"status":"success"}`,
			want: provider.ResultSuccess, wantCode: "query_reported_success",
		},
		{
			// 明确失败。**这一条可以信**：渠道说的是「这笔没有成」，调用方据此落 failed
			// 并让用户换一种方式付。
			name: "已关闭", status: 200,
			body: successPrefix + `"status":"TRADE_CLOSED"}`,
			want: provider.ResultFailed, wantCode: "query_reported_failure",
		},
		{
			name: "还没付", status: 200,
			body: successPrefix + `"status":"WAIT_BUYER_PAY"}`,
			want: provider.ResultUnknown, wantCode: "query_inconclusive",
		},
		{
			name: "刚建单", status: 200,
			body: successPrefix + `"status":"NEW_ORDER"}`,
			want: provider.ResultUnknown, wantCode: "query_inconclusive",
		},
		{
			// **退款发生在收款之后**：把 TRADE_REFUND 判成失败，会让一笔已经收妥的单被标
			// 失败、给订单发一条 payment.failed。
			name: "退款", status: 200,
			body: successPrefix + `"status":"TRADE_REFUND"}`,
			want: provider.ResultUnknown, wantCode: "query_inconclusive",
		},
		{
			// 不认识的词不能当失败，也不能当成功（当成功会推进 pending，而那一步的含义是
			// 「钱收了、在等回调」）。
			name: "不认识的状态", status: 200,
			body: successPrefix + `"status":"WHATEVER"}`,
			want: provider.ResultUnknown, wantCode: "query_inconclusive",
		},
		{
			// errCode 不是 SUCCESS：渠道这边没有给我们关于这一笔的任何事实。
			name: "errCode 不是成功", status: 200,
			body: `{"errCode":"ORDER_NOT_EXIST","errMsg":"查不到这一单","status":"TRADE_SUCCESS"}`,
			want: provider.ResultUnknown, wantCode: "query_inconclusive",
		},
		{
			name: "协议层失败", status: 502,
			body: successPrefix + `"status":"TRADE_SUCCESS"}`,
			want: provider.ResultUnknown, wantCode: "query_inconclusive",
		},
		{
			name: "报文读不懂", status: 200,
			body: `<html>`,
			want: provider.ResultUnknown, wantCode: "query_inconclusive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			result, err := New().Query(context.Background(), queryRequestFor(channelConfig(server.URL)))
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if result.Result != tc.want {
				t.Fatalf("Result = %q, want %q（%s）", result.Result, tc.want, result.FailureMessage)
			}
			if result.FailureCode != tc.wantCode {
				t.Fatalf("FailureCode = %q, want %q", result.FailureCode, tc.wantCode)
			}
		})
	}
}

// TestQueryRejectsAnswersAboutAnotherOrder：渠道回显的商户单号对不上时，这份应答说的是**另一笔**。
//
// 老系统同样处置（RecoverStuckCoffeeOrder `responseOrderNo != order.OrderNo` 直接转人工），
// 而这里的处置是「还是不知道」——绝不能拿别人的状态去改这一单。
func TestQueryRejectsAnswersAboutAnotherOrder(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errCode":"SUCCESS","status":"TRADE_SUCCESS",` +
			`"merOrderId":"PAY-SOMEBODY-ELSE","targetOrderId":"T-9001"}`))
	})

	result, err := New().Query(context.Background(), queryRequestFor(channelConfig(server.URL)))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if result.Result != provider.ResultUnknown || result.FailureCode != "query_order_mismatch" {
		t.Fatalf("Result=%q FailureCode=%q，别人的应答不该被采信", result.Result, result.FailureCode)
	}
	if result.ProviderTransactionID != "" {
		t.Fatalf("别人的交易号不该被带出来：%q", result.ProviderTransactionID)
	}
}

// TestQueryAmountAcceptsBothShapes：金额在两种投递方式下类型不同（JSON 数字 / 字符串），
// 两个都要认，且都以**分**记进摘要。
func TestQueryAmountAcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{"数字", `12800`, 12800},
		{"字符串", `"12800"`, 12800},
		{"带小数的字符串", `"12800.00"`, 12800},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"errCode":"SUCCESS","status":"TRADE_SUCCESS",` +
					`"merOrderId":"PAY20260917000000000001","totalAmount":` + tc.raw + `}`))
			})

			result, err := New().Query(context.Background(), queryRequestFor(channelConfig(server.URL)))
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if result.ResponseSummary["totalAmount"] != tc.want {
				t.Fatalf("totalAmount = %v, want %d（分）", result.ResponseSummary["totalAmount"], tc.want)
			}
		})
	}
}

// TestQueryNeverConcludesFromATransportFailure：查单失败**不等于支付失败**。
//
// 它只等于「我们还是不知道」，调用方据此维持既有处置（支付单停在 created、由超时关单收走）。
// 标 failed 会把一笔可能已收的钱从账上抹掉。
func TestQueryNeverConcludesFromATransportFailure(t *testing.T) {
	t.Run("超时", func(t *testing.T) {
		release := make(chan struct{})
		server := newChannelServer(t)
		server.setHandler(func(w http.ResponseWriter, r *http.Request) { <-release })
		t.Cleanup(func() { close(release) })

		adapter := &Provider{client: httpx.New(50 * time.Millisecond)}
		result, err := adapter.Query(context.Background(), queryRequestFor(channelConfig(server.URL)))
		if err == nil {
			t.Fatal("查单没拿到结论时该返回 error，调用方据此把这次调用记进流水")
		}
		if result.Result != provider.ResultUnknown || result.FailureCode != "QUERY_NOT_ANSWERED" {
			t.Fatalf("Result=%q FailureCode=%q", result.Result, result.FailureCode)
		}
	})

	t.Run("根本没发出去", func(t *testing.T) {
		server := newChannelServer(t)
		url := server.URL
		server.Close()

		result, err := New().Query(context.Background(), queryRequestFor(channelConfig(url)))
		if !errors.Is(err, httpx.ErrNotSent) {
			t.Fatalf("want ErrNotSent, got %v", err)
		}
		if result.Result != provider.ResultUnknown {
			t.Fatalf("Result = %q, want unknown", result.Result)
		}
	})
}

// TestQueryRefusesToGuessWithoutThePinnedInputs：配置解不开、密钥是空的，两条都不出网。
//
// 这里与 Create 有一处**有意的不同**：两种情况下返回的 err 都不是 ErrSecretNotConfigured
// 那种「拒掉了这笔支付」的语义，而是「这次查询做不了」——调用方对两者的处置相同（维持不明），
// 但对它们要能分辨。
func TestQueryRefusesToGuessWithoutThePinnedInputs(t *testing.T) {
	cases := []struct {
		name    string
		damage  func(*provider.QueryRequest)
		wantErr error
	}{
		{
			name: "配置解不开",
			damage: func(r *provider.QueryRequest) {
				r.Method = channelMethod(map[string]any{"baseURL": "https://api-mop.chinaums.com"})
			},
			wantErr: provider.ErrConfigInvalid,
		},
		{
			name:    "密钥是空的",
			damage:  func(r *provider.QueryRequest) { r.Secrets = credentialsFor("  ") },
			wantErr: provider.ErrSecretNotConfigured,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			request := queryRequestFor(channelConfig(server.URL))
			tc.damage(&request)

			result, err := New().Query(context.Background(), request)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if result.Result != provider.ResultUnknown {
				t.Fatalf("Result = %q, want unknown（查不了不等于支付失败）", result.Result)
			}
			if sent := server.requestCount(); sent != 0 {
				t.Fatalf("本地拒时**一个请求都不该发出去**，实际发了 %d 个", sent)
			}
		})
	}
}
