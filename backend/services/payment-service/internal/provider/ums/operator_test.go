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

// 这一族对已有支付单的三条操作。报文形状的依据是官方样例（见 operator.go 抬头），
// 所以这里的断言是**照样例写的**，不是照本包实现反推的。
var _ provider.Operator = (*Provider)(nil)

func operationRequestFor(config map[string]any, operation provider.Operation) provider.OperationRequest {
	return provider.OperationRequest{
		Operation: operation,
		PaymentNo: "PAY20260917000000000001",
		OrderNo:   "ORD20260917000000000001",
		RefundNo:  "REF20260917000000000001",
		Amount:    12800,
		Reason:    "用户申请退款",
		RequestID: "req-1",
		Method:    channelMethod(config),
		Secrets:   credentialsFor(testSecret),
	}
}

// TestOperatorSendsASignedBody 是三条操作共用的第一组形状断言：认证走的是**出站**那套
// （OPEN-BODY-SIG 请求头、appKey 签），与小程序下单同一条路——不是 H5 那套 OPEN-FORM-PARAM。
func TestOperatorSendsASignedBody(t *testing.T) {
	cases := []struct {
		name      string
		operation provider.Operation
		wantPath  string
	}{
		{"退款", provider.OperationRefund, defaultRefundPath},
		{"退款查询", provider.OperationQueryRefund, defaultRefundQueryPath},
		{"关单", provider.OperationClose, defaultClosePath},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"errCode":"SUCCESS","refundStatus":"SUCCESS",` +
					`"merOrderId":"PAY20260917000000000001","refundOrderId":"REF20260917000000000001"}`))
			})

			_, err := New().Execute(context.Background(),
				operationRequestFor(channelConfig(server.URL), tc.operation))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			request := server.verifySignature(t, testSecret)
			if request.method != http.MethodPost {
				t.Fatalf("method = %q, want POST（规范 §5 那张表：三条都是 POST + JSON）", request.method)
			}
			if request.path != tc.wantPath {
				t.Fatalf("path = %q, want %q", request.path, tc.wantPath)
			}
			if request.rawQuery != "" {
				t.Fatalf("查询串上不该有认证参数（那是 H5 下单那套）：%q", request.rawQuery)
			}

			var sent map[string]any
			if err := json.Unmarshal(request.body, &sent); err != nil {
				t.Fatalf("解报文：%v（%s）", err, request.body)
			}
			// appId 在签名头里（上面 verifySignature 已经验过），**不在报文里**：官方样例的
			// 报文类里没有这个字段。
			if _, ok := sent["appId"]; ok {
				t.Fatalf("操作报文里不该有 appId：%s", request.body)
			}
			// 三条操作指的是**同一笔支付**，所以这个号必须与下单时逐字一致（见 orderid.go）：
			// 编码对不上时渠道会认为我们在操作一张它那边不存在的单。
			if sent["merOrderId"] != "3CYMPAY20260917000000000001" {
				t.Fatalf("merOrderId = %v，want 与下单同一个编码", sent["merOrderId"])
			}
			if sent["mid"] != testMid || sent["tid"] != testTid {
				t.Fatalf("mid/tid 该取配置：%s", request.body)
			}
			if _, ok := sent["requestTimestamp"].(string); !ok {
				t.Fatalf("requestTimestamp 该按渠道的墙上时间格式发：%s", request.body)
			}
		})
	}
}

// TestRefundBodyMatchesTheSample 钉死退款报文的字段集与**金额的形状**。
//
// 金额在这一族里有三种形状：下单是 JSON 数字、查单应答两种都认、退款请求是**字符串**
// （官方样例 `"refundAmount":"1"`）。发错形状的表现是渠道回一句关于参数的拒绝，看不出
// 是金额的写法问题。
func TestRefundBodyMatchesTheSample(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errCode":"SUCCESS","refundStatus":"SUCCESS",` +
			`"merOrderId":"PAY20260917000000000001","refundOrderId":"REF20260917000000000001"}`))
	})

	if _, err := New().Execute(context.Background(),
		operationRequestFor(channelConfig(server.URL), provider.OperationRefund)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	request := server.verifySignature(t, testSecret)
	var sent map[string]any
	if err := json.Unmarshal(request.body, &sent); err != nil {
		t.Fatalf("解报文：%v（%s）", err, request.body)
	}

	if got, ok := sent["refundAmount"].(string); !ok || got != "12800" {
		t.Fatalf("refundAmount = %#v，该是字符串 \"12800\"（官方样例发的是字符串）", sent["refundAmount"])
	}
	// 退款单号是渠道判幂等的键：报文里的 refundOrderId 与 msgId 同值（老系统也是这么填的）。
	//
	// 两者都是**带 sourceCode 前缀**的那个值——refundOrderId 是账单号，规范明说它要遵循商户
	// 订单号生成规范（原文与出处见 orderid.go 末尾那一节）。这里钉前缀而不是钉裸单号：把前缀
	// 去掉**不会**让这条测试变红以外的任何地方变红，而渠道那边会回一句「无效订单号」。
	if sent["refundOrderId"] != "3CYMREF20260917000000000001" {
		t.Fatalf("refundOrderId = %v，want 3CYM 前缀 + 退款单号", sent["refundOrderId"])
	}
	if sent["msgId"] != "3CYMREF20260917000000000001" {
		t.Fatalf("msgId = %v，该与退款单号（含前缀）同值", sent["msgId"])
	}
	if sent["refundDesc"] != "用户申请退款" {
		t.Fatalf("refundDesc = %v，该取 Reason", sent["refundDesc"])
	}
	// 分账三键不进**退款**报文：它们随下单走（divisionFlag / platformAmount / subOrders 是
	// 下单报文的字段，见 create.go 的 divisionFields），退款这边根本没有输入可操作——渠道那
	// 边钱已经按当初那条子单分完了，退多少是另一个问题。srcReserve 同样不属于这份报文。
	for _, absent := range []string{"divisionFlag", "platformAmount", "subOrders", "srcReserve"} {
		if _, ok := sent[absent]; ok {
			t.Fatalf("退款报文里不该有 %s：%s", absent, request.body)
		}
	}
}

// TestRefundUsesTheEffectiveInstMid：操作报文里的 instMid 必须与**当初建这一单时**用的那个
// 一致——H5 建的单是 H5DEFAULT，小程序建的单是 MINIDEFAULT。
//
// 取渠道 config 的默认值（MINIDEFAULT）会让所有 H5 单的退款打到别处去，而渠道的表现是
// 「查无此单」——那句错误会把人带去查单号，而不是查 instMid。
func TestRefundUsesTheEffectiveInstMid(t *testing.T) {
	cases := []struct {
		name   string
		action provider.Action
		want   string
	}{
		{"小程序那条路建的单", provider.ActionNativePay, defaultInstMid},
		{"H5 那条路建的单", provider.ActionH5, defaultH5InstMid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"errCode":"SUCCESS","refundStatus":"SUCCESS",` +
					`"merOrderId":"PAY20260917000000000001","refundOrderId":"REF20260917000000000001"}`))
			})

			request := operationRequestFor(channelConfig(server.URL), provider.OperationRefund)
			request.Method.Action = tc.action
			if _, err := New().Execute(context.Background(), request); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			var sent map[string]any
			if err := json.Unmarshal(server.last().body, &sent); err != nil {
				t.Fatalf("解报文：%v", err)
			}
			if sent["instMid"] != tc.want {
				t.Fatalf("instMid = %v, want %q", sent["instMid"], tc.want)
			}
		})
	}
}

// TestRefundClassification：退款那条路的判据是**两层**——errCode 管「这次调用收下了没有」，
// refundStatus 管「钱退成了没有」。
//
// 老系统只看第一层（refund_service.go 里 `errCode == "SUCCESS"` 就返回成功），而规范 §6 写着
// PROCESSING / UNKNOWN 要再查。**PROCESSING 被记成成功就是「钱还没退、账上已经退了」。**
func TestRefundClassification(t *testing.T) {
	echo := `"merOrderId":"PAY20260917000000000001","refundOrderId":"REF20260917000000000001"`
	cases := []struct {
		name     string
		status   int
		body     string
		want     provider.Result
		wantCode string
	}{
		{
			name: "退款成功", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"SUCCESS",` + echo + `}`,
			want: provider.ResultSuccess, wantCode: "refund_reported_success",
		},
		{
			// 大小写是渠道自己都不统一的（交易状态那三个写法就是证据），归一之后比。
			name: "小写状态词", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"success",` + echo + `}`,
			want: provider.ResultSuccess, wantCode: "refund_reported_success",
		},
		{
			// **本文件最要紧的一条**：渠道还在处理，钱没退到，出路是退款查询。
			name: "处理中", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"PROCESSING",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			name: "状态不明", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"UNKNOWN",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			// 应答里没有 refundStatus：errCode 说受理了，但没有任何一个字段说钱退到了。
			// 判成功就是把「受理」当成「退成」——那是老系统那段代码的处置。
			name: "没有状态字段", status: 200,
			body: `{"errCode":"SUCCESS",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			name: "不认识的词", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"WHATEVER",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			// 渠道明确说这笔退款没成：调用方可以换一条路再退。
			name: "退款失败", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"FAIL","errMsg":"余额不足",` + echo + `}`,
			want: provider.ResultFailed, wantCode: "refund_reported_failure",
		},
		{
			// 含糊码：渠道可能已经受理了这笔退款。标失败会让调用方拿**同一个退款单号**再退
			// 一次，而渠道对同一对 merOrderId + refundOrderId 是幂等的——第二次会拿回第一张
			// 退货单，两边都以为退了。
			name: "渠道还在处理上一笔", status: 200,
			body: `{"errCode":"ORDER_PROCESSING","errMsg":"处理中",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "AMBIGUOUS_PROVIDER_CODE",
		},
		{
			name: "应答里没有 errCode", status: 200,
			body: `{"refundStatus":"SUCCESS",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "AMBIGUOUS_PROVIDER_CODE",
		},
		{
			name: "渠道明确拒了", status: 200,
			body: `{"errCode":"NO_ORDER","errMsg":"查无此单",` + echo + `}`,
			want: provider.ResultFailed, wantCode: "provider_declined",
		},
		{
			// HTTP 层没过。FailureCode 是 HTTP_400 而不是 provider_declined：状态码先于报文，
			// 而这时「渠道的业务码说了什么」已经不重要了（与 interpretCreate 那两处同一种分法）。
			name: "HTTP 层拒了", status: 400,
			body: `{"errCode":"BAD_SIGN","errMsg":"签名错误"}`,
			want: provider.ResultFailed, wantCode: "HTTP_400",
		},
		{
			// 5xx 配一份写着 SUCCESS 的报文是自相矛盾的东西：状态码先于报文。
			name: "网关错误", status: 502,
			body: `{"errCode":"SUCCESS","refundStatus":"SUCCESS",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "HTTP_502",
		},
		{
			name: "报文读不懂", status: 200,
			body: `<html>`,
			want: provider.ResultUnknown, wantCode: "UNPARSEABLE_RESPONSE",
		},
		{
			// 4xx 是例外：渠道在协议层就拒了，它没有受理这笔退款。
			name: "报文读不懂且是 4xx", status: 404,
			body: `<html>`,
			want: provider.ResultFailed, wantCode: "UNPARSEABLE_RESPONSE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			result, err := New().Execute(context.Background(),
				operationRequestFor(channelConfig(server.URL), provider.OperationRefund))
			if err != nil {
				t.Fatalf("Execute: %v", err)
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

// TestRefundQueryClassification：退款查询**只问一件事**（这笔退款现在什么结局），所以协议层
// 没过时一律「还是不知道」——与查单那条路逐字相同。
//
// 它不能标失败：调用方对失败退款单的处置是「换一条路再退」，而查询失败说明不了钱没退。
func TestRefundQueryClassification(t *testing.T) {
	echo := `"merOrderId":"PAY20260917000000000001","refundOrderId":"REF20260917000000000001"`
	cases := []struct {
		name     string
		status   int
		body     string
		want     provider.Result
		wantCode string
	}{
		{
			name: "查到退成了", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"SUCCESS",` + echo + `}`,
			want: provider.ResultSuccess, wantCode: "query_reported_success",
		},
		{
			name: "查到退不成", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"FAIL",` + echo + `}`,
			want: provider.ResultFailed, wantCode: "query_reported_failure",
		},
		{
			name: "还在处理", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"PROCESSING",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			name: "状态不明", status: 200,
			body: `{"errCode":"SUCCESS","refundStatus":"UNKNOWN",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			// 与退款发起那条路的差别：这里是查询，渠道没答上来只说明我们不知道，
			// 不说明这笔退款失败了。
			name: "errCode 不是成功", status: 200,
			body: `{"errCode":"NO_ORDER","errMsg":"查无此单","refundStatus":"FAIL"}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			name: "协议层失败", status: 502,
			body: `{"errCode":"SUCCESS","refundStatus":"FAIL",` + echo + `}`,
			want: provider.ResultUnknown, wantCode: "refund_inconclusive",
		},
		{
			name: "报文读不懂", status: 200,
			body: `<html>`,
			want: provider.ResultUnknown, wantCode: "UNPARSEABLE_RESPONSE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			result, err := New().Execute(context.Background(),
				operationRequestFor(channelConfig(server.URL), provider.OperationQueryRefund))
			if err != nil {
				t.Fatalf("Execute: %v", err)
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

// TestCloseClassification：关单只问「这张预支付单还开着吗」，判据就一层。
//
// OPERATION_NOT_ALLOWED 落在失败那一档，这是**故意的**：规范把它注成「订单已关闭」，但同一
// 个码也可能表示「这一单付过了、不许关」，而后者记成「已关单」会让一笔真收了的钱被当成没付。
func TestCloseClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		want     provider.Result
		wantCode string
	}{
		{
			name: "关掉了", status: 200,
			body: `{"errCode":"SUCCESS","errMsg":""}`,
			want: provider.ResultSuccess, wantCode: "close_accepted",
		},
		{
			name: "单不存在", status: 200,
			body: `{"errCode":"NO_ORDER","errMsg":"查无此单"}`,
			want: provider.ResultFailed, wantCode: "provider_declined",
		},
		{
			name: "不许操作", status: 200,
			body: `{"errCode":"OPERATION_NOT_ALLOWED","errMsg":"订单已关闭"}`,
			want: provider.ResultFailed, wantCode: "provider_declined",
		},
		{
			name: "含糊码", status: 200,
			body: `{"errCode":"ORDER_PROCESSING"}`,
			want: provider.ResultUnknown, wantCode: "AMBIGUOUS_PROVIDER_CODE",
		},
		{
			name: "网关错误", status: 502,
			body: `{"errCode":"SUCCESS"}`,
			want: provider.ResultUnknown, wantCode: "HTTP_502",
		},
		{
			name: "HTTP 层拒了", status: 401,
			body: `{"errCode":"BAD_SIGN","errMsg":"签名错误"}`,
			want: provider.ResultFailed, wantCode: "HTTP_401",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			result, err := New().Execute(context.Background(),
				operationRequestFor(channelConfig(server.URL), provider.OperationClose))
			if err != nil {
				t.Fatalf("Execute: %v", err)
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

// TestOperatorRejectsAnswersAboutAnotherRefund：回显的单号对不上时，这份应答说的是**另一笔**。
//
// 第二种（退款单号对不上）是本文件独有的判据：一笔支付退过两次时，退款查询若只按 merOrderId
// 认，拿到的是哪一次退款的状态就不一定了——把另一次的状态记到这一笔上，轻则重复退款、
// 重则少退一笔钱。
func TestOperatorRejectsAnswersAboutAnotherRefund(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "另一笔支付",
			body: `{"errCode":"SUCCESS","refundStatus":"SUCCESS","merOrderId":"PAY-SOMEBODY-ELSE",` +
				`"refundOrderId":"REF20260917000000000001"}`,
		},
		{
			name: "同一笔支付上的另一次退款",
			body: `{"errCode":"SUCCESS","refundStatus":"SUCCESS","merOrderId":"PAY20260917000000000001",` +
				`"refundOrderId":"REF-THE-SECOND-ONE"}`,
		},
	}

	for _, operation := range []provider.Operation{provider.OperationRefund, provider.OperationQueryRefund} {
		for _, tc := range cases {
			t.Run(string(operation)+"/"+tc.name, func(t *testing.T) {
				server := newChannelServer(t)
				server.setHandler(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(tc.body))
				})

				result, err := New().Execute(context.Background(),
					operationRequestFor(channelConfig(server.URL), operation))
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if result.Result != provider.ResultUnknown || result.FailureCode != "refund_order_mismatch" {
					t.Fatalf("Result=%q FailureCode=%q，别人的应答不该被采信", result.Result, result.FailureCode)
				}
			})
		}
	}
}

// TestOperatorNeverConcludesFromATransportFailure：出网失败**一律**「不确定」。
//
// 这三种操作都是「发出去了就可能有后果」的调用：退款可能已经到了渠道那边、关单可能已经把
// 预支付单关了。标失败会让调用方去做一件已经做过的事（再退一次、或以为单还开着）。
func TestOperatorNeverConcludesFromATransportFailure(t *testing.T) {
	t.Run("超时", func(t *testing.T) {
		release := make(chan struct{})
		server := newChannelServer(t)
		server.setHandler(func(w http.ResponseWriter, r *http.Request) { <-release })
		t.Cleanup(func() { close(release) })

		adapter := &Provider{client: httpx.New(50 * time.Millisecond)}
		result, err := adapter.Execute(context.Background(),
			operationRequestFor(channelConfig(server.URL), provider.OperationRefund))
		if err == nil {
			t.Fatal("没拿到结论时该返回 error，调用方据此把这次调用记进流水")
		}
		if result.Result != provider.ResultUnknown || result.FailureCode != "OPERATION_NOT_ANSWERED" {
			t.Fatalf("Result=%q FailureCode=%q", result.Result, result.FailureCode)
		}
		if result.ResponseSummary["notSent"] != false {
			t.Fatalf("notSent = %v，这一次是**发出去了**没等到应答", result.ResponseSummary["notSent"])
		}
	})

	t.Run("根本没发出去", func(t *testing.T) {
		server := newChannelServer(t)
		url := server.URL
		server.Close()

		result, err := New().Execute(context.Background(),
			operationRequestFor(channelConfig(url), provider.OperationRefund))
		if !errors.Is(err, httpx.ErrNotSent) {
			t.Fatalf("want ErrNotSent, got %v", err)
		}
		if result.Result != provider.ResultUnknown {
			t.Fatalf("Result = %q, want unknown", result.Result)
		}
		if result.ResponseSummary["notSent"] != true {
			t.Fatalf("notSent = %v，这一次是**没发出去**", result.ResponseSummary["notSent"])
		}
	})
}

// TestOperatorRefusesToGuessWithoutThePinnedInputs：配置解不开、密钥是空的、单号缺了、
// 金额不是正数——四种都在**本地拒**，一个字节都不发。
//
// 理由与 Create 里那一段逐字相同：渠道回过来的会是一句与本意无关的错（「参数不对」「查无此单」），
// 而那条错误会盖住真正的原因。
func TestOperatorRefusesToGuessWithoutThePinnedInputs(t *testing.T) {
	cases := []struct {
		name    string
		damage  func(*provider.OperationRequest)
		wantErr error
	}{
		{
			name: "配置解不开",
			damage: func(r *provider.OperationRequest) {
				r.Method = channelMethod(map[string]any{"baseURL": "https://api-mop.chinaums.com"})
			},
			wantErr: provider.ErrConfigInvalid,
		},
		{
			name:    "密钥是空的",
			damage:  func(r *provider.OperationRequest) { r.Secrets = credentialsFor("  ") },
			wantErr: provider.ErrSecretNotConfigured,
		},
		{
			name:    "没有支付单号",
			damage:  func(r *provider.OperationRequest) { r.PaymentNo = " " },
			wantErr: provider.ErrConfigInvalid,
		},
		{
			// 退款单号是渠道判幂等的那个键（见 refund 里那一段）：空着发过去，第二笔退款会
			// 撞上第一笔的幂等键，然后把上一次的结果当成这一次的结局回给我们。
			name:    "没有退款单号",
			damage:  func(r *provider.OperationRequest) { r.RefundNo = "" },
			wantErr: provider.ErrConfigInvalid,
		},
		{
			name:    "退款金额不是正数",
			damage:  func(r *provider.OperationRequest) { r.Amount = 0 },
			wantErr: provider.ErrConfigInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			request := operationRequestFor(channelConfig(server.URL), provider.OperationRefund)
			tc.damage(&request)

			result, err := New().Execute(context.Background(), request)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if result.Result != provider.ResultUnknown {
				t.Fatalf("Result = %q, want unknown（没发出去不等于操作失败）", result.Result)
			}
			if sent := server.requestCount(); sent != 0 {
				t.Fatalf("本地拒时**一个请求都不该发出去**，实际发了 %d 个", sent)
			}
		})
	}
}

// TestOperatorRejectsOperationsThisFamilyDoesNotDo：这一族确实有 Operator，但那不等于
// 它对**任何**操作都答得上。
//
// 报的是 ErrOperationNotSupported 而不是 ResultFailed：前者是配置问题（一个这一族没有的操作
// 被挂上来了），后者是「渠道拒了这笔操作」，调用方对两者的处置不同。
func TestOperatorRejectsOperationsThisFamilyDoesNotDo(t *testing.T) {
	server := newChannelServer(t)
	request := operationRequestFor(channelConfig(server.URL), provider.Operation("reconcile"))

	result, err := New().Execute(context.Background(), request)
	if !errors.Is(err, provider.ErrOperationNotSupported) {
		t.Fatalf("want ErrOperationNotSupported, got %v", err)
	}
	if result.Result != provider.ResultUnknown {
		t.Fatalf("Result = %q, want unknown", result.Result)
	}
	if sent := server.requestCount(); sent != 0 {
		t.Fatalf("不支持的操作**一个请求都不该发出去**，实际发了 %d 个", sent)
	}
}

// TestRefundQueryCarriesTheRefundNumber：退款查询报文里带 refundOrderId 是**本仓库对样例的
// 一处增补**（见 refundQueryBody 上那一大段），而它必须真的发出去——那一段最长，也最容易
// 在后续「对齐样例」的改动里被删掉。
func TestRefundQueryCarriesTheRefundNumber(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errCode":"SUCCESS","refundStatus":"SUCCESS",` +
			`"merOrderId":"PAY20260917000000000001","refundOrderId":"REF20260917000000000001"}`))
	})

	if _, err := New().Execute(context.Background(),
		operationRequestFor(channelConfig(server.URL), provider.OperationQueryRefund)); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(server.last().body, &sent); err != nil {
		t.Fatalf("解报文：%v", err)
	}
	// 带前缀，且必须与退款请求（refund()）发出去的那个值逐字相同：渠道拿它找退款单，两边编码
	// 不一致时它会把「有这单」答成「查无此单」。
	if sent["refundOrderId"] != "3CYMREF20260917000000000001" {
		t.Fatalf("refundOrderId = %v，退款查询必须能指出问的是哪一次退款", sent["refundOrderId"])
	}
	// msgId 是「这一次查询」的请求号，与退款单号无关（老系统在那条路上根本没填 msgId）。
	msgID, _ := sent["msgId"].(string)
	if !strings.HasPrefix(msgID, "REFUNDQUERY") {
		t.Fatalf("msgId = %q，该是 REFUNDQUERY 前缀的请求号", msgID)
	}
}

// TestOperationPathsComeFromConfig：三条操作路径都可配，而默认值取自官方样例。
//
// 退款查询那条尤其要钉住：规范与样例是 /v1/netpay/refund-query，老系统打的是另一条
// （/v1/netpay/trade/refund/query），两条在渠道那边未必都开着。
func TestOperationPathsComeFromConfig(t *testing.T) {
	byDefault, err := Parse(channelConfig("https://api-mop.chinaums.com"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if byDefault.refundPath != defaultRefundPath ||
		byDefault.refundQueryPath != defaultRefundQueryPath || byDefault.closePath != defaultClosePath {
		t.Fatalf("默认路径：refund=%q refundQuery=%q close=%q",
			byDefault.refundPath, byDefault.refundQueryPath, byDefault.closePath)
	}

	config := channelConfig("https://api-mop.chinaums.com")
	config["endpoints"] = map[string]any{
		"refund":      "/v1/netpay/trade/refund",
		"refundQuery": "/v1/netpay/trade/refund/query",
		"close":       "/v1/netpay/trade/close",
	}
	got, err := Parse(config)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.refundPath != "/v1/netpay/trade/refund" ||
		got.refundQueryPath != "/v1/netpay/trade/refund/query" ||
		got.closePath != "/v1/netpay/trade/close" {
		t.Fatalf("配置里的路径没生效：refund=%q refundQuery=%q close=%q",
			got.refundPath, got.refundQueryPath, got.closePath)
	}
}

// TestRefundAmountGoesIntoTheSummary：渠道回显的退款金额进摘要（**只记录、不采信**）——
// 排查「渠道说的退款金额和我们记的差多少」时它是第一个要看的。
func TestRefundAmountGoesIntoTheSummary(t *testing.T) {
	for _, raw := range []string{`12800`, `"12800"`} {
		server := newChannelServer(t)
		server.setHandler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"errCode":"SUCCESS","refundStatus":"PROCESSING",` +
				`"refundAmount":` + raw + `}`))
		})

		result, err := New().Execute(context.Background(),
			operationRequestFor(channelConfig(server.URL), provider.OperationRefund))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.ResponseSummary["refundAmount"] != int64(12800) {
			t.Fatalf("refundAmount = %v（原始 %s），该按分记进摘要", result.ResponseSummary["refundAmount"], raw)
		}
		if result.ResponseSummary["providerRefundStatus"] != "PROCESSING" {
			t.Fatalf("状态词该进摘要：%v", result.ResponseSummary)
		}
	}
}
