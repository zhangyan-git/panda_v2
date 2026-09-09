package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewHandlerRoutesAndNormalizesAPIPath(t *testing.T) {
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/brands/7" || r.URL.RawQuery != "q=coffee" {
			t.Errorf("merchant request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("authorization header was not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"7"}}`))
	}))
	defer merchant.Close()

	user := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/merchant/auth/login" {
			t.Errorf("user request path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"opaque"}`))
	}))
	defer user.Close()

	h, err := NewHandler(Config{MerchantServiceURL: merchant.URL, UserServiceURL: user.URL})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	t.Run("merchant", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/brands/7?q=coffee", nil)
		req.Header.Set("Authorization", "Bearer test")
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
		}
		body, _ := io.ReadAll(res.Result().Body)
		if string(body) != `{"success":true,"data":{"id":"7"}}` {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("user", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/merchant/auth/login", nil)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
		}
	})
}

func TestNewHandlerRejectsUnknownPath(t *testing.T) {
	h, err := NewHandler(Config{MerchantServiceURL: "http://merchant.test", UserServiceURL: "http://user.test"})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNotFound)
	}
}

func TestNewHandlerRequiresAbsoluteUpstreamURLs(t *testing.T) {
	if _, err := NewHandler(Config{MerchantServiceURL: "/merchant", UserServiceURL: "http://user.test"}); err == nil {
		t.Fatal("NewHandler() error = nil, want invalid merchant URL error")
	}
}
