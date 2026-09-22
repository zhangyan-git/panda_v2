package ums

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/httpx"
)

// channelServer 是一个**照老系统那套算法验签**的假渠道。
//
// 它刻意不调用本包的 Sign / Payload：那样只会证明「我们自己签自己验」永远成立。它用的是
// config_test.go 里的 legacyAuthHeader——老系统那段代码的复刻。于是「我们发出去的报文能不能
// 被一个照老系统实现的对手方认下来」才是一条证据。
type channelServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []capturedRequest
	// handler 是应答体，由每个用例自己给。
	handler http.HandlerFunc
}

type capturedRequest struct {
	path   string
	method string
	// rawQuery 是**没解码**的查询串。H5 那条路的全部认证参数都在它里面，而且「签名算在未编码
	// 的 content 上」这条判据只有在拿到原始串时才验得了（解过码的串看不出编码这一步做没做）。
	rawQuery string
	body     []byte
	headers  http.Header
	// signature 是 Authorization 头的值（小程序那条路）。
	signature string
}

func (c *channelServer) last() capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return capturedRequest{}
	}
	return c.requests[len(c.requests)-1]
}

// setHandler 换掉应答。**走锁**：handler 在测试 goroutine 上写、在 HTTP 服务端的 goroutine
// 上读，直接赋值在 -race 下是一条真的数据竞争。
func (c *channelServer) setHandler(handler http.HandlerFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = handler
}

func (c *channelServer) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// successBody 是一份能过 interpretCreate 的成功应答：errCode 是 SUCCESS，且 miniPayRequest
// 六个键齐备。六个键的值本身没有意义，它们的**存在与非空**才是判据。
const successBody = `{"errCode":"SUCCESS","errMsg":"","miniPayRequest":{` +
	`"appId":"wx-app","nonceStr":"n-1","package":"prepay_id=wx123",` +
	`"paySign":"S-1","signType":"RSA","timeStamp":"1767225600"}}`

func newChannelServer(t *testing.T) *channelServer {
	t.Helper()
	server := &channelServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured := capturedRequest{
			path: r.URL.Path, method: r.Method, rawQuery: r.URL.RawQuery, body: body,
			headers: r.Header, signature: r.Header.Get(headerAuthorization),
		}
		server.mu.Lock()
		server.requests = append(server.requests, captured)
		handler := server.handler
		server.mu.Unlock()

		if handler != nil {
			handler(w, r)
			return
		}
		_, _ = w.Write([]byte(successBody))
	}))
	t.Cleanup(server.Close)
	return server
}

// authorizationPattern 是那句四段式 Sprintf 造出来的形状。写在测试里而不是复用本包的
// parseAuthorization：验签若用被测代码自己的解析器，验的就成了「我们自己跟自己对得上」。
var authorizationPattern = regexp.MustCompile(
	`^OPEN-BODY-SIG AppId="([^"]*)", Timestamp="([^"]*)", Nonce="([^"]*)", Signature="([^"]*)"$`)

// verifySignature 用老系统那段算法验一次签名，验不过直接让用例失败。
//
// 它比对的是**整条头**（而不是只比签名那一段）：老系统那句 Sprintf 里的空格与引号也是契约的
// 一部分，只比签名会漏掉「格式差了但摘要是对的」。
func (c *channelServer) verifySignature(t *testing.T, secret string) capturedRequest {
	t.Helper()
	request := c.last()
	if request.path == "" {
		t.Fatal("渠道一个请求都没收到")
	}
	matches := authorizationPattern.FindStringSubmatch(request.signature)
	if matches == nil {
		t.Fatalf("Authorization 头拆不出四段：%q", request.signature)
	}
	appID, timestamp, nonce := matches[1], matches[2], matches[3]
	if appID != testAppID {
		t.Fatalf("AppId = %q, want %q", appID, testAppID)
	}
	if want := legacyAuthHeader(appID, secret, timestamp, nonce, request.body); request.signature != want {
		t.Fatalf("签名对不上：\n 收到 %s\n 期望 %s", request.signature, want)
	}
	return request
}

func createRequestFor(config map[string]any) provider.CreateRequest {
	return provider.CreateRequest{
		PaymentNo:    "PAY20260917000000000001",
		OrderNo:      "ORD20260917000000000001",
		UserID:       "user-1",
		Amount:       12800,
		Subject:      "一杯拿铁",
		RequestID:    "req-1",
		WalletOpenID: "open-user-9",
		Attach:       map[string]string{"deviceNo": "D-9"},
		NotifyURL:    "https://api.example.com/v1/payments/callback/ums_wechat",
		Method:       channelMethod(config),
		Secrets:      credentialsFor(testSecret),
	}
}

// TestCreateSendsASignedBody 是这一族端到端形状的第一条：报文被一个照老系统实现的对手方
// **验签通过**，且报文里的每一项都对得上。
func TestCreateSendsASignedBody(t *testing.T) {
	server := newChannelServer(t)

	result, err := New().Create(context.Background(), createRequestFor(channelConfig(server.URL)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Result != provider.ResultSuccess {
		t.Fatalf("Result = %q（%s %s）", result.Result, result.FailureCode, result.FailureMessage)
	}
	if result.HTTPStatus != http.StatusOK || result.Attempts != 1 {
		t.Fatalf("HTTPStatus=%d Attempts=%d，两个都要落进 payment_provider_calls",
			result.HTTPStatus, result.Attempts)
	}

	request := server.verifySignature(t, testSecret)
	if request.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", request.method)
	}
	if request.path != defaultCreatePath {
		t.Fatalf("path = %q, want %q", request.path, defaultCreatePath)
	}
	if got := request.headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}

	var sent createBody
	if err := json.Unmarshal(request.body, &sent); err != nil {
		t.Fatalf("解报文：%v（%s）", err, request.body)
	}
	if sent.AppID != testAppID || sent.Mid != testMid || sent.Tid != testTid {
		t.Fatalf("appId/mid/tid 该取配置：%+v", sent)
	}
	// merOrderId 是**支付单号加上账户要求的前缀**（见 orderid.go）：后半段是我们的锚点，前缀是
	// 渠道那边的账户规则——2026-09-21 拿真实账户打的时候，不带前缀的单连收银台都进不去
	// （「无效订单号，订单号必须以3CYM开头」）。
	if sent.MerOrderID != "3CYMPAY20260917000000000001" {
		t.Fatalf("merOrderId = %q，want 3CYM 前缀 + 支付单号", sent.MerOrderID)
	}
	// msgId 取调用方给的幂等号（老系统两者同值，所以回落过去等同于老系统）。
	if sent.MsgID != "req-1" {
		t.Fatalf("msgId = %q, want req-1", sent.MsgID)
	}
	if sent.InstMid != defaultInstMid || sent.TradeType != defaultTradeType {
		t.Fatalf("instMid/tradeType 该取默认值：%+v", sent)
	}
	if sent.DomainName != testDomain || sent.SourceCode != testSource {
		t.Fatalf("domainName/sourceCode 该取渠道登记值：%+v", sent)
	}
	if sent.SubOpenID != "open-user-9" {
		t.Fatalf("subOpenId = %q，微信小程序支付必须指定付款人", sent.SubOpenID)
	}
	if sent.NotifyURL != "https://api.example.com/v1/payments/callback/ums_wechat" {
		t.Fatalf("notifyUrl = %q", sent.NotifyURL)
	}
	// 每一个键都无条件出现，连恒为空串的 srcReserve 也是（照抄老系统的 map 语义）。
	if !strings.Contains(string(request.body), `"srcReserve":""`) {
		t.Fatalf("srcReserve 该以空串出现：%s", request.body)
	}
	if !strings.Contains(string(request.body), `"returnUrl":""`) {
		t.Fatalf("没配 returnUrl 时该发空串：%s", request.body)
	}

	// 回给客户端的六个键原样带出来，且**只有**这六个：渠道多塞的字段不该跟着进客户端。
	want := map[string]string{
		"appId": "wx-app", "nonceStr": "n-1", "package": "prepay_id=wx123",
		"paySign": "S-1", "signType": "RSA", "timeStamp": "1767225600",
	}
	if len(result.PayParams) != len(want) {
		t.Fatalf("PayParams = %v", result.PayParams)
	}
	for key, value := range want {
		if result.PayParams[key] != value {
			t.Fatalf("PayParams[%q] = %q, want %q", key, result.PayParams[key], value)
		}
	}
}

// TestCreateSendsDivisionWithTheOrder：有分账指令时，三个分账键随下单一次发出去。
//
// 解报文用的是**报文里真实的键名**，不是本包的结构体：子单那个键在老系统的内部口径里叫
// amount，进报文前才换成 totalAmount（order_service.go:1977-1979）。拿本包的结构体解会把
// 这个差别一起藏起来——两边都写错的话永远不会红。
func TestCreateSendsDivisionWithTheOrder(t *testing.T) {
	server := newChannelServer(t)
	request := createRequestFor(channelConfig(server.URL))
	request.Division = &provider.DivisionInstruction{
		PlatformAmount: 800,
		SubOrders: []provider.DivisionSubOrder{
			{Mid: "SUB-MID-1", Amount: 12000},
		},
	}

	if _, err := New().Create(context.Background(), request); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sent := server.verifySignature(t, testSecret)
	var body struct {
		TotalAmount    int64  `json:"totalAmount"`
		DivisionFlag   *bool  `json:"divisionFlag"`
		PlatformAmount *int64 `json:"platformAmount"`
		SubOrders      []struct {
			Mid          string `json:"mid"`
			TotalAmount  int64  `json:"totalAmount"`
			LegacyAmount *int64 `json:"amount"`
		} `json:"subOrders"`
	}
	if err := json.Unmarshal(sent.body, &body); err != nil {
		t.Fatalf("解报文：%v（%s）", err, sent.body)
	}
	if body.DivisionFlag == nil || !*body.DivisionFlag {
		t.Fatalf("divisionFlag 该为 true：%s", sent.body)
	}
	if body.PlatformAmount == nil || *body.PlatformAmount != 800 {
		t.Fatalf("platformAmount = %v，want 800", body.PlatformAmount)
	}
	if len(body.SubOrders) != 1 {
		t.Fatalf("subOrders = %+v，want 一条", body.SubOrders)
	}
	if sub := body.SubOrders[0]; sub.Mid != "SUB-MID-1" || sub.TotalAmount != 12000 {
		t.Fatalf("子单 = %+v，want mid=SUB-MID-1 totalAmount=12000", sub)
	}
	if body.SubOrders[0].LegacyAmount != nil {
		t.Fatalf("子单里出现了 amount 键：渠道不认它，那条子单会被当成没有金额（%s）", sent.body)
	}
	// 规范那条硬约束，也是渠道唯一会拿来判「这笔分账对不对」的东西：
	// totalAmount = Σ subOrders.totalAmount + platformAmount。
	var subTotal int64
	for _, sub := range body.SubOrders {
		subTotal += sub.TotalAmount
	}
	if subTotal+*body.PlatformAmount != body.TotalAmount {
		t.Fatalf("恒等式不成立：Σ子单 %d + 平台 %d ≠ totalAmount %d",
			subTotal, *body.PlatformAmount, body.TotalAmount)
	}
}

// TestCreateSendsAZeroPlatformAmount：平台自留那一份是 0 时，`platformAmount` 这个键**仍然
// 要在**，而且要是数字 0。
//
// 「自留 0」不是边角情形：一条把比例全分给接收方的规则配出来就是这个形状。而报文里那个键带
// omitempty，字段写成 int64 的话 0 会被整条吞掉——报文里少的是恒等式 `totalAmount = Σ子单 +
// platformAmount` 的一项，渠道要么拒，要么按它自己的默认补一个数，两种都不是我们要的。所以
// divisionFields.PlatformAmount 是指针，这条用例钉住那件事，别让后来的人把它「简化」回去。
func TestCreateSendsAZeroPlatformAmount(t *testing.T) {
	server := newChannelServer(t)
	request := createRequestFor(channelConfig(server.URL))
	request.Division = &provider.DivisionInstruction{
		PlatformAmount: 0,
		SubOrders:      []provider.DivisionSubOrder{{Mid: "SUB-MID-1", Amount: request.Amount}},
	}

	if _, err := New().Create(context.Background(), request); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sent := server.verifySignature(t, testSecret)
	// 直接看报文原文：解成 `*int64` 再断言非 nil 也过得去，但那样验的是「解码器认为键在不在」，
	// 而这里要钉的恰恰是**报文里有这个键**——0 与「没有这个键」在渠道那边是两件事。
	if !strings.Contains(string(sent.body), `"platformAmount":0`) {
		t.Fatalf("报文里没有 platformAmount:0：%s", sent.body)
	}

	var body struct {
		TotalAmount    int64  `json:"totalAmount"`
		DivisionFlag   *bool  `json:"divisionFlag"`
		PlatformAmount *int64 `json:"platformAmount"`
		SubOrders      []struct {
			TotalAmount int64 `json:"totalAmount"`
		} `json:"subOrders"`
	}
	if err := json.Unmarshal(sent.body, &body); err != nil {
		t.Fatalf("解报文：%v（%s）", err, sent.body)
	}
	if body.DivisionFlag == nil || !*body.DivisionFlag {
		t.Fatalf("divisionFlag 该为 true：%s", sent.body)
	}
	// 自留 0 意味着整单都分出去，恒等式这时候最容易看走眼（0 被吞掉之后它依然「成立」——
	// 只是成立在一个少了平台那一项的和上，而那正是渠道会拒的那一份报文）。
	var subTotal int64
	for _, sub := range body.SubOrders {
		subTotal += sub.TotalAmount
	}
	if subTotal != body.TotalAmount || subTotal != request.Amount {
		t.Fatalf("Σ子单 %d ≠ totalAmount %d（实付 %d）", subTotal, body.TotalAmount, request.Amount)
	}
}

// TestCreateOmitsDivisionKeysWithoutASubOrder：没有分账时三个键**一个都不出现**。
//
// 两种「没有分账」都要覆盖：调用方给 nil（账户出资、没命中规则），以及给了一个没有子单的
// 指令。后者尤其不能翻成 `"divisionFlag":true` + 空数组——规范说 divisionFlag=true 时
// subOrders 不能为空，那样渠道拒的是**整笔支付**，不是分账。
func TestCreateOmitsDivisionKeysWithoutASubOrder(t *testing.T) {
	cases := []struct {
		name     string
		division *provider.DivisionInstruction
	}{
		{"nil", nil},
		{"空指令（没有子单）", &provider.DivisionInstruction{PlatformAmount: 12800}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newChannelServer(t)
			request := createRequestFor(channelConfig(server.URL))
			request.Division = testCase.division

			if _, err := New().Create(context.Background(), request); err != nil {
				t.Fatalf("Create: %v", err)
			}

			sent := server.verifySignature(t, testSecret)
			for _, absent := range []string{"divisionFlag", "platformAmount", "subOrders"} {
				if strings.Contains(string(sent.body), `"`+absent+`"`) {
					t.Fatalf("这一笔没有分账，%s 不该出现：%s", absent, sent.body)
				}
			}
		})
	}
}

// TestCreateSignsWhatItSends 钉死「签什么就发什么」。
//
// 这一族的签名覆盖**报文体本身**（待签串末段是它的 sha256），所以体只能编一次：编两遍只要
// 有一处空值判断不同，签名就永远对不上，而两边打印出来的报文看上去一模一样。
func TestCreateSignsWhatItSends(t *testing.T) {
	server := newChannelServer(t)

	// 让几个字段取**空值**：体里那些空串最容易让人想「顺手 omitempty 掉」，而签的与发的
	// 一旦不是同一份字节，这里就红。
	request := createRequestFor(channelConfig(server.URL))
	request.WalletOpenID = ""
	request.RequestID = ""
	request.Method.Params = map[string]string{paramTradeType: "ALIPAY"}

	if _, err := New().Create(context.Background(), request); err != nil {
		t.Fatalf("Create: %v", err)
	}
	server.verifySignature(t, testSecret)
}

// TestCreateAmountIsFenInTheBody 单独立一条：这一族的报文用**整数分**记金额。
//
// 它与 hmac_body 那一族正相反（那边是元、float64），form_md5 又是字符串——三种都不一样，
// 所以这是接这家渠道时最容易搞错的一处，值得一条独立的断言。写成 1 元（0.01）是本条要抓的错。
func TestCreateAmountIsFenInTheBody(t *testing.T) {
	cases := []struct {
		amount int64
		want   string
	}{
		{1, `"totalAmount":1`},
		{12800, `"totalAmount":12800`},
		{99999999, `"totalAmount":99999999`},
	}

	for _, tc := range cases {
		server := newChannelServer(t)
		request := createRequestFor(channelConfig(server.URL))
		request.Amount = tc.amount

		if _, err := New().Create(context.Background(), request); err != nil {
			t.Fatalf("Create: %v", err)
		}
		body := string(server.last().body)
		if !strings.Contains(body, tc.want) {
			t.Fatalf("%d 分该原样报成分：%s", tc.amount, body)
		}
		// 反过来钉一次：分不该被换算成元。1 分报成 0.01 是这条最要防的错。
		if strings.Contains(body, `"totalAmount":0.`) {
			t.Fatalf("金额被当成元了：%s", body)
		}
	}
}

// TestCreateNeverGoesOutWithoutThePinnedInputs：四种「本地拒」。
//
// 共同的理由是：它们都会让渠道回一句与本意无关的错（「签名错误」「参数不对」），而那条错误
// 会盖住真正的原因。**一个字节都不发出去**是判据——发出去了就说明我们放弃了这个判断。
func TestCreateNeverGoesOutWithoutThePinnedInputs(t *testing.T) {
	cases := []struct {
		name    string
		damage  func(*provider.CreateRequest)
		wantErr error
	}{
		{
			// openid 是付款人，而微信的预支付单必须指定付款人。取不到就本地拒：发一份
			// subOpenId 为空的报文过去，渠道要么拒、要么更糟——把它当成某个默认用户。
			name:    "没有 openid",
			damage:  func(r *provider.CreateRequest) { r.WalletOpenID = "" },
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
			request := createRequestFor(channelConfig(server.URL))
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

// TestCreateAlipayNeedsNoOpenID：一个渠道挂三个支付方式的那一处。
//
// 渠道默认是 MINI（要 openid），但把 tradeType 覆盖成 ALIPAY 的那条支付方式**不该**被要求
// 带 openid——照 config 默认值判会让支付宝那条一条都发不出去。同时报文体里的三个值必须以
// params 为准，且 subOpenId 照旧出现（空串）。
func TestCreateAlipayNeedsNoOpenID(t *testing.T) {
	server := newChannelServer(t)
	request := createRequestFor(channelConfig(server.URL))
	request.WalletOpenID = ""
	request.Method.Params = map[string]string{
		paramTradeType:  "ALIPAY",
		paramCreatePath: "/v1/netpay/trade/create",
		paramInstMid:    "ALIPAYDEFAULT",
	}

	result, err := New().Create(context.Background(), request)
	if err != nil {
		t.Fatalf("支付宝那条支付方式不该要求 openid：%v", err)
	}
	if result.Result != provider.ResultSuccess {
		t.Fatalf("Result = %q", result.Result)
	}

	request2 := server.verifySignature(t, testSecret)
	if request2.path != "/v1/netpay/trade/create" {
		t.Fatalf("params 里的 createPath 没生效：%q", request2.path)
	}
	var sent createBody
	if err := json.Unmarshal(request2.body, &sent); err != nil {
		t.Fatalf("解报文：%v", err)
	}
	if sent.TradeType != "ALIPAY" || sent.InstMid != "ALIPAYDEFAULT" {
		t.Fatalf("params 没覆盖住 config：%+v", sent)
	}
	if sent.SubOpenID != "" {
		t.Fatalf("subOpenId 该是空串（这个键无条件出现），实际 %q", sent.SubOpenID)
	}
}

// TestCreateRejectsBrokenConfigWithoutCallingOut：配置解不开时连请求都不该发。
func TestCreateRejectsBrokenConfigWithoutCallingOut(t *testing.T) {
	server := newChannelServer(t)
	config := channelConfig(server.URL)
	delete(config, "mid")

	_, err := New().Create(context.Background(), createRequestFor(config))
	if !errors.Is(err, provider.ErrConfigInvalid) {
		t.Fatalf("want ErrConfigInvalid, got %v", err)
	}
	if sent := server.requestCount(); sent != 0 {
		t.Fatalf("配置解不开时不该发出任何请求，实际发了 %d 个", sent)
	}
}

// TestCreateResultClassification：同一份应答（或同一类应答）落在哪一档。
func TestCreateResultClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		want     provider.Result
		wantCode string
	}{
		{
			name: "下单成功", status: 200,
			body: successBody,
			want: provider.ResultSuccess,
		},
		{
			// 应答里压根没有 errCode，等于我们没读懂。**不能当失败**：渠道可能已经建单了，
			// 标失败会让用户去付第二遍，而第一笔若成了就是重复收款。
			name: "没有 errCode", status: 200,
			body: `{"errMsg":"whatever"}`,
			want: provider.ResultUnknown, wantCode: "AMBIGUOUS_PROVIDER_CODE",
		},
		{
			name: "重复下单", status: 200,
			body: `{"errCode":"DUP_ORDER","errMsg":"订单已存在"}`,
			want: provider.ResultUnknown, wantCode: "AMBIGUOUS_PROVIDER_CODE",
		},
		{
			name: "渠道还在处理上一笔", status: 200,
			body: `{"errCode":"ORDER_PROCESSING","errMsg":"处理中"}`,
			want: provider.ResultUnknown, wantCode: "AMBIGUOUS_PROVIDER_CODE",
		},
		{
			// 渠道明确拒了这次调用（商户号不存在、签名错误、参数不合法），它没有建单。
			name: "渠道明确拒绝", status: 200,
			body: `{"errCode":"PARAM_ERROR","errMsg":"参数不合法"}`,
			want: provider.ResultFailed, wantCode: "provider_declined",
		},
		{
			// errCode 说成功却给不出能用的支付参数：钱可能已经在渠道那边挂了单，而客户端
			// 什么都拉不起来。同一档，理由同上。
			name: "成功但没有 miniPayRequest", status: 200,
			body: `{"errCode":"SUCCESS","errMsg":""}`,
			want: provider.ResultUnknown, wantCode: "INVALID_MINI_PAY_REQUEST",
		},
		{
			name: "miniPayRequest 少了键", status: 200,
			body: `{"errCode":"SUCCESS","miniPayRequest":{"appId":"wx-app","nonceStr":"n"}}`,
			want: provider.ResultUnknown, wantCode: "INVALID_MINI_PAY_REQUEST",
		},
		{
			name: "miniPayRequest 的键是空的", status: 200,
			body: `{"errCode":"SUCCESS","miniPayRequest":{"appId":"","nonceStr":"n","package":"p","paySign":"s","signType":"RSA","timeStamp":"1"}}`,
			want: provider.ResultUnknown, wantCode: "INVALID_MINI_PAY_REQUEST",
		},
		{
			name: "miniPayRequest 不是对象", status: 200,
			body: `{"errCode":"SUCCESS","miniPayRequest":"拉不起来"}`,
			want: provider.ResultUnknown, wantCode: "INVALID_MINI_PAY_REQUEST",
		},
		{
			// 4xx 是协议层就拒了这次请求，它没有建单。配置写错时用户能立刻换一种方式付。
			name: "4xx", status: 400,
			body: `{"errCode":"SIGN_ERROR","errMsg":"签名错误"}`,
			want: provider.ResultFailed, wantCode: "HTTP_400",
		},
		{
			// 5xx：对面自己出了故障，它到底有没有建单我们不知道。**绝不能当失败**。
			name: "5xx", status: 502,
			body: `{"errCode":"SUCCESS"}`,
			want: provider.ResultUnknown, wantCode: "HTTP_502",
		},
		{
			name: "报文读不懂", status: 200,
			body: `<html>hello</html>`,
			want: provider.ResultUnknown, wantCode: "UNPARSEABLE_RESPONSE",
		},
		{
			// 4xx 上面那条例外同样管这里：渠道在协议层就拒了。
			name: "报文读不懂且是 4xx", status: 404,
			body: `<html>hello</html>`,
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

			result, err := New().Create(context.Background(), createRequestFor(channelConfig(server.URL)))
			if err != nil {
				t.Fatalf("请求发出去之后的失败**不该**返回 error（err 只在没发出去时非 nil）：%v", err)
			}
			if result.Result != tc.want {
				t.Fatalf("Result = %q, want %q（%s）", result.Result, tc.want, result.FailureMessage)
			}
			if result.FailureCode != tc.wantCode {
				t.Fatalf("FailureCode = %q, want %q", result.FailureCode, tc.wantCode)
			}
			if result.HTTPStatus != tc.status || result.Attempts != 1 {
				t.Fatalf("HTTPStatus=%d Attempts=%d，两个都要落进 payment_provider_calls",
					result.HTTPStatus, result.Attempts)
			}
			// 摘要里不许出现密钥（方案 11.5）：它是脱敏后要落库的那一栏。
			if summary := fmt.Sprintf("%v", result.ResponseSummary); strings.Contains(summary, testSecret) {
				t.Fatalf("应答摘要里出现了密钥：%v", result.ResponseSummary)
			}
		})
	}
}

// TestCreateTimeoutIsNotFailure 钉死这个包里最要紧的一条：请求发出去了、没等到应答时，
// **钱可能已经收了**。既不能标失败（用户付过的钱会对不上账），也不能重试。
func TestCreateTimeoutIsNotFailure(t *testing.T) {
	release := make(chan struct{})
	server := newChannelServer(t)
	server.setHandler(func(w http.ResponseWriter, r *http.Request) { <-release })
	t.Cleanup(func() { close(release) })

	adapter := &Provider{client: httpx.New(50 * time.Millisecond)}
	result, err := adapter.Create(context.Background(), createRequestFor(channelConfig(server.URL)))
	if err != nil {
		t.Fatalf("超时**不该**返回 error：它意味着「发过了」，调用方要按结果不明处置。实际 %v", err)
	}
	if result.Result != provider.ResultTimeout {
		t.Fatalf("Result = %q, want timeout", result.Result)
	}
	if result.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1（超时绝不重试）", result.Attempts)
	}
	if result.FailureCode != "TIMEOUT" {
		t.Fatalf("FailureCode = %q", result.FailureCode)
	}
}

// TestCreateNotSentReturnsError 是上一条的反面：连接根本没建起来时，渠道侧什么都没发生。
func TestCreateNotSentReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close() // 关掉，端口上再没有东西会应答

	result, err := New().Create(context.Background(), createRequestFor(channelConfig(url)))
	if err == nil {
		t.Fatalf("一个字节都没发出去时应当返回 error，实际 result = %q", result.Result)
	}
	if !errors.Is(err, httpx.ErrNotSent) {
		t.Fatalf("错误该是 ErrNotSent，实际 %v", err)
	}
	if result.Result != provider.ResultUnknown || result.FailureCode != "NOT_SENT" {
		t.Fatalf("Result = %q FailureCode = %q，没发出去是我们自己的问题，不该标成超时",
			result.Result, result.FailureCode)
	}
}

// TestCreateRecordsTheHostItTalkedTo：摘要里记的是**这次打到了哪台机器**，为了让「联调的
// 单打到了生产渠道上」这类事故在流水里看得见——它是排查时第一个会去看的字段。
//
// 这一格原来叫 mode（沙箱/生产是渠道行上的一列）。那一列随渠道表一起没了：今天「打的是哪一边」
// 完全由 baseURL 决定（联调时把它指到假服务端），所以该记的是它。而记 host 不记整条地址，是
// 因为路径部分属于我们这一版的接口定义、不属于「打到了哪儿」。
func TestCreateRecordsTheHostItTalkedTo(t *testing.T) {
	server := newChannelServer(t)

	result, err := New().Create(context.Background(), createRequestFor(channelConfig(server.URL)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := hostOf(server.URL)
	if got := result.ResponseSummary["baseURLHost"]; got != want {
		t.Fatalf("ResponseSummary[baseURLHost] = %v, want %v", got, want)
	}
	// 记的是主机名不是整条地址：地址里的路径与查询串都不该进流水。
	if s, ok := result.ResponseSummary["baseURLHost"].(string); ok && strings.Contains(s, "/") {
		t.Fatalf("baseURLHost 里出现了路径：%q", s)
	}
}

// TestTypeAssertionsHold：装配处靠这几个断言探能力，探不到的后果必须是安全的默认。
func TestTypeAssertionsHold(t *testing.T) {
	adapter := New()
	if _, ok := any(adapter).(provider.Querier); !ok {
		t.Fatal("这一族有查单接口，超时那笔单的唯一出路就是它")
	}
	if _, ok := any(adapter).(provider.SecretSlotter); !ok {
		t.Fatal("密钥住在凭据槽里，没有它装配处只能退回渠道行的 secret_ref")
	}
	if _, ok := any(adapter).(provider.HeaderRecorder); !ok {
		t.Fatal("RecordableHeaders 由装配处探；这一族返回空名单，回调的请求头因此不留档")
	}
}

// TestNameAndAckShape：注册名与应答体的形状。
//
// 应答体是本仓库认定的形状（老系统在这个入口上什么都不回），见 ums.go 里的说明。
func TestNameAndAckShape(t *testing.T) {
	if got := New().Name(); got != "ums" {
		t.Fatalf("Name = %q, want ums（它要与 payment_channels.provider 的值一致）", got)
	}

	accepted := New().Ack(channelMethod(nil), true)
	if accepted.Status != http.StatusOK || string(accepted.Body) != `{"msg":"SUCCESS"}` {
		t.Fatalf("收下时应答 = %d %s", accepted.Status, accepted.Body)
	}
	if accepted.ContentType != "application/json" {
		t.Fatalf("ContentType = %q", accepted.ContentType)
	}

	rejected := New().Ack(channelMethod(nil), false)
	if rejected.Status >= 200 && rejected.Status < 300 {
		t.Fatalf("拒绝时必须回非 2xx，实际 %d——回 2xx 等于告诉渠道别再投了", rejected.Status)
	}
	// 应答体里一个字都不该提到原因（见 provider.Ack 的注释）。
	body := strings.ToLower(string(rejected.Body))
	for _, leak := range []string{"signature", "secret", "appkey", "amount", "timestamp"} {
		if strings.Contains(body, leak) {
			t.Fatalf("应答体里出现了内部原因 %q：%s", leak, rejected.Body)
		}
	}
}
