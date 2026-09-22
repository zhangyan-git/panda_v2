package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// DefaultWechatAPIBase 是微信开放接口的默认基址。它可配而不是写死，
// 是为了让测试指向 httptest，也让私有化/代理部署有地方改。
const DefaultWechatAPIBase = "https://api.weixin.qq.com"

// tokenRefreshSlack 是 access_token 的提前刷新窗口。微信给的 expires_in 是
// 7200 秒，留 5 分钟余量是为了避开「取到时还剩 3 秒、路上用掉 4 秒」这种
// 边界——那种失败只会在流量低谷偶发，最难查。
const tokenRefreshSlack = 5 * time.Minute

// WechatMiniappConfig 是小程序凭据与传输参数。
type WechatMiniappConfig struct {
	AppID   string
	Secret  string
	BaseURL string
	Timeout time.Duration
}

// WechatMiniappClient 调用小程序相关的微信开放接口。
//
// 没有 appid/secret 时构造仍然成功，调用时才返回 service.ErrWechatUnavailable：
// 让服务能带着一个未配置的微信客户端启动，是因为后台和商户端的功能不依赖它，
// 因为少一个环境变量就把整个 user-service 拦在启动之外是过分的。
type WechatMiniappClient struct {
	appID, secret, baseURL string
	http                   *http.Client

	// accessToken 缓存在进程内存里，每个副本各存各的。这对稳定版令牌是安全的：
	// /cgi-bin/stable_token 反复请求拿到的是同一枚，不会像旧的 /cgi-bin/token
	// 那样后一次调用把前一次的令牌作废——多副本各取各的，不会互相踢掉对方的
	// 令牌。若换成旧接口，这里就变成一个必须共享的缓存，得另建一张表。
	mu                sync.Mutex
	accessToken       string
	accessTokenExpiry time.Time
}

// NewWechatMiniappClient 构造微信客户端。凭据为空是允许的，见类型注释。
func NewWechatMiniappClient(cfg WechatMiniappConfig) *WechatMiniappClient {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultWechatAPIBase
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &WechatMiniappClient{
		appID:   strings.TrimSpace(cfg.AppID),
		secret:  strings.TrimSpace(cfg.Secret),
		baseURL: strings.TrimRight(base, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Configured 报告凭据是否齐备。调用方在启动时用它决定是否打印一条警告，
// 而不是用它来决定是否启动。
func (c *WechatMiniappClient) Configured() bool {
	return c.appID != "" && c.secret != ""
}

type code2SessionResponse struct {
	OpenID     string `json:"openid"`
	SessionKey string `json:"session_key"`
	UnionID    string `json:"unionid"`
	ErrCode    int    `json:"errcode"`
	ErrMsg     string `json:"errmsg"`
}

// Code2Session 见 service.WechatGateway。
func (c *WechatMiniappClient) Code2Session(ctx context.Context, code string) (string, string, error) {
	if !c.Configured() {
		return "", "", service.ErrWechatUnavailable
	}
	if strings.TrimSpace(code) == "" {
		return "", "", fmt.Errorf("%w: empty js_code", service.ErrWechatAuthFailed)
	}
	// secret 走查询串（微信只支持这种传法），所以这里绝不能把完整 URL 打进
	// 日志或错误里——那会把 appsecret 写进日志和链路追踪。下面的错误只带
	// 状态码和 errcode。
	q := url.Values{}
	q.Set("appid", c.appID)
	q.Set("secret", c.secret)
	q.Set("js_code", code)
	q.Set("grant_type", "authorization_code")

	var resp code2SessionResponse
	if err := c.getJSON(ctx, "/sns/jscode2session?"+q.Encode(), &resp); err != nil {
		return "", "", err
	}
	if resp.ErrCode != 0 {
		return "", "", fmt.Errorf("%w: jscode2session errcode=%d errmsg=%s", service.ErrWechatAuthFailed, resp.ErrCode, resp.ErrMsg)
	}
	// errcode 为 0 但 openid 为空是没见过的形态，但一旦发生，签发的 token 会挂在
	// 一个空标识上——所有这类登录会指向同一个身份。宁可在这里失败。
	if resp.OpenID == "" {
		return "", "", fmt.Errorf("%w: jscode2session returned an empty openid", service.ErrWechatUnavailable)
	}
	return resp.OpenID, resp.UnionID, nil
}

type stableTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	ErrCode     int    `json:"errcode"`
	ErrMsg      string `json:"errmsg"`
}

type phoneNumberResponse struct {
	ErrCode   int    `json:"errcode"`
	ErrMsg    string `json:"errmsg"`
	PhoneInfo struct {
		PhoneNumber     string `json:"phoneNumber"`
		PurePhoneNumber string `json:"purePhoneNumber"`
		CountryCode     string `json:"countryCode"`
	} `json:"phone_info"`
}

// ResolvePhone 见 service.WechatGateway。
func (c *WechatMiniappClient) ResolvePhone(ctx context.Context, code string) (string, error) {
	if !c.Configured() {
		return "", service.ErrWechatUnavailable
	}
	if strings.TrimSpace(code) == "" {
		return "", fmt.Errorf("%w: empty phone code", service.ErrWechatAuthFailed)
	}
	token, err := c.accessTokenValue(ctx)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", err
	}
	var resp phoneNumberResponse
	if err := c.postJSON(ctx, "/wxa/business/getuserphonenumber?access_token="+url.QueryEscape(token), body, &resp); err != nil {
		return "", err
	}
	if resp.ErrCode != 0 {
		// 40001/40014 是令牌失效。这时把缓存清掉，下一个请求会重新取——
		// 否则一枚提前失效的令牌会把后续所有手机号登录都拖住两个小时。
		if resp.ErrCode == 40001 || resp.ErrCode == 40014 {
			c.invalidateAccessToken()
		}
		return "", fmt.Errorf("%w: getuserphonenumber errcode=%d errmsg=%s", service.ErrWechatAuthFailed, resp.ErrCode, resp.ErrMsg)
	}
	// 优先纯号码：phoneNumber 带国际区号前缀，落库的手机号要和短信登录那条
	// 链路一致，否则同一个人的两个手机号会在唯一索引上不相等。
	phone := resp.PhoneInfo.PurePhoneNumber
	if phone == "" {
		phone = resp.PhoneInfo.PhoneNumber
	}
	if phone == "" {
		return "", fmt.Errorf("%w: getuserphonenumber returned an empty phone", service.ErrWechatUnavailable)
	}
	return phone, nil
}

// accessTokenValue 返回缓存中的 access_token，过期或没有时重新取。
//
// 用 /cgi-bin/stable_token 而不是 /cgi-bin/token：后者每调用一次就作废上一次
// 发的令牌，多副本部署下两个实例会互相把对方的令牌废掉，表现为「登录随机失败」。
// 稳定版令牌反复请求返回同一枚，多副本各缓存各的也不会冲突。
// force_refresh 必须为 false，否则又变回每次都要新的。
func (c *WechatMiniappClient) accessTokenValue(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.accessToken != "" && time.Now().Before(c.accessTokenExpiry) {
		token := c.accessToken
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	body, err := json.Marshal(map[string]any{
		"grant_type":    "client_credential",
		"appid":         c.appID,
		"secret":        c.secret,
		"force_refresh": false,
	})
	if err != nil {
		return "", err
	}
	var resp stableTokenResponse
	if err := c.postJSON(ctx, "/cgi-bin/stable_token", body, &resp); err != nil {
		return "", err
	}
	if resp.ErrCode != 0 {
		// appid/secret 不对会落到这里。这是配置问题，不是用户问题，所以归
		// service.ErrWechatUnavailable；把它报成「授权失败」会让用户反复重试。
		return "", fmt.Errorf("%w: stable_token errcode=%d errmsg=%s", service.ErrWechatUnavailable, resp.ErrCode, resp.ErrMsg)
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("%w: stable_token returned an empty token", service.ErrWechatUnavailable)
	}
	ttl := time.Duration(resp.ExpiresIn) * time.Second
	if ttl <= tokenRefreshSlack {
		ttl = tokenRefreshSlack + time.Minute
	}

	c.mu.Lock()
	c.accessToken = resp.AccessToken
	c.accessTokenExpiry = time.Now().Add(ttl - tokenRefreshSlack)
	c.mu.Unlock()
	return resp.AccessToken, nil
}

func (c *WechatMiniappClient) invalidateAccessToken() {
	c.mu.Lock()
	c.accessToken = ""
	c.accessTokenExpiry = time.Time{}
	c.mu.Unlock()
}

func (c *WechatMiniappClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		// 构造失败的错误同样是 *url.Error，Error() 里带完整 URL（含 secret）。
		return &wechatTransportError{msg: "wechat request could not be created", cause: err}
	}
	return c.do(req, out)
}

func (c *WechatMiniappClient) postJSON(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return &wechatTransportError{msg: "wechat request could not be created", cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

// wechatTransportError 是一次传输层失败的包装。
//
// 它存在的唯一理由是：http.Client.Do 与 http.NewRequest 返回的错误都是 *url.Error，
// 而它的 Error() 里带**完整 URL**。微信的 appsecret 只能走查询串传（见 Code2Session
// 的注释），把底层错误原样拼进来，等于把密钥写进日志、错误串和链路追踪。
//
// 所以 Error() 只有一句固定短语，可供判定的信息全在 Unwrap 链上：errors.Is 命中
// service.ErrWechatUnavailable（调用方的分支依据）以及 context.DeadlineExceeded /
// context.Canceled，errors.As 能取到 *net.OpError。两样都要，所以这里返回两个错误
// （Go 1.20 的多重 Unwrap），而不是在「保 Unwrap」和「保干净」之间二选一。
type wechatTransportError struct {
	msg   string
	cause error
}

func (e *wechatTransportError) Error() string { return e.msg }

func (e *wechatTransportError) Unwrap() []error {
	if e.cause == nil {
		return []error{service.ErrWechatUnavailable}
	}
	return []error{service.ErrWechatUnavailable, e.cause}
}

// transportFailure 把一次网络层失败翻成固定短句，只保留「哪一类失败」这个可判定
// 的信息，不把底层错误的文本带出来——理由见 wechatTransportError。
func transportFailure(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &wechatTransportError{msg: "wechat request timed out", cause: err}
	case errors.Is(err, context.Canceled):
		return &wechatTransportError{msg: "wechat request canceled", cause: err}
	}
	// *url.Error 自身也实现 net.Error，它的 Timeout() 会问到里面那层。
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return &wechatTransportError{msg: "wechat request timed out", cause: err}
		}
		return &wechatTransportError{msg: "wechat connection failed", cause: err}
	}
	return &wechatTransportError{msg: "wechat request failed", cause: err}
}

// do 发请求并解 JSON。
//
// 微信的失败几乎都是 HTTP 200 + 非零 errcode，所以这里不能只看状态码；
// 反过来，状态码非 200 时响应体也不是 JSON，不能拿去解析。
//
// 传输层失败一律经 transportFailure 翻译：底层 *url.Error 带着完整 URL，
// 而 URL 上挂着 appsecret（见 Code2Session 的注释）。
func (c *WechatMiniappClient) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return transportFailure(err)
	}
	defer resp.Body.Close()
	// 限长：这些响应都是几百字节，读到 1 MiB 还没结束说明对面不是微信。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", service.ErrWechatUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: wechat returned http %d", service.ErrWechatUnavailable, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %v", service.ErrWechatUnavailable, err)
	}
	return nil
}
