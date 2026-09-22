package ums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

// H5 那条路（action=h5）的测试。
//
// 它与 create_test.go 里那组是**两种协议**，所以判据也不同：那组验的是 Authorization 头，
// 这组验的是查询串——H5 的认证参数全在 URL 里，而且其中一项（content）要先编码再拼。
//
// 与那组一样，签名**不调本包的 Sign 去验**：那样只会证明「我们自己签自己验」永远成立。
// 这里用的是 config_test.go 里的 legacySignature——规范第 4 节那段算法的独立复刻（它与
// OPEN-BODY-SIG 是同一套算法，只是参数换了地方放）。

// h5Method 造一条 action=h5 的支付方式。params 是这条支付方式自己的参数（H5 的下单路径**只能**
// 从这里来，见 config.options）。
func h5Method(config map[string]any, params map[string]string) provider.Method {
	method := channelMethod(config)
	method.Action = provider.ActionH5
	method.Params = params
	return method
}

// h5CreateRequestFor 造一次 H5 发起支付。默认按支付宝 H5 那条接口配。
func h5CreateRequestFor(config map[string]any, params map[string]string) provider.CreateRequest {
	request := createRequestFor(config)
	request.Method = h5Method(config, params)
	// H5 的收银台在浏览器里，回跳地址是这一次请求的一部分（小程序那条路上它没有意义）。
	request.ReturnURL = "https://shop.example.com/paid"
	// WalletOpenID 留着不动：H5 那条路根本不读它（微信 H5 认的是浏览器，不是小程序里的付款人），
	// 留着正好钉住「有没有被误当成小程序那一路」。
	return request
}

// h5Query 把假渠道收到的查询串解出来，并用**独立实现**验一遍签名。
//
// 返回的是解好码的 content（也就是待签的那份原文）。校验分三层，缺一层就漏一种错：
//
//	authorization=OPEN-FORM-PARAM   认证方式在不在
//	签名 == legacySignature(解好码的 content)   签名算的是**原文**，不是编码之后的串
//	原始串里看得见 %7B%22merOrderId%22           content 确实被编码后才拼进 URL
func h5Query(t *testing.T, server *channelServer, secret string) (url.Values, h5CreateBody) {
	t.Helper()
	request := server.last()
	if request.rawQuery == "" {
		t.Fatal("H5 下单必须把认证参数放进查询串，实际一个都没有")
	}
	values, err := url.ParseQuery(request.rawQuery)
	if err != nil {
		t.Fatalf("查询串解不开：%v（%s）", err, request.rawQuery)
	}

	if got := values.Get("authorization"); got != formParamScheme {
		t.Fatalf("authorization = %q, want %q", got, formParamScheme)
	}
	content := values.Get("content")
	if content == "" {
		t.Fatalf("content 是空的：%s", request.rawQuery)
	}
	// content 必须是**编码之后**才进 URL 的：未编码的 JSON 在查询串里会把 & 与 = 变成分隔符，
	// 整条串当场散架。
	// msgId 是报文里的第一个键（结构体声明顺序），所以编码后的串一定以 content=%7B%22msgId%22
	// 开头——它同时也是「content 确实被编码了」最直接的一处证据。
	if !strings.Contains(request.rawQuery, "content=%7B%22msgId%22") {
		t.Fatalf("content 没有被 URL 编码就拼进了查询串：%s", request.rawQuery)
	}
	if want := legacySignature(testAppID, secret, values.Get("timestamp"), values.Get("nonce"),
		[]byte(content)); values.Get("signature") != want {
		t.Fatalf("签名对不上：\n 收到 %s\n 期望 %s\n（签名算的是**未编码**的 content）",
			values.Get("signature"), want)
	}

	var body h5CreateBody
	if err := json.Unmarshal([]byte(content), &body); err != nil {
		t.Fatalf("content 不是一份能读的业务内容：%v（%s）", err, content)
	}
	return values, body
}

// TestCreateH5CarriesDivision：H5 这条路的业务内容里也要带分账三键。
//
// 分账是渠道能力，不因为下单走的是 H5 还是小程序而变；而 dev 上**只有支付宝 H5 走得通**
// （其余支付方式回 412 / 5131016），真要拿一笔单去验分账，走的就是这条路——所以这条用例
// 不是「另一半顺手补上」，它是能验的那一半。
func TestCreateH5CarriesDivision(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cashier.example.com/pay?token=abc", http.StatusFound)
	})

	request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
		paramCreatePath: "/v1/netpay/trade/h5-pay",
	})
	request.Division = &provider.DivisionInstruction{
		PlatformAmount: 800,
		// 两条子单：一条会被「只发第一条」这种写法漏掉，而银联那边少一条就是少分一份钱。
		SubOrders: []provider.DivisionSubOrder{
			{Mid: "SUB-MID-1", Amount: 12000},
			{Mid: "SUB-MID-2", Amount: 300},
		},
	}

	if _, err := New().Create(context.Background(), request); err != nil {
		t.Fatalf("Create: %v", err)
	}

	values, body := h5Query(t, server, testSecret)
	if body.DivisionFlag == nil || !*body.DivisionFlag {
		t.Fatalf("divisionFlag 该为 true：%s", values.Get("content"))
	}
	if body.PlatformAmount == nil || *body.PlatformAmount != 800 {
		t.Fatalf("platformAmount = %v，want 800", body.PlatformAmount)
	}
	if len(body.SubOrders) != 2 {
		t.Fatalf("subOrders = %+v，want 两条", body.SubOrders)
	}
	if sub := body.SubOrders[0]; sub.Mid != "SUB-MID-1" || sub.Amount != 12000 {
		t.Fatalf("子单 = %+v，want mid=SUB-MID-1 totalAmount=12000", sub)
	}
	if !strings.Contains(values.Get("content"), `"totalAmount":12000`) {
		t.Fatalf("子单金额的键名该是 totalAmount：%s", values.Get("content"))
	}
	if strings.Contains(values.Get("content"), `"amount":`) {
		t.Fatalf("content 里出现了 amount 键（那是老系统的内部口径，渠道不认）：%s",
			values.Get("content"))
	}
}

// TestCreateH5OmitsDivisionKeysWithoutASubOrder：没有分账时 H5 的 content 里也不该出现它们。
//
// 这一条比小程序那条更要紧：content 是**逐字节签名**的一串，多一个空键就多一个变量。
func TestCreateH5OmitsDivisionKeysWithoutASubOrder(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cashier.example.com/pay?token=abc", http.StatusFound)
	})

	request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
		paramCreatePath: "/v1/netpay/trade/h5-pay",
	})
	request.Division = &provider.DivisionInstruction{PlatformAmount: 12800}

	if _, err := New().Create(context.Background(), request); err != nil {
		t.Fatalf("Create: %v", err)
	}

	values, body := h5Query(t, server, testSecret)
	if body.DivisionFlag != nil || body.PlatformAmount != nil || body.SubOrders != nil {
		t.Fatalf("没有分账时三个键都不该解出值：%+v", body)
	}
	for _, absent := range []string{"divisionFlag", "platformAmount", "subOrders"} {
		if strings.Contains(values.Get("content"), `"`+absent+`"`) {
			t.Fatalf("content 里不该有 %s：%s", absent, values.Get("content"))
		}
	}
}

// TestCreateH5SendsASignedGetURL 是这条路的形状：GET、参数全在查询串、一个字节的体都不发。
func TestCreateH5SendsASignedGetURL(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cashier.example.com/pay?token=abc", http.StatusFound)
	})

	request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
		paramCreatePath: "/v1/netpay/trade/h5-pay",
	})
	request.ExpiresAt = time.Date(2026, 9, 20, 15, 4, 5, 0, time.UTC)

	result, err := New().Create(context.Background(), request)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Result != provider.ResultSuccess {
		t.Fatalf("Result = %q（%s %s）", result.Result, result.FailureCode, result.FailureMessage)
	}

	captured := server.last()
	if captured.method != http.MethodGet {
		t.Fatalf("method = %q，H5 下单是 GET", captured.method)
	}
	if captured.path != "/v1/netpay/trade/h5-pay" {
		t.Fatalf("path = %q，params 里的 createPath 该生效", captured.path)
	}
	if len(captured.body) != 0 {
		t.Fatalf("GET 不该带请求体，实际发了 %d 字节：%s", len(captured.body), captured.body)
	}
	// 认证参数全在查询串里，头里一个都不该有——这一族的 GET 不带 Authorization。
	if got := captured.headers.Get(headerAuthorization); got != "" {
		t.Fatalf("H5 那条路不该发 Authorization 头，实际 %q", got)
	}

	values, body := h5Query(t, server, testSecret)

	// —— 五个认证参数一个都不能少（少一个渠道判签名错误，而错误串不会说少的是哪个）——
	for _, name := range []string{"appId", "timestamp", "nonce", "content", "signature"} {
		if values.Get(name) == "" {
			t.Fatalf("认证参数 %s 是空的", name)
		}
	}
	if values.Get("appId") != testAppID {
		t.Fatalf("appId = %q", values.Get("appId"))
	}
	if len(values.Get("timestamp")) != len(signTimestampLayout) {
		t.Fatalf("timestamp = %q，该是 %s 格式", values.Get("timestamp"), signTimestampLayout)
	}

	// —— 业务内容 ——
	// 与小程序那条路**逐字同一种编码**（见 orderid.go）：两条路发出去的单号必须是同一个写法，
	// 否则同一笔支付在小程序与 H5 上是两个渠道单号，回调认不回同一张支付单。
	if body.MerOrderID != "3CYMPAY20260917000000000001" {
		t.Fatalf("merOrderId = %q，want 3CYM 前缀 + 支付单号", body.MerOrderID)
	}
	if body.MsgID != "req-1" {
		t.Fatalf("msgId = %q, want req-1", body.MsgID)
	}
	if body.InstMid != defaultH5InstMid {
		t.Fatalf("instMid = %q, want %q（H5 与小程序是两个不同的取值）", body.InstMid, defaultH5InstMid)
	}
	if body.Mid != testMid || body.Tid != testTid {
		t.Fatalf("mid/tid 该取配置：%+v", body)
	}
	if body.TotalAmount != 12800 {
		t.Fatalf("totalAmount = %d，该是**分**且是 JSON 数字", body.TotalAmount)
	}
	if body.NotifyURL != "https://api.example.com/v1/payments/callback/ums_wechat" {
		t.Fatalf("notifyUrl = %q", body.NotifyURL)
	}
	// returnUrl 在这一条路上优先取调用方给的（装配处按环境拼），而不是渠道 config 里那一项。
	if body.ReturnURL != "https://shop.example.com/paid" {
		t.Fatalf("returnUrl = %q，该取 CreateRequest.ReturnURL", body.ReturnURL)
	}
	if body.ExpireTime != "2026-09-20 15:04:05" {
		t.Fatalf("expireTime = %q，格式该是 yyyy-MM-dd HH:mm:ss", body.ExpireTime)
	}

	// —— 客户端拿到的就是那行 Location ——
	if result.PayParams[paramH5URL] != "https://cashier.example.com/pay?token=abc" {
		t.Fatalf("PayParams = %v，键名 h5Url 与 wechat_v3 逐字相同", result.PayParams)
	}
	// 摘要里只记主机名：整条收银台地址带着渠道侧的会话标识，而这一栏要落库。
	if host := result.ResponseSummary["h5Host"]; host != "cashier.example.com" {
		t.Fatalf("h5Host = %v", host)
	}
	if summary := fmt.Sprintf("%v", result.ResponseSummary); strings.Contains(summary, "token=abc") {
		t.Fatalf("摘要里带了整条收银台地址：%v", result.ResponseSummary)
	}
}

// TestCreateH5HasNoTradeTypeAndOmitsEmptyOptionalFields 钉死 H5 业务内容与小程序报文的两处差别。
//
//   - **没有 tradeType**：渠道文档的业务内容表里查无此键。带上它等于发一个渠道不认识的参数，
//     而那一项在小程序那条路上是必填的——两边的报文形状不一样，混起来是最容易犯的错。
//   - **可选项为空时省略**：与小程序那条「每个键都无条件出现」相反，见 h5CreateBody 的说明。
func TestCreateH5HasNoTradeTypeAndOmitsEmptyOptionalFields(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cashier.example.com/pay", http.StatusFound)
	})

	// 渠道 config 里配了 tradeType 与微信 H5 那三项，但参数只给路径——微信那三项是**支付方式级**
	// 的，配在渠道上会被 params 覆盖，这里刻意不给 params，看它们会不会漏进支付宝 H5 的报文。
	request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
		paramCreatePath: "/v1/netpay/trade/h5-pay",
	})
	request.ExpiresAt = time.Time{} // 不给过期时间

	if _, err := New().Create(context.Background(), request); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, body := h5Query(t, server, testSecret)

	raw := jsonOf(t, server)
	if strings.Contains(raw, `"tradeType"`) {
		t.Fatalf("H5 的业务内容里不该有 tradeType：%s", raw)
	}
	if strings.Contains(raw, `"expireTime"`) {
		t.Fatalf("没给过期时间时该省略这一项（而不是发一个空日期）：%s", raw)
	}
	if strings.Contains(raw, `"returnUrl":""`) {
		t.Fatalf("可选项为空时该省略，而不是发空串：%s", raw)
	}
	if body.ExpireTime != "" {
		t.Fatalf("expireTime = %q, want 空", body.ExpireTime)
	}
	if body.SceneType != "" || body.MerAppName != "" || body.MerAppID != "" {
		t.Fatalf("微信 H5 那三项没配时该是空的：%+v", body)
	}
	// 报文里的键名逐个点名：这一族**没有**配置驱动的字段映射，多一个键、少一个键都是协议
	// 层面的错，而它们不会自己报错。
	for _, key := range []string{"msgId", "requestTimestamp", "merOrderId", "mid", "tid", "instMid",
		"totalAmount", "notifyUrl", "returnUrl"} {
		if !strings.Contains(raw, `"`+key+`"`) {
			t.Fatalf("报文里少了 %s：%s", key, raw)
		}
	}
	// 没配的可选项一个都不出现（老系统那套「每个键都无条件出现」在这里不成立，见 h5CreateBody）。
	for _, key := range []string{"srcReserve", "sceneType", "merAppName", "merAppId"} {
		if strings.Contains(raw, `"`+key+`"`) {
			t.Fatalf("没配的 %s 不该出现在报文里：%s", key, raw)
		}
	}

	// 反过来：params 给了就该出现，且**只有**给了的那一项出现。
	server2 := newChannelServer(t)
	server2.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cashier.example.com/pay", http.StatusFound)
	})
	wxRequest := h5CreateRequestFor(channelConfig(server2.URL), map[string]string{
		paramCreatePath: "/v1/netpay/wxpay/h5-pay",
		paramSceneType:  "AND_WAP",
		paramMerAppName: "银联商务官网",
		paramMerAppID:   "https://m.example.com",
	})
	if _, err := New().Create(context.Background(), wxRequest); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, wxBody := h5Query(t, server2, testSecret)
	if wxBody.SceneType != "AND_WAP" || wxBody.MerAppName != "银联商务官网" ||
		wxBody.MerAppID != "https://m.example.com" {
		t.Fatalf("微信 H5 那三项该由 params 给出：%+v", wxBody)
	}
}

// jsonOf 取假渠道收到的 content 原文（已经是解码之后的那份 JSON）。
func jsonOf(t *testing.T, server *channelServer) string {
	t.Helper()
	values, err := url.ParseQuery(server.last().rawQuery)
	if err != nil {
		t.Fatalf("查询串解不开：%v", err)
	}
	return values.Get("content")
}

// TestCreateH5DoesNotFollowRedirect 钉死这条路上唯一一条「跟过去就出事」的规矩。
//
// 302 是渠道在说「把浏览器送到这儿去」。跟着走的话，读回来的是收银台的 HTML，HTTP 状态还是
// 200，与「渠道建了单」看不出区别——而那个 HTML 会占满一次调用的时间与内存。断言
// **假渠道只收到一次请求**：跟过去了就必然是两次。
func TestCreateH5DoesNotFollowRedirect(t *testing.T) {
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cashier.example.com/pay?token=abc", http.StatusFound)
	})

	request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
		paramCreatePath: "/v1/netpay/trade/h5-pay",
	})
	result, err := New().Create(context.Background(), request)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Result != provider.ResultSuccess {
		t.Fatalf("Result = %q（%s）", result.Result, result.FailureCode)
	}
	if got := result.PayParams[paramH5URL]; got != "https://cashier.example.com/pay?token=abc" {
		t.Fatalf("h5Url = %q", got)
	}
	if count := server.requestCount(); count != 1 {
		t.Fatalf("假渠道收到了 %d 个请求，**302 之后那一跳不该跟**", count)
	}
	if result.HTTPStatus != http.StatusFound {
		t.Fatalf("HTTPStatus = %d，该是渠道原样回的那一个（302）", result.HTTPStatus)
	}
}

// TestCreateH5Classification：H5 的成功判据是一次 3xx，不是一份报文。这里把每一档都钉住。
func TestCreateH5Classification(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		location   string
		body       string
		want       provider.Result
		wantCode   string
		wantParams bool
	}{
		{
			name: "302 带 Location", status: http.StatusFound,
			location: "https://cashier.example.com/pay",
			want:     provider.ResultSuccess, wantParams: true,
		},
		{
			// 301/303/307 同样是跳转，一样是成功：渠道换一个状态码不该让一笔已建的单变成
			// 「不明」。
			name: "303 带 Location", status: http.StatusSeeOther,
			location: "https://cashier.example.com/pay",
			want:     provider.ResultSuccess, wantParams: true,
		},
		{
			// 3xx 却没有 Location：我们拿不到收银台地址。**不能当失败**——渠道可能已经建单了。
			name: "302 没有 Location", status: http.StatusFound,
			want: provider.ResultUnknown, wantCode: "H5_UNEXPECTED_RESPONSE",
		},
		{
			// 「302 还是 200 + HTML」是文档 §10.3 的待确认项，**已用真实测试凭据定稿：302**
			// （四条下单路径实测都回 302 + Location 指向收银台）。所以上面那一条是主干，
			// 而这一条留着当防线：渠道哪天改成回一页 HTML，我们要落进「结果不明」由查单探针
			// 去问，**绝不能当失败**——那会让用户去付第二遍。
			name: "200 加一页 HTML", status: http.StatusOK,
			body: `<html><body><a href="https://cashier.example.com/pay">去支付</a></body></html>`,
			want: provider.ResultUnknown, wantCode: "H5_UNEXPECTED_RESPONSE",
		},
		{
			// 4xx 是协议层就拒了：路径不对、签名不对、商户号不认识。它没有建单，用户可以立刻
			// 换一种方式付。报文里那两句渠道的话要进流水（这是 H5 唯一的诊断线索）。
			name: "400 带渠道错误码", status: http.StatusBadRequest,
			body:     `{"errCode":"SIGN_ERROR","errMsg":"签名错误"}`,
			want:     provider.ResultFailed,
			wantCode: "HTTP_400",
		},
		{
			// 5xx：对面自己出了故障，它到底建了单没有我们不知道。
			name: "502", status: http.StatusBadGateway,
			body: `{"errCode":"SUCCESS"}`,
			want: provider.ResultUnknown, wantCode: "HTTP_502",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
				paramCreatePath: "/v1/netpay/trade/h5-pay",
			})
			result, err := New().Create(context.Background(), request)
			if err != nil {
				t.Fatalf("请求发出去之后的失败**不该**返回 error：%v", err)
			}
			if result.Result != tc.want {
				t.Fatalf("Result = %q, want %q（%s）", result.Result, tc.want, result.FailureMessage)
			}
			if result.FailureCode != tc.wantCode {
				t.Fatalf("FailureCode = %q, want %q", result.FailureCode, tc.wantCode)
			}
			if gotParams := len(result.PayParams) > 0; gotParams != tc.wantParams {
				t.Fatalf("PayParams = %v", result.PayParams)
			}
			if result.HTTPStatus != tc.status || result.Attempts != 1 {
				t.Fatalf("HTTPStatus=%d Attempts=%d，两个都要落进 payment_provider_calls",
					result.HTTPStatus, result.Attempts)
			}
			if summary := fmt.Sprintf("%v", result.ResponseSummary); strings.Contains(summary, testSecret) {
				t.Fatalf("应答摘要里出现了密钥：%v", result.ResponseSummary)
			}
		})
	}
}

// TestCreateH5FailureKeepsTheProviderWords：4xx 那种失败唯一的诊断线索是报文里那两句话，
// 而它必须**有上限**地进流水——H5 的渠道在失败时可能回一整页 HTML，而 response_summary 是要
// 落库的一栏。
func TestCreateH5FailureKeepsTheProviderWords(t *testing.T) {
	long := strings.Repeat("很", failureMessageLimit*2)
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"errCode":"PARAM_ERROR","errMsg":%q}`, long)
	})

	request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
		paramCreatePath: "/v1/netpay/trade/h5-pay",
	})
	result, err := New().Create(context.Background(), request)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Result != provider.ResultFailed {
		t.Fatalf("Result = %q", result.Result)
	}
	if result.ResponseSummary["providerCode"] != "PARAM_ERROR" {
		t.Fatalf("渠道的错误码该进摘要：%v", result.ResponseSummary)
	}
	message, _ := result.ResponseSummary["providerMessage"].(string)
	if len([]rune(message)) != failureMessageLimit {
		t.Fatalf("说明文字该被截到 %d 个字符，实际 %d", failureMessageLimit, len([]rune(message)))
	}
}

// TestCreateH5ReadsErrInfoToo：H5 那条协议的说明字段叫 errInfo，不叫 errMsg。
//
// 这不是猜的：拿真实测试凭据打沙箱时，一条格式不对的请求回的是
// `{"errCode":"1000","errInfo":"Timestamp解析失败"}`。只认 errMsg 的实现不会报错、也不会崩，
// 它只是把渠道写的那句话丢掉——运维在流水里看到「provider returned http 412」加一个
// 数字错误码，然后得去翻渠道文档猜那个数字是什么意思。
//
// 两个字段**都**要认，且 errMsg 优先（同一份应答不会两个都写，而先读 errMsg 不改变小程序
// 那条路今天的行为）。
func TestCreateH5ReadsErrInfoToo(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "只有 errInfo（H5 实测的形状）",
			body: `{"errCode":"1000","errInfo":"Timestamp解析失败"}`,
		},
		{
			name: "两个都有时取 errMsg",
			body: `{"errCode":"1000","errInfo":"渠道内部的话","errMsg":"给商户看的话"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			server.setHandler(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(tc.body))
			})

			request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
				paramCreatePath: "/v1/netpay/trade/h5-pay",
			})
			result, err := New().Create(context.Background(), request)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if result.Result != provider.ResultFailed || result.FailureCode != "HTTP_412" {
				t.Fatalf("412 该判失败：Result=%q FailureCode=%q", result.Result, result.FailureCode)
			}
			want := "Timestamp解析失败"
			if tc.name == "两个都有时取 errMsg" {
				want = "给商户看的话"
			}
			if result.FailureMessage != want {
				t.Fatalf("FailureMessage = %q, want %q（渠道写的话不该被丢掉）", result.FailureMessage, want)
			}
			if got := result.ResponseSummary["providerMessage"]; got != want {
				t.Fatalf("providerMessage = %v, want %q", got, want)
			}
			// 错误码照旧：两个字段名说的是同一件事，判据那一栏不该因为这个改动而变。
			if got := result.ResponseSummary["providerCode"]; got != "1000" {
				t.Fatalf("providerCode = %v", got)
			}
		})
	}
}

// TestCreateH5NeverGoesOutWithoutThePinnedInputs：四种本地拒，**一个字节都不许发**。
func TestCreateH5NeverGoesOutWithoutThePinnedInputs(t *testing.T) {
	cases := []struct {
		name    string
		damage  func(*provider.CreateRequest)
		wantErr error
	}{
		{
			// H5 的四条下单路径没有默认值：缺了就拒，而不是拿渠道 config 里那条小程序的路径
			// 发过去（那会让渠道回一句与本意无关的错）。
			name:    "params 里没有 createPath",
			damage:  func(r *provider.CreateRequest) { r.Method.Params = nil },
			wantErr: provider.ErrConfigInvalid,
		},
		{
			name:    "createPath 是空白",
			damage:  func(r *provider.CreateRequest) { r.Method.Params = map[string]string{paramCreatePath: "  "} },
			wantErr: provider.ErrConfigInvalid,
		},
		{
			// 空回调地址在这条路上最致命：用户被浏览器送走之后，我们**没有别的成交通知**。
			name:    "没有回调地址",
			damage:  func(r *provider.CreateRequest) { r.NotifyURL = "" },
			wantErr: provider.ErrConfigInvalid,
		},
		{
			name:    "没有密钥",
			damage:  func(r *provider.CreateRequest) { r.Secrets = credentialsFor("") },
			wantErr: provider.ErrSecretNotConfigured,
		},
		{
			name:    "金额不是正数",
			damage:  func(r *provider.CreateRequest) { r.Amount = 0 },
			wantErr: nil, // 这一条报的是普通错误，不是上面那两个哨兵
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newChannelServer(t)
			request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
				paramCreatePath: "/v1/netpay/trade/h5-pay",
			})
			tc.damage(&request)

			_, err := New().Create(context.Background(), request)
			if err == nil {
				t.Fatal("该报错，实际一个字节发出去了")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			if sent := server.requestCount(); sent != 0 {
				t.Fatalf("本地拒时**一个请求都不该发出去**，实际发了 %d 个", sent)
			}
		})
	}
}

// TestCreateH5IgnoresTheChannelLevelCreatePath：渠道 config 里那条下单路径**只对小程序生效**。
//
// 这一条是 H5 最容易踩的一个坑：运营在渠道上填了一条下单路径（默认值就是小程序那条），
// 然后在支付方式上忘了填 params——如果 H5 分支回落到了 config，报文会被发到微信小程序的
// 下单接口上，渠道回一句含糊的错，而配置看上去全都填了。
func TestCreateH5IgnoresTheChannelLevelCreatePath(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")
	config["endpoints"] = map[string]any{"create": "/v1/netpay/wx/unified-order"}

	server := newChannelServer(t)
	config["baseURL"] = server.URL

	request := h5CreateRequestFor(config, nil) // params 全空
	_, err := New().Create(context.Background(), request)
	if !errors.Is(err, provider.ErrConfigInvalid) {
		t.Fatalf("渠道级的下单路径不该被 H5 用上，want ErrConfigInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), "createPath") {
		t.Fatalf("错误串该点名 createPath，实际 %v", err)
	}
	if sent := server.requestCount(); sent != 0 {
		t.Fatalf("拒掉时一个请求都不该发，实际发了 %d 个", sent)
	}
}

// TestCreateDispatchesOnAction：分派认 action，**认不出来就报错**，不静默走某一条分支。
//
// 走错分支的代价是一笔按 H5 建的、却回给小程序一份 h5Url 的支付——客户端拿到看不懂的东西
// 之后什么都做不了，而流水里一切正常。
func TestCreateDispatchesOnAction(t *testing.T) {
	server := newChannelServer(t)

	request := createRequestFor(channelConfig(server.URL))
	request.Method.Action = provider.ActionQrcode

	_, err := New().Create(context.Background(), request)
	if !errors.Is(err, provider.ErrConfigInvalid) {
		t.Fatalf("want ErrConfigInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), string(provider.ActionQrcode)) {
		t.Fatalf("错误串该点名那个 action，实际 %v", err)
	}
	if sent := server.requestCount(); sent != 0 {
		t.Fatalf("拒掉时一个请求都不该发，实际发了 %d 个", sent)
	}
}

// TestCreateH5TimeoutIsNotFailure：与小程序那条同源——请求发出去了、没等到应答时，钱可能已经
// 收了。既不能标失败，也不能重试。
func TestCreateH5TimeoutIsNotFailure(t *testing.T) {
	release := make(chan struct{})
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) { <-release })
	t.Cleanup(func() { close(release) })

	adapter := &Provider{client: httpx.New(50 * time.Millisecond)}
	request := h5CreateRequestFor(channelConfig(server.URL), map[string]string{
		paramCreatePath: "/v1/netpay/trade/h5-pay",
	})
	result, err := adapter.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("超时**不该**返回 error：它意味着「发过了」，调用方要按结果不明处置。实际 %v", err)
	}
	if result.Result != provider.ResultTimeout {
		t.Fatalf("Result = %q, want timeout", result.Result)
	}
	if result.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1（超时绝不重试）", result.Attempts)
	}
}
