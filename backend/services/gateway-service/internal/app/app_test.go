package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
)

func testAuthorizer(t *testing.T) *auth.Service {
	t.Helper()
	service, err := auth.NewService([]byte("test-secret-test-secret-test-secret"), "gateway-test", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPHandlerForwardsAuthEndpointsAndPassesResponse(t *testing.T) {
	for _, tt := range []struct {
		name     string
		endpoint string
		input    string
		response string
		status   int
	}{
		{name: "login", endpoint: "/v1/auth/login", input: `{"username":"alice","password":"secret","type":"admin"}`, response: `{"access_token":"token"}`, status: http.StatusOK},
		{name: "refresh", endpoint: "/v1/auth/refresh", input: `{"refresh_token":"refresh"}`, response: `{"access_token":"new-token"}`, status: http.StatusOK},
		{name: "revoke", endpoint: "/v1/auth/revoke", input: `{"refresh_token":"refresh"}`, response: `{"revoked":true}`, status: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody []byte
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "http://account.test"+tt.endpoint || req.Method != http.MethodPost {
					t.Fatalf("request = %s %s", req.Method, req.URL)
				}
				gotBody, _ = io.ReadAll(req.Body)
				return &http.Response{StatusCode: tt.status, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Request-ID": []string{"downstream"}}, Body: io.NopCloser(bytes.NewBufferString(tt.response))}, nil
			})}
			request := httptest.NewRequest(http.MethodPost, tt.endpoint, bytes.NewBufferString(tt.input))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			NewHTTPHandler(testAuthorizer(t), WithAccountServiceURL("http://account.test"), WithHTTPClient(client)).ServeHTTP(response, request)

			if response.Code != tt.status || response.Header().Get("X-Request-ID") != "downstream" || response.Body.String() != tt.response {
				t.Fatalf("response = %d %#v %q", response.Code, response.Header(), response.Body.String())
			}
			if string(gotBody) != tt.input {
				t.Fatalf("body = %q, want %q", gotBody, tt.input)
			}
		})
	}
}

func TestHTTPHandlerPassesDownstreamNon2xx(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"error":"invalid credentials"}`))}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{}`))
	response := httptest.NewRecorder()
	NewHTTPHandler(testAuthorizer(t), WithAccountServiceURL("http://account.test"), WithHTTPClient(client)).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Body.String() != `{"error":"invalid credentials"}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}

func TestHTTPHandlerMapsTimeoutAndConnectionErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"timeout", timeoutError{}, http.StatusGatewayTimeout},
		{"connection", errors.New("connection refused"), http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tt.err })}
			response := httptest.NewRecorder()
			NewHTTPHandler(testAuthorizer(t), WithAccountServiceURL("http://account.test"), WithHTTPClient(client)).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{}`)))
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d", response.Code, tt.want)
			}
			var body map[string]string
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil || body["error"] == "" {
				t.Fatalf("body = %q, err = %v", response.Body.String(), err)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestHTTPHandlerLoginRequiresConfiguration(t *testing.T) {
	response := httptest.NewRecorder()
	NewHTTPHandler(testAuthorizer(t)).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestHTTPHandlerProtectsV1Routes(t *testing.T) {
	handler := NewHTTPHandler(testAuthorizer(t))
	for _, authorization := range []string{"", "Bearer invalid"} {
		request := httptest.NewRequest(http.MethodGet, "/v1/orders", nil)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("authorization %q: status = %d, want %d", authorization, response.Code, http.StatusUnauthorized)
		}
	}
}

func TestHTTPHandlerAllowsAuthenticatedV1Routes(t *testing.T) {
	authorizer := testAuthorizer(t)
	token, err := authorizer.SignAccess("account-1", "user-1", "account-1", "tenant-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/orders", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	NewHTTPHandler(authorizer).ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotImplemented)
	}
}

func TestHTTPHandlerProxiesAuthenticatedUserProfile(t *testing.T) {
	authorizer := testAuthorizer(t)
	token, err := authorizer.SignAccess("account-1", "user-1", "account-1", "tenant-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPatch || req.URL.String() != "http://user.test/v1/users/1?verbose=true" {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		if req.Header.Get("X-User-ID") != "" || req.Header.Get("X-Internal-Role") != "" {
			t.Fatal("internal identity header was forwarded")
		}
		body, _ := io.ReadAll(req.Body)
		if string(body) != `{"nickname":"new"}` {
			t.Fatalf("body = %q", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"id":1}`))}, nil
	})}
	req := httptest.NewRequest(http.MethodPatch, "/v1/users/1?verbose=true", bytes.NewBufferString(`{"nickname":"new"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-ID", "spoofed")
	req.Header.Set("X-Internal-Role", "admin")
	response := httptest.NewRecorder()
	NewHTTPHandler(authorizer, WithUserServiceURL("http://user.test"), WithHTTPClient(client)).ServeHTTP(response, req)
	if response.Code != http.StatusOK || response.Body.String() != `{"id":1}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}

func TestHTTPHandlerProxiesAuthenticatedMerchantRoutes(t *testing.T) {
	authorizer := testAuthorizer(t)
	token, _ := authorizer.SignAccess("account-1", "merchant-1", "account-1", "tenant-1", nil)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://merchant.test"+req.URL.Path || req.Header.Get("Authorization") != "Bearer "+token {
			t.Fatalf("request = %s %s authorization=%q", req.Method, req.URL, req.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString(`{"ok":true}`)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	})}
	for _, path := range []string{"/v1/merchant/profile", "/v1/admin/merchants", "/v1/admin/stores/1", "/v1/admin/merchant-accounts"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		NewHTTPHandler(authorizer, WithMerchantServiceURL("http://merchant.test"), WithHTTPClient(client)).ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
	}
}

func TestHTTPHandlerUserProxyMapsErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{name: "timeout", err: timeoutError{}, want: http.StatusGatewayTimeout},
		{name: "connection", err: errors.New("connection refused"), want: http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, tt.err })}
			authorizer := testAuthorizer(t)
			token, _ := authorizer.SignAccess("account-1", "user-1", "account-1", "tenant-1", nil)
			req := httptest.NewRequest(http.MethodGet, "/v1/users/1", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			NewHTTPHandler(authorizer, WithUserServiceURL("http://user.test"), WithHTTPClient(client)).ServeHTTP(response, req)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d", response.Code, tt.want)
			}
		})
	}
}
