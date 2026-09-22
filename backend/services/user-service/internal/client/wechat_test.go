package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// wechatTestSecret 是一枚形状正确、内容可检索的 appsecret。测试全部围绕
// 「它绝不出现在错误串里」展开。
const wechatTestSecret = "0123456789abcdef0123456789abcdef"

// failingTransport 复现 net/http 在传输层失败时返回的东西：一个 *url.Error，
// 它的 Error() 里带**完整请求 URL**——而微信的 appsecret 只能走查询串传。
// 这正是「密钥经错误串落库」那条链路的起点。
type failingTransport struct {
	err error
}

func (f failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: f.err}
}

func newFailingWechatClient(transportErr error) *WechatMiniappClient {
	c := NewWechatMiniappClient(WechatMiniappConfig{
		AppID: "wx-test-appid", Secret: wechatTestSecret, BaseURL: "https://api.weixin.test",
	})
	c.http = &http.Client{Transport: failingTransport{err: transportErr}}
	return c
}

// assertWechatErrorIsClean 断言错误串里没有任何能还原出请求的信息。
// secret= 与密钥原文是硬要求；完整 URL、主机名与接口路径一并查，因为
// 「密钥在哪条请求上」和「密钥是什么」一样，都不该出现在审计表里。
func assertWechatErrorIsClean(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	for _, leak := range []string{wechatTestSecret, "secret=", "appsecret", "api.weixin.test", "/sns/jscode2session", "/cgi-bin/stable_token", "https://"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("错误串泄漏了 %q: %s", leak, msg)
		}
	}
	if !errors.Is(err, service.ErrWechatUnavailable) {
		t.Fatalf("错误链里应当仍有 ErrWechatUnavailable: %v", err)
	}
}

func TestCode2SessionTransportFailureNeverLeaksSecret(t *testing.T) {
	// 先证明夹具确实复现了泄漏：*url.Error 的 Error() 带完整 URL，
	// 而 URL 上挂着 secret。没有这一步，下面的断言可能是空过的。
	raw := &url.Error{
		Op:  "Get",
		URL: "https://api.weixin.test/sns/jscode2session?appid=wx-test-appid&secret=" + wechatTestSecret,
		Err: errors.New("dial tcp: connect: connection refused"),
	}
	if !strings.Contains(raw.Error(), wechatTestSecret) {
		t.Fatalf("夹具没有复现泄漏，用例失去意义: %s", raw.Error())
	}

	// 连接被拒是真实部署里最常见的一种失败，它同样带完整 URL。
	c := newFailingWechatClient(errors.New("dial tcp 127.0.0.1:443: connect: connection refused"))

	_, _, err := c.Code2Session(context.Background(), "js-code")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	assertWechatErrorIsClean(t, err)
	if !strings.Contains(err.Error(), "wechat connection failed") {
		t.Fatalf("应当保留可判定的失败类别: %s", err.Error())
	}
}

func TestCode2SessionTimeoutKeepsDeadlineOnUnwrapChain(t *testing.T) {
	c := newFailingWechatClient(context.DeadlineExceeded)

	_, _, err := c.Code2Session(context.Background(), "js-code")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	assertWechatErrorIsClean(t, err)
	// Error() 干净不等于把原因丢掉：超时/取消这两类仍要能判出来，
	// 否则调用方只能看到一个笼统的「不可用」。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时应当仍在 Unwrap 链上: %v", err)
	}
}

func TestCode2SessionTimeoutIsRecognizedFromNetError(t *testing.T) {
	// 超时不一定以 context 错误的形式出现：net.Error.Timeout() 是另一种，
	// 断言的正是它也被归到同一句话上。
	c := newFailingWechatClient(&net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}})

	_, _, err := c.Code2Session(context.Background(), "js-code")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	assertWechatErrorIsClean(t, err)
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("超时应当被识别: %s", err.Error())
	}
}

func TestResolvePhoneTransportFailureNeverLeaksSecret(t *testing.T) {
	// 手机号这条路先取 stable_token，请求体里带着同一个 secret，
	// 所以两台请求失败都得干净。
	c := newFailingWechatClient(context.Canceled)

	_, err := c.ResolvePhone(context.Background(), "phone-code")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	assertWechatErrorIsClean(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应当仍在 Unwrap 链上: %v", err)
	}
}

// 构造请求这一步也会失败（基址配错），而它的错误同样是 *url.Error，
// 同样带完整 URL。
func TestRequestBuildFailureNeverLeaksSecret(t *testing.T) {
	c := NewWechatMiniappClient(WechatMiniappConfig{
		AppID: "wx-test-appid", Secret: wechatTestSecret, BaseURL: "https://[::1",
	})

	_, _, err := c.Code2Session(context.Background(), "js-code")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	assertWechatErrorIsClean(t, err)
}

// 业务失败（HTTP 非 200 / errcode 非零）的文案本来就是给日志看的，
// 但不能因为改传输层错误处理而把 secret 加进去。
func TestBusinessFailuresStayClean(t *testing.T) {
	c := NewWechatMiniappClient(WechatMiniappConfig{AppID: "a", Secret: wechatTestSecret, BaseURL: "https://api.weixin.test"})
	c.http = &http.Client{Transport: statusTransport{status: http.StatusInternalServerError}}

	_, _, err := c.Code2Session(context.Background(), "js-code")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	assertWechatErrorIsClean(t, err)
}

type statusTransport struct {
	status int
}

func (s statusTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: s.status,
		Body:       http.NoBody,
		Header:     make(http.Header),
	}, nil
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
