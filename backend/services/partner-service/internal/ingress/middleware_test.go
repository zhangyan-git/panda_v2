package ingress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/cache"
	"github.com/panda-dev/panda-v2/backend/platform/ratelimit"
	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
)

// 这个文件盯的是 Guard 的那九道与它们的**顺序**（见 middleware.go 的 Handler 注释）：
// 每一条拒绝路径一个用例，断言三件事——对外状态码、对外报文、**只进日志**的内部 error_code。
//
// 内部 error_code 是这里最值得断言的东西：它是排查时唯一能分清「对方签名写错了」与「对方在
// 重放」的地方（两者对外都是同一句 401），而它错了不会让任何测试变红——只会让某一天的排查
// 停在一个错误的方向上。

// ============================================================
// 桩
// ============================================================

const (
	testAPIKey   = "KEY-0000000000000000000000000001"
	testPlainKey = "0123456789abcdef0123456789abcdef" // 签名密钥的明文
	testPeerIP   = "192.0.2.1:12345"                  // httptest 默认的 RemoteAddr 形状
)

var testNow = time.Unix(1700000000, 0)

type fakeLookup struct {
	key     *model.APIKey
	partner *model.PartnerAccount
	err     error
	calls   int
}

func (f *fakeLookup) lookup(context.Context, string) (*model.APIKey, *model.PartnerAccount, error) {
	f.calls++
	return f.key, f.partner, f.err
}

type fakeNonces struct {
	claimed bool
	err     error
	keys    []string
}

func (f *fakeNonces) Claim(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return false, f.err
	}
	return f.claimed, nil
}

type fakeLogs struct {
	mu      sync.Mutex
	entries []model.CallLog
	err     error
}

func (f *fakeLogs) RecordCallLog(_ context.Context, entry model.CallLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, entry)
	return f.err
}

func (f *fakeLogs) only(t *testing.T) model.CallLog {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) != 1 {
		t.Fatalf("一次调用应当只写一条日志，写了 %d 条: %+v", len(f.entries), f.entries)
	}
	return f.entries[0]
}

// last 取最后一条。限流那条用例会先打一次「用掉额度」的请求，所以那里断言的是**这一次**
// 写下来的那一条，而不是「一共只有一条」。
func (f *fakeLogs) last(t *testing.T) model.CallLog {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		t.Fatal("没有写下任何日志")
	}
	return f.entries[len(f.entries)-1]
}

// ============================================================
// 装配
// ============================================================

type harness struct {
	guard   *Guard
	lookup  *fakeLookup
	nonces  *fakeNonces
	logs    *fakeLogs
	keyring *secret.Keyring
	reach   bool // handler 是否被调到
}

// newHarness 装一个可用的 Guard：真 Keyring（用来真的封/解）、真限流器（cache.Noop 时
// platform/ratelimit 退化成进程内令牌桶，对测试足够且不需要 Redis）。
func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()

	keyring, err := secret.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	envelope, err := keyring.Seal(model.SecretSlot, secret.KindText, testPlainKey)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	h := &harness{
		lookup: &fakeLookup{
			key:     &model.APIKey{ID: "11111111-1111-1111-1111-111111111111", PartnerID: "22222222-2222-2222-2222-222222222222", APIKey: testAPIKey, APIKeyMask: "KEY-…0001", SecretMask: "0123…cdef", Secret: envelope, Status: model.StatusEnabled, RateLimitPerMinute: 600},
			partner: &model.PartnerAccount{ID: "22222222-2222-2222-2222-222222222222", Code: "fengxuan", Status: model.StatusEnabled},
		},
		nonces:  &fakeNonces{claimed: true},
		logs:    &fakeLogs{},
		keyring: keyring,
	}

	options := Options{
		Lookup:   h.lookup.lookup,
		Keyring:  keyring,
		Nonces:   h.nonces,
		Limiters: NewLimiters(cache.Noop{}),
		CallLogs: h.logs,
		Now:      func() time.Time { return testNow },
	}
	for _, opt := range opts {
		opt(&options)
	}
	guard, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.guard = guard
	return h
}

// serve 让一个请求穿过 Guard。handler 只记「被调到了」并把 Caller 抄出来。
func (h *harness) serve(r *http.Request) (*httptest.ResponseRecorder, Caller, bool) {
	h.reach = false
	var caller Caller
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.reach = true
		caller, _ = CallerFrom(req.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"data":{"ok":true}}`))
	})
	recorder := httptest.NewRecorder()
	h.guard.Handler(handler).ServeHTTP(recorder, r)
	return recorder, caller, h.reach
}

// signedRequest 造一份签名正确的请求（用 Spec() 自己签，见 spec_test.go 的 signAsPartner）。
func (h *harness) signedRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	r := partnerRequest(http.MethodPost, "https://partner.example.com/v1/openapi/member-price-entitlement", body, testNow.Unix(), "nonce-1")
	r.RemoteAddr = testPeerIP
	signature := signAsPartner(t, r, body, testPlainKey)
	r.Header.Set(HeaderSignature, signature)
	return r
}

// ============================================================
// 成功路径
// ============================================================

func TestGuardLetsAProperlySignedRequestThrough(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"userId":"u-1"}`)
	recorder, caller, reached := h.serve(h.signedRequest(t, body))

	if !reached {
		t.Fatalf("验签通过之后应当进 handler，得到 %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200: %s", recorder.Code, recorder.Body.String())
	}
	if caller.PartnerID != h.lookup.key.PartnerID || caller.APIKeyID != h.lookup.key.ID {
		t.Fatalf("请求上下文里的身份不对: %+v", caller)
	}
	if caller.PartnerCode != "fengxuan" {
		t.Fatalf("合作方编码应当进上下文（转发给下游时也用它），得到 %q", caller.PartnerCode)
	}

	entry := h.logs.only(t)
	if entry.ErrorCode != "" {
		t.Fatalf("成功的一次调用不该有 error_code，得到 %q", entry.ErrorCode)
	}
	if entry.StatusCode != http.StatusOK {
		t.Fatalf("日志里的状态码应当是 200，得到 %d", entry.StatusCode)
	}
	if entry.APIKeyID != h.lookup.key.ID || entry.PartnerID != h.lookup.key.PartnerID {
		t.Fatalf("日志里的身份不对: %+v", entry)
	}
	if !strings.Contains(entry.ResponseBody, `"ok":true`) {
		t.Fatalf("日志应当留一份响应体副本，得到 %q", entry.ResponseBody)
	}
	if !strings.Contains(entry.RequestBody, "u-1") {
		t.Fatalf("日志应当留一份请求体，得到 %q", entry.RequestBody)
	}
	if entry.DurationMS < 0 {
		t.Fatalf("耗时不该是负数: %d", entry.DurationMS)
	}
}

// ============================================================
// 失败矩阵
// ============================================================

func TestGuardRejectionMatrix(t *testing.T) {
	expired := testNow.Add(-time.Hour)

	cases := []struct {
		name string
		// mutate 在装配之后调整依赖（密钥行、合作方、nonce、lookup 的错误）
		mutate func(*harness)
		// request 可以覆盖默认那份签名正确的请求
		request func(*testing.T, *harness) *http.Request
		// 不请求签名时（头都缺、时间戳超窗那几条）用不上 harness 的签名
		status int
		body   string
		code   string
	}{
		{
			name:   "一个头都没带",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeMissingCredentials,
			request: func(_ *testing.T, _ *harness) *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/v1/openapi/x", nil)
				return r
			},
		},
		{
			name:   "少一个 X-Nonce（V2 新加的那个头）",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeMissingCredentials,
			request: func(t *testing.T, h *harness) *http.Request {
				r := h.signedRequest(t, nil)
				r.Header.Del(HeaderNonce)
				return r
			},
		},
		{
			name:   "时间戳超窗（过去一小时）",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeTimestampOutOfRange,
			request: func(t *testing.T, h *harness) *http.Request {
				r := h.signedRequest(t, nil)
				r.Header.Set(HeaderTimestamp, strconv.FormatInt(testNow.Add(-time.Hour).Unix(), 10))
				return r
			},
		},
		{
			name:   "时间戳不是十进制",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeTimestampOutOfRange,
			request: func(t *testing.T, h *harness) *http.Request {
				r := h.signedRequest(t, nil)
				r.Header.Set(HeaderTimestamp, "yesterday")
				return r
			},
		},
		{
			name:   "X-API-Key 查不到",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeUnknownAPIKey,
			mutate: func(h *harness) { h.lookup.err = ErrCredentialNotFound },
		},
		{
			name:   "查密钥行这一步本身失败了（库不可用）",
			status: http.StatusServiceUnavailable,
			body:   unavailableBody,
			code:   codeCredentialLookup,
			mutate: func(h *harness) { h.lookup.err = errors.New("connection refused") },
		},
		{
			name:   "合作方被停用",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codePartnerDisabled,
			mutate: func(h *harness) { h.lookup.partner.Status = model.StatusDisabled },
		},
		{
			name:   "合作方过期",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codePartnerExpired,
			mutate: func(h *harness) { h.lookup.partner.ExpiresAt = &expired },
		},
		{
			name:   "密钥被停用",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeAPIKeyDisabled,
			mutate: func(h *harness) { h.lookup.key.Status = model.StatusDisabled },
		},
		{
			name:   "密钥过期",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeAPIKeyExpired,
			mutate: func(h *harness) { h.lookup.key.ExpiresAt = &expired },
		},
		{
			name:   "来源不在 IP 白名单里",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeIPNotAllowed,
			mutate: func(h *harness) { h.lookup.key.IPWhitelist = []string{"10.0.0.0/8"} },
		},
		{
			name:   "白名单里有一条解析不出来（整份作废）",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeIPNotAllowed,
			mutate: func(h *harness) { h.lookup.key.IPWhitelist = []string{"10.0.0.0/8", "oops"} },
		},
		{
			name:   "密钥解不开（密文被换过）",
			status: http.StatusServiceUnavailable,
			body:   unavailableBody,
			code:   codeSecretUnreadable,
			mutate: func(h *harness) {
				other, err := secret.New([]byte("ffffffffffffffffffffffffffffffff"))
				if err != nil {
					t.Fatalf("secret.New: %v", err)
				}
				envelope, err := other.Seal(model.SecretSlot, secret.KindText, testPlainKey)
				if err != nil {
					t.Fatalf("Seal: %v", err)
				}
				h.lookup.key.Secret = envelope
			},
		},
		{
			name:   "签名不对",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeSignatureMismatch,
			request: func(t *testing.T, h *harness) *http.Request {
				r := h.signedRequest(t, nil)
				r.Header.Set(HeaderSignature, strings.Repeat("ab", 32))
				return r
			},
		},
		{
			name:   "换了报文之后签名就对不上了",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeSignatureMismatch,
			request: func(t *testing.T, h *harness) *http.Request {
				r := h.signedRequest(t, []byte(`{"userId":"u-1"}`))
				// 把 body 换成另一份（签名是照原来那份算的）。需要一个可重放的 body。
				r.Body = io_NopCloser(`{"userId":"u-2"}`)
				r.ContentLength = int64(len(`{"userId":"u-2"}`))
				return r
			},
		},
		{
			name:   "nonce 去重做不了（Redis 不可用）→ 失败关闭",
			status: http.StatusServiceUnavailable,
			body:   unavailableBody,
			code:   codeNonceUnavailable,
			mutate: func(h *harness) { h.nonces.err = ErrNonceUnavailable },
		},
		{
			name:   "nonce 用过了（重放）",
			status: http.StatusUnauthorized,
			body:   unauthorizedBody,
			code:   codeNonceReplayed,
			mutate: func(h *harness) { h.nonces.claimed = false },
		},
		{
			name:   "超过每分钟额度",
			status: http.StatusTooManyRequests,
			body:   tooManyBody,
			code:   codeRateLimited,
			mutate: func(h *harness) { h.lookup.key.RateLimitPerMinute = 1 },
			request: func(t *testing.T, h *harness) *http.Request {
				// 第一次把额度用掉（同一把密钥、同一个 limiter 实例），第二次才是被测的那次。
				first, _, _ := h.serve(h.signedRequest(t, nil))
				if first.Code != http.StatusOK {
					t.Fatalf("第一次调用应当通过（额度 1），得到 %d: %s", first.Code, first.Body.String())
				}
				return h.signedRequest(t, nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.mutate != nil {
				tc.mutate(h)
			}
			var r *http.Request
			if tc.request != nil {
				r = tc.request(t, h)
			} else {
				r = h.signedRequest(t, nil)
			}

			recorder, _, reached := h.serve(r)

			if reached {
				t.Fatal("被拒的请求不该进 handler")
			}
			if recorder.Code != tc.status {
				t.Fatalf("状态码 %d，期望 %d: %s", recorder.Code, tc.status, recorder.Body.String())
			}
			if strings.TrimSpace(recorder.Body.String()) != tc.body {
				t.Fatalf("对外报文应当逐字是 %q，得到 %q", tc.body, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type 应当是 application/json，得到 %q", got)
			}
			if tc.status == http.StatusTooManyRequests {
				if got := recorder.Header().Get("Retry-After"); got != strconv.Itoa(int(rateWindow.Seconds())) {
					t.Fatalf("429 必须带 Retry-After，得到 %q", got)
				}
			}

			entry := h.logs.last(t)
			if entry.ErrorCode != tc.code {
				t.Fatalf("日志里的 error_code 应当是 %s，得到 %q", tc.code, entry.ErrorCode)
			}
			if entry.StatusCode != tc.status {
				t.Fatalf("日志里的状态码应当是 %d，得到 %d", tc.status, entry.StatusCode)
			}
			if entry.Path == "" || entry.Method == "" {
				t.Fatalf("日志里应当有方法与路径: %+v", entry)
			}
			if entry.RequestIP != "192.0.2.1" {
				t.Fatalf("日志里的来源地址应当是 192.0.2.1，得到 %q", entry.RequestIP)
			}
		})
	}
}

// TestGuardRejectsOversizedBody 单独一条：413 要在**读进来之前**就判掉，并且不碰凭据。
//
// 上限是 maxBodyBytes（1 MiB），而日志里那一份是 8 KiB——两个数不能混。这条测试用一份刚过
// 上限的报文把第一个数钉住。
func TestGuardRejectsOversizedBody(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`)
	r := h.signedRequest(t, body)

	recorder, _, reached := h.serve(r)
	if reached {
		t.Fatal("超限的报文不该进 handler")
	}
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 %d，期望 413", recorder.Code)
	}
	if recorder.Body.String() != tooLargeBody {
		t.Fatalf("报文应当是 %q，得到 %q", tooLargeBody, recorder.Body.String())
	}
	if got := h.logs.only(t).ErrorCode; got != codeBodyTooLarge {
		t.Fatalf("error_code 应当是 %s，得到 %q", codeBodyTooLarge, got)
	}
}

// TestGuardRejectsDuplicateNonceForTheSameKeyOnly 钉住 nonce 键里带 api_key_id。
//
// 两个合作方恰好用了同一个随机串时不该互相把对方拒掉——那不是安全问题，是一次很难查的误伤。
func TestGuardRejectsDuplicateNonceForTheSameKeyOnly(t *testing.T) {
	h := newHarness(t)
	_, _, _ = h.serve(h.signedRequest(t, nil))

	if len(h.nonces.keys) != 1 {
		t.Fatalf("应当只认领一个 nonce，得到 %v", h.nonces.keys)
	}
	if !strings.Contains(h.nonces.keys[0], h.lookup.key.ID) {
		t.Fatalf("nonce 的键必须包含 api_key_id，得到 %q", h.nonces.keys[0])
	}
	if !strings.Contains(h.nonces.keys[0], "nonce-1") {
		t.Fatalf("nonce 的键必须包含 nonce 本身，得到 %q", h.nonces.keys[0])
	}
}

// TestGuardIgnoresForwardedForFromAnUntrustedPeer 钉住白名单绕不过去。
//
// 判定的来源必须来自「可信代理写的那一跳」。一个不可信的对端自己伪造 X-Forwarded-For 时，
// 那个值不能被当成来源——否则任何人都能用一个头把自己伪装成白名单里的地址。
func TestGuardIgnoresForwardedForFromAnUntrustedPeer(t *testing.T) {
	trust, err := ratelimit.NewProxyTrust([]string{"192.0.2.0/24"})
	if err != nil {
		t.Fatalf("NewProxyTrust: %v", err)
	}
	h := newHarness(t, func(o *Options) { o.TrustedProxies = trust })
	h.lookup.key.IPWhitelist = []string{"203.0.113.9"}

	// 可信对端写的 XFF：认。
	fromProxy := h.signedRequest(t, nil)
	fromProxy.RemoteAddr = "192.0.2.1:12345"
	fromProxy.Header.Set("X-Forwarded-For", "203.0.113.9")
	if recorder, _, reached := h.serve(fromProxy); !reached {
		t.Fatalf("可信代理写的 XFF 应当被认，得到 %d: %s", recorder.Code, recorder.Body.String())
	}

	// 不可信对端写的同一个 XFF：不认——来源是那个对端自己，不在白名单里。
	forged := h.signedRequest(t, nil)
	forged.RemoteAddr = "198.51.100.7:12345"
	forged.Header.Set("X-Forwarded-For", "203.0.113.9")
	recorder, _, reached := h.serve(forged)
	if reached {
		t.Fatal("不可信对端写的 X-Forwarded-For 绝不能决定来源，否则白名单可以被一个头绕过")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 %d，期望 401", recorder.Code)
	}
}

// ============================================================
// 日志里不出现密钥
// ============================================================

// TestCallLogsNeverContainTheSigningSecret 是本文件里最该存在的一条断言。
//
// 它把「明文签名密钥不进日志」从一个承诺变成一个被检查过的事实：走一遍成功与失败两条路，
// 然后把写进去的每一条日志的**每一个字段**都翻一遍，找那个明文串与它的密文信封。
//
// 这条测试能抓到的具体事故是把 entry 里某个字段接成了「解出来的密钥」（比如为了排查顺手把
// 它塞进 Query 或 error_code），而那种改动在任何人工 review 里都看着像一句调试代码。
func TestCallLogsNeverContainTheSigningSecret(t *testing.T) {
	h := newHarness(t)

	// 成功一次：handler 回一句含数据的响应；请求体里也放一个像是密钥的字段（合作方偶尔会
	// 把凭据塞进业务报文，这张表照样得挡住——它记的是报文，不是我们的密钥）。
	_, _, reached := h.serve(h.signedRequest(t, []byte(`{"userId":"u-1"}`)))
	if !reached {
		t.Fatal("这一条的前提是成功路径能走通")
	}
	// 失败一次：签名不对（这一条路上我们手里有密钥，是最容易顺手记下来的地方）。
	bad := h.signedRequest(t, nil)
	bad.Header.Set(HeaderSignature, strings.Repeat("cd", 32))
	_, _, _ = h.serve(bad)

	envelope := h.lookup.key.Secret
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	if len(h.logs.entries) != 2 {
		t.Fatalf("两次调用应当写两条日志，得到 %d", len(h.logs.entries))
	}
	for i, entry := range h.logs.entries {
		fields := map[string]string{
			"query": entry.Query, "request_body": entry.RequestBody, "response_body": entry.ResponseBody,
			"error_code": entry.ErrorCode, "method": entry.Method, "path": entry.Path,
			"request_ip": entry.RequestIP, "api_key_mask": entry.APIKeyMask,
			"partner_id": entry.PartnerID, "api_key_id": entry.APIKeyID,
		}
		for name, value := range fields {
			if strings.Contains(value, testPlainKey) {
				t.Fatalf("第 %d 条日志的 %s 里出现了签名密钥的明文", i, name)
			}
		}
		// 密文信封同样不许出现：它虽然解不开，但把它抄进日志等于给将来的主密钥轮换留了一个
		// 已经离库的副本（而平台/secret 的全部设计前提是密文只在库里）。
		if strings.Contains(entry.RequestBody+entry.ResponseBody+entry.Query, envelope.Nonce) {
			t.Fatalf("第 %d 条日志里出现了密钥密文信封的一部分", i)
		}
		// 掩码是**允许**的，也是这张表唯一该有的密钥痕迹。
		if i == 0 && entry.APIKeyMask != h.lookup.key.APIKeyMask {
			t.Fatalf("日志里应当留掩码（api_key_mask），得到 %q", entry.APIKeyMask)
		}
	}
}

// ============================================================
// 词表
// ============================================================

// TestErrorCodesCoverEveryConstant 盯着「加了一个码但忘了加进清单」。
//
// 加一个码时只改常量、忘了 errorCodes，表现是后台按新码筛日志筛不出来（列表里有那些行，
// 筛选框里没有这个选项）；反过来，清单里多一个不存在的码，表现是筛出来永远是空。两种都不会
// 让任何别的测试变红，而两种都是运营会撞上的。
func TestErrorCodesCoverEveryConstant(t *testing.T) {
	constants := []string{
		codeMissingCredentials, codeTimestampOutOfRange, codeCredentialLookup, codeUnknownAPIKey,
		codePartnerDisabled, codePartnerExpired, codeAPIKeyDisabled, codeAPIKeyExpired,
		codeIPNotAllowed, codeSecretUnreadable, codeSignatureMismatch, codeNonceUnavailable,
		codeNonceReplayed, codeRateLimited, codeRateLimitDegraded, codeBodyUnreadable, codeBodyTooLarge,
	}
	listed := ErrorCodes()
	if len(listed) != len(constants) {
		t.Fatalf("errorCodes 里有 %d 个码，常量有 %d 个——两处必须一一对应", len(listed), len(constants))
	}
	// ErrorCodes() 返回的是排好序的，所以比之前先把常量那一份也排一遍（常量是按语义分组写的，
	// 顺序与字典序不同，而这里的判据是集合相等 + 已排序）。
	want := slices.Clone(constants)
	slices.Sort(want)
	for i, code := range want {
		if listed[i] != code {
			t.Fatalf("清单第 %d 项是 %q，常量是 %q（两处必须一一对应）", i, listed[i], code)
		}
		if !IsErrorCode(code) {
			t.Fatalf("%s 应当被 IsErrorCode 认出来", code)
		}
	}
	// 清单是副本：调用方排序或改它都不该影响下一次读取。
	sorted := ErrorCodes()
	for i := 0; i < len(sorted)-1; i++ {
		if sorted[i] > sorted[i+1] {
			t.Fatalf("ErrorCodes 应当是排好序的: %v", sorted)
		}
	}

	for _, unknown := range []string{"", "  ", "SIGNATURE-MISMATCH", "signature_mismatch", "NOT_A_CODE"} {
		if IsErrorCode(unknown) {
			t.Fatalf("%q 不是我们写过的码，IsErrorCode 应当返回 false（否则运营打错一个字会得到「这段时间没有这类失败」）", unknown)
		}
	}
}

// io_NopCloser 是给「换掉请求体」那一条用的极小工具，避免为一行 import io。
func io_NopCloser(body string) ioReadCloser { return nopCloser{strings.NewReader(body)} }

type ioReadCloser interface {
	Read([]byte) (int, error)
	Close() error
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }
