package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// closedURL 返回一个「连上去一定失败」的地址：起一个 server 再关掉它，端口上就再没有
// 东西会应答了。dial 阶段的失败是这个包里**唯一**允许重试的一类。
func closedURL(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()
	return url
}

// TestDoRetriesOnlyWhenNothingWasSent 钉死重试策略的一半：连接根本没建立时重试。
func TestDoRetriesOnlyWhenNothingWasSent(t *testing.T) {
	client := New(time.Second)

	_, err := client.Do(context.Background(), Request{URL: closedURL(t) + "/pay", Body: []byte("{}")})
	if err == nil {
		t.Fatal("连不上的地址该报错")
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("错误该是 *httpx.Error（适配器要读它的 NotSent 与 Attempts），实际 %T", err)
	}
	if !httpErr.NotSent {
		t.Fatalf("dial 失败意味着一个字节都没发出去，NotSent 该为真：%v", err)
	}
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("errors.Is(err, ErrNotSent) 该成立（适配器就靠它判断）：%v", err)
	}
	if httpErr.Attempts != MaxAttempts {
		t.Fatalf("Attempts = %d, want %d（可重试的失败要试满）", httpErr.Attempts, MaxAttempts)
	}
}

// TestDoDoesNotRetryOnceTheRequestWasWritten 钉死另一半，而这一半是真正要命的那一半。
//
// 服务端收下了请求、读完了体，然后**在应答之前**把连接掐掉。这时我们无从知道对面到底
// 处理了没有——而在「发起支付」这个语境里，那意味着**钱可能已经收了**。重试会再建一张
// 预支付单，或者更糟：让用户付两次。
func TestDoDoesNotRetryOnceTheRequestWasWritten(t *testing.T) {
	reads := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		reads <- struct{}{}
		// ErrAbortHandler 让 net/http 直接掐断连接，不回任何东西——这正是「请求送到了、
		// 应答没回来」的形状。
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	client := New(time.Second)
	_, err := client.Do(context.Background(), Request{URL: server.URL + "/pay", Body: []byte("{}")})
	if err == nil {
		t.Fatal("连接被掐断该报错")
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("错误该是 *httpx.Error，实际 %T", err)
	}
	if httpErr.NotSent {
		t.Fatalf("请求已经写出去了，NotSent **必须**为假：把它当成没发出去去重试，"+
			"代价是用户被收两次钱。实际 %v", err)
	}
	if httpErr.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1（不确定送没送出去的失败绝不重试）", httpErr.Attempts)
	}
	select {
	case <-reads:
	default:
		t.Fatal("服务端没收到请求，这条用例就测不到它想测的东西")
	}
}

// TestDoDoesNotRetryTimeout 是上一条的时间版本：对面收下了、但一直不回。
func TestDoDoesNotRetryTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := New(50 * time.Millisecond)
	_, err := client.Do(context.Background(), Request{URL: server.URL + "/pay", Body: []byte("{}")})
	if err == nil {
		t.Fatal("超时该报错")
	}
	var httpErr *Error
	if !errors.As(err, &httpErr) {
		t.Fatalf("错误该是 *httpx.Error，实际 %T", err)
	}
	if httpErr.NotSent {
		t.Fatalf("请求写出去了，超时**不是**「没发出去」：%v", err)
	}
	if httpErr.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1（超时绝不重试）", httpErr.Attempts)
	}
}

// TestErrorNeverCarriesTheQueryString：错误串会一路进日志、进
// payment_provider_calls.error 这一列，而查询串里可能有令牌。
func TestErrorNeverCarriesTheQueryString(t *testing.T) {
	client := New(time.Second)

	_, err := client.Do(context.Background(), Request{
		URL: closedURL(t) + "/pay?token=SUPERSECRET&sign=abcdef",
	})
	if err == nil {
		t.Fatal("连不上该报错")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("错误串里带上了查询串：%v", err)
	}
	if !strings.Contains(err.Error(), urlSecretPlaceholder) {
		t.Fatalf("查询串该被换成 %s，好让人看出**这里本来有个查询串**："+
			"抹干净而不留痕会让「地址少拼了一段」看起来像「地址对了但对面挂了」。实际 %v",
			urlSecretPlaceholder, err)
	}
	// 路径留着：定位靠它（哪个渠道的哪个接口）。
	if !strings.Contains(err.Error(), "/pay") {
		t.Fatalf("路径不该被抹掉：%v", err)
	}
}

// TestBuildFailureRedactsURL 走的是另一条路径：URL 根本没构造出来，而那种错误的原文里
// 几乎总是原样带着 URL。
func TestBuildFailureRedactsURL(t *testing.T) {
	client := New(time.Second)

	_, err := client.Do(context.Background(), Request{URL: "http://exa mple.com/pay?token=SUPERSECRET"})
	if err == nil {
		t.Fatal("非法 URL 该报错")
	}
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("URL 拼不出来意味着什么都没发出去，该是 ErrNotSent：%v", err)
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatalf("错误串里带上了查询串：%v", err)
	}
}

// TestDoCapsResponseBody：渠道（或一个伪造的渠道）回一个巨大的体。
func TestDoCapsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("a", 8<<10)
		for i := 0; i < (MaxResponseBytes/len(chunk))+2; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client := New(5 * time.Second)
	_, err := client.Do(context.Background(), Request{URL: server.URL + "/pay"})
	if err == nil {
		t.Fatal("超长响应体该被拒")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("错误串该说明是「太大了」而不是帧断了——后者看着像「对面的报文不是 JSON」：%v", err)
	}
	var httpErr *Error
	if errors.As(err, &httpErr) && httpErr.NotSent {
		t.Fatalf("应答都收到了，不该是 NotSent：%v", err)
	}
}

// TestDoSendsRequestAsGiven 钉死「发出去的就是给的这些」：方法、头、体。
func TestDoSendsRequestAsGiven(t *testing.T) {
	var gotBody string
	var gotMethod, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := New(5 * time.Second)
	response, err := client.Do(context.Background(), Request{
		URL:    server.URL + "/pay",
		Header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Body:   []byte("order_no=1&sign=abc"),
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(response.Body) != `{"ok":true}` {
		t.Fatalf("应答 = %d %s", response.StatusCode, response.Body)
	}
	if response.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", response.Attempts)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("Method = %q, want POST（这一族的下单接口都是 POST）", gotMethod)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type 被改成了 %q，请求头该原样透传", gotContentType)
	}
	if gotBody != "order_no=1&sign=abc" {
		t.Fatalf("请求体 = %q", gotBody)
	}
}

// TestDoDefaultsContentTypeOnlyWhenBodyPresent：没有体的请求不该被凭空加上 Content-Type。
func TestDoDefaultsContentTypeOnlyWhenBodyPresent(t *testing.T) {
	var withBody, withoutBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/with" {
			withBody = r.Header.Get("Content-Type")
		} else {
			withoutBody = r.Header.Get("Content-Type")
		}
	}))
	defer server.Close()

	client := New(5 * time.Second)
	if _, err := client.Do(context.Background(), Request{URL: server.URL + "/with", Body: []byte("{}")}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := client.Do(context.Background(), Request{URL: server.URL + "/without"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if withBody != "application/json" {
		t.Fatalf("有体且没指定时该补 application/json，实际 %q", withBody)
	}
	if withoutBody != "" {
		t.Fatalf("没有体时不该补 Content-Type，实际 %q", withoutBody)
	}
}

// TestDoHonoursContextCancellation：上下文取消要立刻返回，不能等满超时。
func TestDoHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	client := New(30 * time.Second) // 故意远大于取消时刻：取消该赢，不是超时该赢
	_, err := client.Do(ctx, Request{URL: server.URL + "/pay", Body: []byte("{}")})
	if err == nil {
		t.Fatal("上下文取消该报错")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("取消之后等了 %v 才返回，说明它在等超时而不是等取消", elapsed)
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{raw: "https://pay.example.com/v1/order", want: "https://pay.example.com/v1/order"},
		{raw: "https://pay.example.com/v1/order?token=x", want: "https://pay.example.com/v1/order?<redacted>"},
		{raw: "https://pay.example.com", want: "https://pay.example.com"},
		// 解析不了的整串都不打印：我们看不出哪一截是秘密。
		{raw: "://:::", want: "<unparsable url>"},
	}
	for _, tc := range cases {
		if got := redactURL(tc.raw); got != tc.want {
			t.Fatalf("redactURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestRedactURLsIn(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{
			// net/http 的错误原文就长这样：URL 被引号包着，所以引号负责收尾，
			// 后面那个冒号是句子的一部分，不该被算进 URL。
			name: "引号包着的 URL（net/http 的实际形状）",
			raw:  `Post "https://pay.example.com/v1/order?token=x": dial tcp: timeout`,
			want: `Post "https://pay.example.com/v1/order?<redacted>": dial tcp: timeout`,
		},
		{
			// 这一条是回归：主机名里带空格的畸形 URL 会被「扫到第一个空白就收尾」拦腰截断，
			// 剩下的半截连同查询串原样漏进日志（这个函数存在的全部理由就是防它）。
			name: "畸形到解析不了的 URL",
			raw:  `parse "http://exa mple.com/pay?token=SUPERSECRET": invalid character " "`,
			want: `parse "<unparsable url>": invalid character " "`,
		},
		{
			name: "带查询串但没有路径",
			raw:  "GET https://pay.example.com?sign=abc 失败",
			want: "GET https://pay.example.com?<redacted> 失败",
		},
		{
			name: "没有查询串的 URL 原样保留（路径不是秘密）",
			raw:  "POST https://pay.example.com/v1/order: connection refused",
			want: "POST https://pay.example.com/v1/order: connection refused",
		},
		{
			name: "两个 URL",
			raw:  "https://a.example.com/x?k=1 -> https://b.example.com/y?k=2",
			want: "https://a.example.com/x?<redacted> -> https://b.example.com/y?<redacted>",
		},
		{
			name: "整段都是 URL",
			raw:  "https://pay.example.com/pay?key=SECRET",
			want: "https://pay.example.com/pay?<redacted>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURLsIn(tc.raw)
			if got != tc.want {
				t.Fatalf("redactURLsIn(%q)\n got %q\nwant %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestNewFallsBackToDefaultTimeout(t *testing.T) {
	// 超时设成 0 或负数时**必须有**一个默认值：没有超时的客户端会挂在一个不响应的渠道上
	// 直到进程重启，而 payment-service 的 goroutine 是有限的。
	if got := New(0).http.Timeout; got != DefaultTimeout {
		t.Fatalf("New(0) 的超时 = %v, want %v", got, DefaultTimeout)
	}
	if got := New(-time.Second).http.Timeout; got != DefaultTimeout {
		t.Fatalf("New(负数) 的超时 = %v, want %v", got, DefaultTimeout)
	}
	if got := New(time.Second).http.Timeout; got != time.Second {
		t.Fatalf("New(1s) 的超时 = %v", got)
	}
}

// TestDoNoFollowRedirectStopsAtTheFirstHop 钉死 Request.NoFollowRedirect。
//
// 这条不是「多个开关」：银联商务的 H5 下单**没有应答报文**，它的「应答」就是一次 302
// （见 docs/unionpay-h5-pay.md 第 2 节）。跟过去读回来的是收银台的 HTML，HTTP 状态还是
// 200——与「渠道建了单」在日志里长得一模一样。
func TestDoNoFollowRedirectStopsAtTheFirstHop(t *testing.T) {
	const cashier = "https://cashier.example.com/pay?token=abc"
	var hops int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hops, 1)
		http.Redirect(w, r, cashier, http.StatusFound)
	}))
	defer server.Close()

	client := New(time.Second)
	response, err := client.Do(context.Background(),
		Request{Method: http.MethodGet, URL: server.URL + "/h5-pay", NoFollowRedirect: true})
	if err != nil {
		t.Fatalf("不跟重定向时该把 302 原样交回，而不是报错：%v", err)
	}
	if response.StatusCode != http.StatusFound {
		t.Fatalf("StatusCode = %d, want %d", response.StatusCode, http.StatusFound)
	}
	if got := response.Header.Get("Location"); got != cashier {
		t.Fatalf("Location = %q, want %q", got, cashier)
	}
	if got := atomic.LoadInt32(&hops); got != 1 {
		t.Fatalf("服务端收到了 %d 次请求，want 1——多出来的那一次就是跟去了收银台", got)
	}
}

// TestDoFollowsRedirectByDefault 钉住另一半：默认行为**没有**被上面那个开关改掉。
//
// 其余四个协议族一直在按标准库的默认行为跟重定向，改默认值等于悄悄改它们的行为，
// 而那是没有任何测试兜住的一侧。
func TestDoFollowsRedirectByDefault(t *testing.T) {
	var hops int32
	var finalBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hops, 1) == 1 {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		finalBody = "arrived"
		_, _ = w.Write([]byte(finalBody))
	}))
	defer server.Close()

	client := New(time.Second)
	response, err := client.Do(context.Background(),
		Request{Method: http.MethodGet, URL: server.URL + "/start"})
	if err != nil {
		t.Fatalf("跟重定向的路上不该出错：%v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want %d（默认要跟到终点）", response.StatusCode, http.StatusOK)
	}
	if got := string(response.Body); got != finalBody {
		t.Fatalf("body = %q, want %q", got, finalBody)
	}
	if got := atomic.LoadInt32(&hops); got != 2 {
		t.Fatalf("服务端收到了 %d 次请求，want 2", got)
	}
}
