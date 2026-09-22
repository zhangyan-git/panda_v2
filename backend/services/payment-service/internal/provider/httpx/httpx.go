// Package httpx 是支付域出网调用渠道用的 HTTP 客户端。
//
// 为什么单独一个包而不是各适配器直接 `http.Post`：出网调用在这个服务里要守的三条规矩，
// 每一条都是「漏一次就出事」的类型，写在五份适配器里意味着五处都要记得。
//
//  1. **必须有超时**。没有超时的客户端会挂在一个不响应的渠道上直到进程重启，而
//     payment-service 的 goroutine 是有限的——一个不响应的渠道能把整个服务拖垮。
//  2. **响应体必须有上限**。渠道（或一个伪造的渠道）回一个 2 GB 的体，`io.ReadAll`
//     会把它整个读进内存。参照 user-service 的微信客户端（internal/client/wechat.go）。
//  3. **错误串里不许出现请求体与查询串**。请求体里有签名原文、查询串里可能有令牌；
//     这些错误会进日志、进 payment_provider_calls 的 error 字段。参照 wechat.go 里
//     「绝不把带密钥的 URL 写进错误」那一条。
//
// # 重试只发生在「一个字节都没发出去」的时候
//
// provider.Result 已经定死了这条不变量（见 provider.go 里 ResultTimeout 的注释）：
// 请求发出去了但没等到应答时，**钱可能已经收了**，重试会再建一张预支付单，或者更糟——
// 让用户付两次。所以本包只重试**能证明请求没送到**的失败：连接没建立起来（dial 阶段失败、
// DNS 解析不了、TLS 握手失败）。其余一律原样报出去，由适配器记成 timeout / unknown。
package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultTimeout 是单次尝试的超时。渠道收银台的下单接口正常在几百毫秒内返回，
	// 10 秒足够覆盖一次慢的回声；再长就该走「超时 → 查单」那条路（见 Querier）。
	DefaultTimeout = 10 * time.Second

	// MaxResponseBytes 是响应体的上限（64 KiB）。渠道的下单应答是几百字节的 JSON，
	// 64 KiB 比它宽出两个数量级，同时又把「对面回了个巨大的东西」挡在内存之外。
	MaxResponseBytes = 64 << 10

	// MaxAttempts 是一次调用的总尝试次数上限。取 2 而不是更多：能重试的只有「连接根本
	// 没建立」，那种失败要么立刻恢复、要么就要人去修（域名写错、对面挂了），多试几次
	// 只是把一次确定的错误拖成一次慢的确定错误。
	MaxAttempts = 2

	// retryBackoff 是两次尝试之间的等待。取一个短值：可重试的失败是「连接被拒」这类
	// 瞬时状态，等一下通常就好了；等太久反而把一次发起支付拖过用户的耐心。
	retryBackoff = 200 * time.Millisecond

	// urlSecretPlaceholder 是错误串里被抹掉的查询串。见 redactURL。
	urlSecretPlaceholder = "?<redacted>"

	// maxRedirects 是**允许跟随时**最多跟几跳，与 net/http 的默认值一致。
	//
	// 这个值本来由标准库兜着，这里显式写出来是因为下面装了 CheckRedirect：装了钩子就等于
	// 接管了这条策略，不写这个上限，一次环状重定向会一直转到超时。
	maxRedirects = 10
)

// noRedirectKey 是「这次请求不许跟重定向」在 ctx 上的标记，见 Request.NoFollowRedirect。
type noRedirectKey struct{}

// ErrNotSent 表示这次请求**一个字节都没有发出去**。
//
// 它是适配器区分「我们自己的问题」与「渠道那边可能已经收了钱」的唯一依据，所以下面
// neverSent 的判据宁可保守：判错方向必须是「不确定」，绝不能是「没送出去」——把一次
// 可能已经成交的调用当成没发出去去重试，代价是用户被收两次钱。
var ErrNotSent = errors.New("http request was not sent")

// Error 是一次出网调用的失败。
//
// 它为什么不是一句普通的 fmt.Errorf：适配器在失败这条路上需要两个**结构化**的事实，
// 而它们都不是错误串能可靠承载的。
//
//   - NotSent：请求到底有没有发出去。它决定这次失败算「我们自己的问题」还是「结果不明」，
//     而这两条路的下场完全不同（见 ErrNotSent）。
//   - Attempts：实际尝试了几次。它要落进 payment_provider_calls.attempt_no——排查一笔慢的
//     发起支付时，「对面慢」与「我们重试了一次」在流水里必须分得开。
//
// Unwrap 让它与 errors.Is(err, ErrNotSent) 那套写法兼容，两种读法都对。
type Error struct {
	// NotSent 为真表示请求没发出去。
	NotSent bool
	// Attempts 是实际尝试次数（至少 1）。
	Attempts int
	// Err 是原始错误，已做过 URL 脱敏。
	Err error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// Client 是出网客户端。零值不可用，要 New。
//
// 它是并发安全的（内部那个 http.Client 就是），适配器在装配处建一个、之后只读。
type Client struct {
	http     *http.Client
	attempts int
}

// New 构造客户端。timeout <= 0 时用 DefaultTimeout。
func New(timeout time.Duration) *Client {
	return newClient(timeout, nil)
}

// NewMutualTLS 构造一个**带上商户证书**的客户端，与 New 只差一个 transport。
//
// 为什么它在这里而不是让适配器自己拼一个 http.Client：包注释那三条规矩（超时、响应体上限、
// 错误串脱敏）与重试策略，每一条都是「漏一次就出事」，而这个客户端与 New 出来的那个是同一种
// 东西——只是在某些接口上渠道要求我们出示商户证书。自己拼一个的适配器会同时丢掉那四条，
// 而丢掉的后果（一个不回应的渠道把 goroutine 挂死、一次超时被当成没发出去去重试）在
// 代码上看不出来。
//
// # 什么时候要证书
//
// 不是「这一族都要」：微信 APIv2 的四个接口里，代扣（pay/pappayapply）、解约
// （papay/deletecontract）与退款（secapi/pay/refund）要，查签约（papay/querycontract）与
// 预扣费通知（papay/pap_pay_apply）**不要**。所以客户端的装配处会同时持有两个，按接口挑一个用。
//
// 证书是**调用方加载好的**（tls.LoadX509KeyPair），这里只负责把它装进 transport：加载失败
// 属于「这条渠道这次部署没配齐」，那是配置解析那条路上该报的错，不该混进一次出网调用里。
//
// MinVersion 钉在 TLS 1.2：微信支付一律 TLS 1.2 起，标准库的默认下限比它低，而这一条写出来
// 的成本是零——降级到 TLS 1.0 只会发生在有人中间人降级的时候。
func NewMutualTLS(timeout time.Duration, cert tls.Certificate) *Client {
	return newClient(timeout, &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	})
}

// newClient 是 New 与 NewMutualTLS 的共同实现。transport 为 nil 时用标准库的默认 transport。
func newClient(timeout time.Duration, transport http.RoundTripper) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		// 超时设在 Client 上而不是从 ctx 来：ctx 的超时管的是**整次调用**（含重试与退避），
		// 这里的超时管的是**每一次尝试**。两者都要有，缺一个都会出现「一次尝试把整次调用
		// 的时间预算吃光」。
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			// 跟着重定向走是标准库的默认行为，对绝大多数渠道是对的（登录跳转、域名搬迁）。
			// 装这个钩子只为一件事：让**下单**能显式不跟（见 Request.NoFollowRedirect）——
			// 银联商务的 H5 下单就是用 302 把浏览器送去收银台，跟过去会把收银台的 HTML
			// 当应答读回来，而且 HTTP 状态还是 200，与「渠道建了单」看不出区别。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if req.Context().Value(noRedirectKey{}) != nil {
					// ErrUseLastResponse 让 Do 把最后一跳**原样交回**（不报错），调用方
					// 从 Location 取跳转地址——那正是这条路上要的东西。
					return http.ErrUseLastResponse
				}
				if len(via) >= maxRedirects {
					return fmt.Errorf("stopped after %d redirects", maxRedirects)
				}
				return nil
			},
		},
		attempts: MaxAttempts,
	}
}

// Request 是一次出网请求。
type Request struct {
	// Method 是 HTTP 方法，空则用 POST（这一族渠道的下单接口都是 POST）。
	Method string
	URL    string
	// Header 是请求头。**不放凭据**：这里的值会随连接明文发出，而响应头那侧的签名不走它。
	Header map[string]string
	Body   []byte
	// NoFollowRedirect 为真时，3xx **不跟**，原样作为应答交回（调用方从 Location 取值）。
	//
	// 默认（false）跟重定向，那是标准库的行为，也是其余四族一直以来的行为——这一项是
	// 为银联商务的 H5 下单加的：那个接口的「应答」就是一次 302，跟过去读回来的是收银台
	// 的 HTML，而不是任何能判断成败的东西。
	//
	// 顺带堵住另一个口子：跨主机重定向时 Go 会丢掉 Authorization 头，而在这一族里签名
	// 就装在 Authorization 里，跟丢一跳等于把签名发给了一个陌生地址（同主机才会保留）。
	NoFollowRedirect bool
}

// Response 是渠道的应答。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	// Attempts 是这次调用实际发出去的次数（至少 1）。它落进
	// payment_provider_calls.attempt_no，是排查「为什么这一笔慢」的第一条线索。
	Attempts int
	Duration time.Duration
}

// Do 发一次请求，必要时重试。
//
// 返回的 error 分两类，调用方必须区别对待（errors.Is 判断）：
//
//   - ErrNotSent：请求没发出去。这是我们自己的问题（域名、网络、证书），
//     支付单**不该**被推成失败——渠道侧什么都没有。
//   - 其余错误：请求可能已经送到了。当成「结果不明」处理（见包注释）。
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	startedAt := time.Now()
	var lastErr error
	for attempt := 1; attempt <= c.attempts; attempt++ {
		response, err := c.once(ctx, req)
		if err == nil {
			response.Attempts = attempt
			response.Duration = time.Since(startedAt)
			return response, nil
		}
		lastErr = err
		if !errors.Is(err, ErrNotSent) {
			// 送出去过或不确定送没送出去——**绝不重试**，理由见包注释。
			return nil, &Error{Attempts: attempt, Err: err}
		}
		if attempt < c.attempts {
			if err := sleep(ctx, retryBackoff); err != nil {
				return nil, &Error{Attempts: attempt, Err: err}
			}
		}
	}
	return nil, &Error{NotSent: true, Attempts: c.attempts, Err: lastErr}
}

// once 是一次尝试。
func (c *Client) once(ctx context.Context, req Request) (*Response, error) {
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = http.MethodPost
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		// URL 拼不出来。**错误里不带那个 URL**：它可能带着查询串里的令牌，而这条错误
		// 会一路进日志。带上「哪个渠道的哪个接口」就够定位了——调用方在错误外面加渠道码。
		return nil, fmt.Errorf("%w: build request: %v", ErrNotSent, withoutURL(err))
	}
	for name, value := range req.Header {
		httpReq.Header.Set(name, value)
	}
	if len(req.Body) > 0 && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.NoFollowRedirect {
		// 标记挂在**请求自己的 ctx** 上：CheckRedirect 收到的那个 req 是重定向后的新请求，
		// 而它的 ctx 由原请求继承而来，所以标记能一路带到最后一跳。
		httpReq = httpReq.WithContext(context.WithValue(httpReq.Context(), noRedirectKey{}, true))
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if neverSent(err) {
			// url.Error 的 Error() 会把完整 URL（含查询串）抄进去，所以取它包着的那个
			// 原始错误。见包注释第 3 条。
			return nil, fmt.Errorf("%w: %s %s: %v", ErrNotSent, method, redactURL(req.URL), cause(err))
		}
		return nil, fmt.Errorf("%s %s: %v", method, redactURL(req.URL), cause(err))
	}
	defer resp.Body.Close()

	// 多读一个字节用来判断「是不是被截断了」：正好读满上限时无法区分「就是这么大」与
	// 「后面还有」。截断的 JSON 解出来会是「格式不对」，那会把一次「对面回了个巨大的
	// 东西」误导成「对面回的东西不是 JSON」。
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response body: %v", method, redactURL(req.URL), err)
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("%s %s: response body exceeds %d bytes",
			method, redactURL(req.URL), MaxResponseBytes)
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// neverSent 判断一次失败是否**只可能发生在请求送出之前**。
//
// 判据是「失败发生在 dial 阶段」：建连接、DNS 解析、TLS 握手都在这一个阶段里，
// 它们失败时一个字节的 HTTP 报文都还没写出去。反过来，一旦进了 roundTrip 的写/读阶段，
// 我们就不再有能力区分「没送到」与「送到了但回包丢了」——那时一律当成不确定。
func neverSent(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

// cause 剥掉 url.Error 那一层。它包着的是真正的失败原因，而它自己的 Error() 会把
// 完整 URL 抄进去。
func cause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// redactURL 去掉 URL 里的查询串，只留 方案://主机/路径。
//
// 查询串被整个抹掉而不是逐键判断：「哪些键是凭据」这个问题每一家渠道的答案都不同，
// 而抹错的代价是一把令牌进日志。路径上不会有凭据——凭据要么在请求体里（签名），
// 要么在请求头里（微信的 Authorization），两者都不进这里。
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		// 连解析都失败：整串都不打印，我们看不出哪一截是秘密。
		return "<unparsable url>"
	}
	if parsed.RawQuery == "" {
		return parsed.Scheme + "://" + parsed.Host + parsed.Path
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.Path + urlSecretPlaceholder
}

// withoutURL 把错误串里出现的 URL 抹掉，用于「请求根本没构造出来」那条路径。
//
// 那一步的错误恰恰是「这个 URL 我不认识」，它几乎总会把 URL 原样带在错误里。
func withoutURL(err error) string {
	return redactURLsIn(err.Error())
}

// urlOpeners / urlClosers 是自由文本里会给一个 URL 划边界的字符。
//
// **开头的那个字符同时是结尾**，这一点是这个函数唯一不显然的地方：见 redactURLsIn。
const (
	urlOpeners = " \t\"'("
	urlClosers = " \t\"')"
)

// redactURLsIn 把一段文本里形如 scheme://... 的片段截到主机名。
//
// 只做一刀切：找到 "://" 就往前找一个开头的分隔符、往后找一个结尾的，把中间那段交给
// redactURL。不追求解析正确——它守的是「万一带上了，别带查询串」，而不是「精确还原 URL」。
//
// 结尾的分隔符**优先与开头的那个配对**（引号配引号、左括号配右括号）。这不是讲究：这个函数
// 只在 URL **本身没构造出来**时被调用，而「构造不出来」的典型原因恰恰是它畸形到解析不了
// （主机名里带了个空格之类）。这时按「扫到第一个空白就收尾」会把那段 URL 拦腰截断，剩下的
// 半截连同查询串原样漏进日志——正好是它要防的那件事。
func redactURLsIn(message string) string {
	var builder strings.Builder
	rest := message
	for {
		index := strings.Index(rest, "://")
		if index < 0 {
			builder.WriteString(rest)
			return builder.String()
		}
		start, closer := 0, ""
		if open := strings.LastIndexAny(rest[:index], urlOpeners); open >= 0 {
			start = open + 1
			closer = string(rest[open])
		}
		end := -1
		if closer != "" {
			if offset := strings.Index(rest[start:], closer); offset > 0 {
				end = start + offset
			}
		}
		if end < 0 {
			if offset := strings.IndexAny(rest[start:], urlClosers); offset >= 0 {
				end = start + offset
			} else {
				end = len(rest)
			}
		}
		builder.WriteString(rest[:start])
		builder.WriteString(redactURL(rest[start:end]))
		rest = rest[end:]
	}
}

// sleep 等一会儿，ctx 取消就立刻返回。
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
