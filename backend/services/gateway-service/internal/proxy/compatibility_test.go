package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testConfig() Config {
	return Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: "http://user.test"}
}

func testHandler(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return h
}

func TestCompatibilityRouteMatrix(t *testing.T) {
	prefixes := []struct{ path, host string }{
		{"/v1/admin/merchants", "merchant.test"},
		{"/v1/admin/brands", "merchant.test"},
		{"/v1/admin/stores", "merchant.test"},
		{"/v1/admin/uploads", "merchant.test"},
		{"/v1/admin/merchant-users", "user.test"},
		{"/v1/admin/accounts", "user.test"},
		{"/v1/admin/auth", "user.test"},
		{"/v1/admin/users", "user.test"},
		{"/v1/admin/roles", "user.test"},
		{"/v1/admin/permissions", "user.test"},
		{"/v1/admin/menus", "user.test"},
		{"/v1/merchant/auth", "user.test"},
		{"/v1/merchant/users", "user.test"},
	}
	for _, api := range []string{"", "/api"} {
		for _, prefix := range prefixes {
			for _, suffix := range []string{"", "/", "/123", "/123/permissions", "-extra", "extra"} {
				path := api + prefix.path + suffix
				wantHost := prefix.host
				if suffix == "-extra" || suffix == "extra" {
					wantHost = ""
				}
				t.Run(path, func(t *testing.T) { assertRoute(t, path, wantHost) })
			}
		}
		for _, tc := range []struct{ path, host string }{
			{"/v1/admin/merchants/123/users", "user.test"},
			{"/v1/admin/merchants/123/users/", "merchant.test"},
			{"/v1/admin/merchants/123/users/456", "merchant.test"},
			{"/v1/admin/merchants/123/users-extra", "merchant.test"},
			{"/v1/admin/merchants/123/user", "merchant.test"},
			{"/v1/admin/merchants//users", "merchant.test"},
			{"/v1/admin/merchants/users", "merchant.test"},
			{"/v1/admin/merchants/123/456/users", "merchant.test"},
			{"/v1/admin/merchants-extra/123/users", ""},
			// 设备域的上游是可选配置。testConfig() 不填它，所以这一组锁的是「没配
			// 就保持 404」——既有部署里没这个服务，不能因此把请求转到空地址上。
			// 前缀按路径段比较：coffee-machines-extra 与 coffee-machines 不是同一个
			// 前缀，配了上游也不该被它接走（配时的那一面在 proxy_test.go 里测）。
			{"/v1/admin/coffee-machines/devices", ""},
			{"/v1/admin/coffee-machines/devices/123/balance", ""},
			{"/v1/admin/coffee-machines/manufacturers/123/status", ""},
			{"/v1/admin/coffee-machines-extra/devices", ""},
			// 资产账户域（福卡 + 咖啡豆）同理：testConfig() 不填 ACCOUNT_SERVICE_URL，
			// 所以这几条锁的是「没配就保持 404」。小程序那两条尤其重要——它们没配时
			// 必须在这里 404，而不是落到 /v1/miniapp 被转给 user-service（那样客户端
			// 会以为用户服务没有这个接口）。配了上游时的两面带在 proxy_test.go 里测。
			{"/v1/admin/fortune-cards/entries", ""},
			{"/v1/miniapp/fortune-cards", ""},
			{"/v1/admin/coffee-beans/entries", ""},
			{"/v1/admin/coffee-beans/0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e/adjustments", ""},
			{"/v1/miniapp/coffee-beans", ""},
			{"/v1/unknown", ""},
			{"/v1/admin", ""},
			{"//v1/admin/users", ""},
			{"/v1/merchant/roles", ""},
			{"/v1/internal/auth/authorize", ""},
		} {
			t.Run(api+tc.path, func(t *testing.T) { assertRoute(t, api+tc.path, tc.host) })
		}
	}
	for _, path := range []string{"/", "/api", "/api/", "/api/api/v1/admin/users", "/apix/v1/admin/users", "/api-v1/admin/users"} {
		t.Run(path, func(t *testing.T) { assertRoute(t, path, "") })
	}
}

func assertRoute(t *testing.T, path, wantHost string) {
	t.Helper()
	cfg := testConfig()
	calls := 0
	cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != wantHost || r.URL.Path != normalizePath(path) {
			t.Errorf("upstream URL = %s, want host %q, path %q", r.URL, wantHost, normalizePath(path))
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	res := httptest.NewRecorder()
	testHandler(t, cfg).ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
	if wantHost == "" {
		if res.Code != http.StatusNotFound || calls != 0 {
			t.Fatalf("status/calls = %d/%d, want 404/0", res.Code, calls)
		}
	} else if res.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("status/calls = %d/%d, want 204/1 (no gateway IAM calls)", res.Code, calls)
	}
}

func TestCompatibilityStripsSpoofedHeaders(t *testing.T) {
	// 走查三条代表性路由：两条去 merchant、一条去 user。网关自己不再注入
	// X-Service-Token，所以下游任何时候都不该看见它——既不能是客户端塞进来的，
	// 也不能是网关补的。
	for _, path := range []string{
		"/api/v1/admin/brands",
		"/api/v1/admin/roles",
		"/api/v1/admin/merchants/123/users",
	} {
		t.Run(path, func(t *testing.T) {
			cfg := testConfig()
			cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				for key := range r.Header {
					name := strings.ToLower(key)
					if strings.HasPrefix(name, "x-user") || strings.HasPrefix(name, "x-tenant") || name == "x-roles" {
						t.Errorf("spoofed header survived: %s", key)
					}
				}
				if got := r.Header.Values("X-Service-Token"); len(got) != 0 {
					t.Errorf("service token reached the upstream: %q", got)
				}
				if r.Header.Get("Authorization") != "Bearer opaque" || r.Header.Get("X-Request-ID") != "request-123" {
					t.Error("ordinary headers not preserved")
				}
				if strings.Contains(r.Header.Get("X-Forwarded-For"), "attacker") || r.Header.Get("X-Forwarded-Host") == "attacker" {
					t.Error("spoofed forwarded headers survived")
				}
				return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
			})}
			r := httptest.NewRequest(http.MethodGet, path, nil)
			for _, key := range []string{"X-User", "X-User-ID", "X-User-Name", "X-Users", "X-UserRole", "X-Tenant", "X-Tenant-ID", "X-Tenant-Context", "X-Tenants", "X-Roles", "X-Service-Token", "x-user-custom", "x-tenant-custom", "x-roles", "x-service-token"} {
				r.Header[key] = []string{"attacker", "admin"}
			}
			r.Header.Set("Authorization", "Bearer opaque")
			r.Header.Set("X-Request-ID", "request-123")
			r.Header.Set("X-Forwarded-For", "attacker")
			r.Header.Set("X-Forwarded-Host", "attacker")
			res := httptest.NewRecorder()
			testHandler(t, cfg).ServeHTTP(res, r)
			if res.Code != http.StatusNoContent {
				t.Fatalf("status = %d", res.Code)
			}
		})
	}
}

func TestCompatibilityPreservesRequestAndResponse(t *testing.T) {
	const query = "q=a%2Fb&q=two+words&empty=&bad=%zz&semi=a;b"
	const body = "{\"name\":\"咖啡\"}\n"
	for _, prefix := range []string{"", "/api"} {
		for _, root := range []string{"", "/"} {
			t.Run(prefix+"_root="+root, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.RequestURI != "/v1/admin/brands/a%2fb%20c?"+query || r.URL.RawPath != "/v1/admin/brands/a%2fb%20c" {
						t.Errorf("URI/RawPath = %q/%q", r.RequestURI, r.URL.RawPath)
					}
					data, err := io.ReadAll(r.Body)
					if err != nil || string(data) != body || r.Method != http.MethodPatch || r.Header.Get("Authorization") != "Bearer opaque" {
						t.Errorf("request not preserved: method=%s body=%q err=%v", r.Method, data, err)
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Upstream", "retained")
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = io.WriteString(w, `{"success":false,"errorCode":"UPSTREAM_ERROR","errorMessage":"unchanged"}`)
				}))
				defer upstream.Close()
				cfg := testConfig()
				cfg.MerchantServiceURL = upstream.URL + root
				r := httptest.NewRequest(http.MethodPatch, prefix+"/v1/admin/brands/a%2fb%20c?"+query, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer opaque")
				res := httptest.NewRecorder()
				testHandler(t, cfg).ServeHTTP(res, r)
				if res.Code != http.StatusUnprocessableEntity || res.Header().Get("X-Upstream") != "retained" || res.Body.String() != `{"success":false,"errorCode":"UPSTREAM_ERROR","errorMessage":"unchanged"}` {
					t.Fatalf("response not preserved: %d %v %s", res.Code, res.Header(), res.Body)
				}
			})
		}
	}
}

func TestCompatibilityValidatesConfiguration(t *testing.T) {
	for _, field := range []string{"merchant", "user"} {
		for _, value := range []string{"", "/relative", "//host", "ftp://host", "http://", "http://user:pass@host", "http://host?q=1", "http://host?", "http://host#fragment", "http://host#", "http://host/base", "http://host/base/", "http://host/%62ase", "http://host/%zz", "http://host:invalid"} {
			t.Run(field+"="+value, func(t *testing.T) {
				cfg := testConfig()
				if field == "merchant" {
					cfg.MerchantServiceURL = value
				} else {
					cfg.UserServiceURL = value
				}
				if _, err := NewHandler(cfg); err == nil || !strings.Contains(err.Error(), field+" service URL") {
					t.Fatalf("NewHandler() error = %v, want %s URL error", err, field)
				}
			})
		}
	}
	for _, value := range []string{"http://localhost", "https://host/", "http://127.0.0.1:8080", "http://[::1]:8080/"} {
		t.Run(value, func(t *testing.T) {
			cfg := testConfig()
			cfg.MerchantServiceURL, cfg.UserServiceURL = value, value
			testHandler(t, cfg)
		})
	}
	cfg := testConfig()
	cfg.RequestTimeout = -time.Nanosecond
	if _, err := NewHandler(cfg); err == nil {
		t.Error("accepted negative timeout")
	}
}

func TestCompatibilityGatewayErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unavailable", errors.New("dial failed: private infrastructure detail"), 502, "BAD_GATEWAY"},
		{"deadline", context.DeadlineExceeded, 504, "GATEWAY_TIMEOUT"},
		{"network timeout", &timeoutError{}, 504, "GATEWAY_TIMEOUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tc.err })}
			res := httptest.NewRecorder()
			testHandler(t, cfg).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil))
			assertGatewayError(t, res, tc.status, tc.code)
		})
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "network timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func assertGatewayError(t *testing.T, res *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	want := map[string]any{"success": false, "errorCode": code, "errorMessage": http.StatusText(status)}
	var got map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v; body=%s", err, res.Body)
	}
	if res.Code != status || res.Header().Get("Content-Type") != "application/json" || !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %d %v %v, want %d %v", res.Code, res.Header(), got, status, want)
	}
}

func TestCompatibilityTimeoutBeforeHeaders(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.MerchantServiceURL = upstream.URL
	cfg.RequestTimeout = 100 * time.Millisecond
	res := httptest.NewRecorder()
	testHandler(t, cfg).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/admin/brands", nil))
	assertGatewayError(t, res, http.StatusGatewayTimeout, "GATEWAY_TIMEOUT")
	waitSignal(t, canceled, "upstream cancellation")
}

// 上传路径走 UploadTimeout，其余路由仍走 RequestTimeout。两个预算故意设成一长
// 一短，上游固定睡 1 秒：任何「一个超时打天下」的实现都会让其中一条断言失败
// （都用 3s → 品牌不该只等到 200ms；都用 200ms → 上传不该活过 1s 的上游）。
func TestCompatibilityUploadPathHasItsOwnTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.MerchantServiceURL = upstream.URL
	cfg.RequestTimeout = 200 * time.Millisecond
	cfg.UploadTimeout = 3 * time.Second
	gateway := httptest.NewServer(testHandler(t, cfg))
	defer gateway.Close()
	client := &http.Client{Timeout: 5 * time.Second}

	res, err := client.Post(gateway.URL+"/v1/admin/uploads/images", "application/octet-stream", strings.NewReader("bytes"))
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("upload = %d %q, error = %v; want 200 %q", res.StatusCode, body, err, "ok")
	}

	// 同一个上游、同一个网关，只因为路径前缀不同就必须撞上短预算。
	brandRes, err := client.Get(gateway.URL + "/v1/admin/brands")
	if err != nil {
		t.Fatalf("brand request: %v", err)
	}
	defer brandRes.Body.Close()
	if brandRes.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("brand status = %d, want %d", brandRes.StatusCode, http.StatusGatewayTimeout)
	}
}

func TestCompatibilityClientCancellation(t *testing.T) {
	started, canceled, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.UserServiceURL = upstream.URL
	h := testHandler(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/users", nil).WithContext(ctx)
	go func() { defer close(finished); h.ServeHTTP(httptest.NewRecorder(), r) }()
	waitSignal(t, started, "upstream request")
	cancel()
	waitSignal(t, canceled, "upstream client cancellation")
	waitSignal(t, finished, "gateway completion")
}

func TestCompatibilityStreamsUntilBodyCompletes(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "second\n")
		case <-r.Context().Done():
			t.Error("upstream canceled before body completed")
		}
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.UserServiceURL = upstream.URL
	cfg.RequestTimeout = 3 * time.Second
	gateway := httptest.NewServer(testHandler(t, cfg))
	defer gateway.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get(gateway.URL + "/api/v1/admin/users")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	first, err := reader.ReadString('\n')
	close(release)
	if err != nil || first != "first\n" {
		t.Fatalf("first chunk = %q, error = %v", first, err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "second\n" {
		t.Fatalf("remaining body = %q, error = %v", rest, err)
	}
}

func TestCompatibilityTimeoutDuringResponseBody(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.MerchantServiceURL = upstream.URL
	cfg.RequestTimeout = 200 * time.Millisecond
	gateway := httptest.NewServer(testHandler(t, cfg))
	defer gateway.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get(gateway.URL + "/v1/admin/brands")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(data) != "first\n" || err == nil {
		t.Fatalf("stream = %d %q, error = %v; want initial 200 then truncated body", res.StatusCode, data, err)
	}
	waitSignal(t, canceled, "body deadline cancellation")
}

func waitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
