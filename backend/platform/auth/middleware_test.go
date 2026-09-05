package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareAddsIdentityToContext(t *testing.T) {
	service := newTestService(t)
	token, err := service.SignAccess("subject-1", "user-1", "account-1", "tenant-1", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}

	var got Identity
	handler := Middleware(service)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ok bool
		got, ok = IdentityFromRequest(r)
		if !ok {
			t.Fatal("identity missing from request context")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if got.Subject != "subject-1" || got.Tenant != "tenant-1" || len(got.Roles) != 1 || got.Roles[0] != "admin" {
		t.Fatalf("unexpected identity: %+v", got)
	}
}

func TestMiddlewareRejectsMissingAndMalformedAuthorization(t *testing.T) {
	service := newTestService(t)
	handler := Middleware(service)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	for _, header := range []string{"", "Basic token", "Bearer", "Bearer one two", "bearer"} {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", header)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			assertUnauthorized(t, response)
		})
	}
}

func TestMiddlewareRejectsInvalidAndWrongTypeTokens(t *testing.T) {
	service := newTestService(t)
	refresh, err := service.SignRefresh("subject-1", "user-1", "account-1", "tenant-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"invalid token": "Bearer not-a-token",
		"refresh token": "Bearer " + refresh,
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", header)
			response := httptest.NewRecorder()
			Middleware(service)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler should not be called")
			})).ServeHTTP(response, req)
			assertUnauthorized(t, response)
		})
	}
}

func TestMiddlewareCanRequireSpecificTokenType(t *testing.T) {
	service := newTestService(t)
	token, err := service.SignRefresh("subject-1", "user-1", "account-1", "tenant-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	Middleware(service, RefreshTokenType)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := IdentityFromContext(r.Context()); !ok {
			t.Fatal("identity missing from context")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestIdentityFromContextWithoutIdentity(t *testing.T) {
	if _, ok := IdentityFromContext(nil); ok {
		t.Fatal("nil context should not contain identity")
	}
	if _, ok := IdentityFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); ok {
		t.Fatal("empty context should not contain identity")
	}
}

func assertUnauthorized(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q, want application/json", response.Header().Get("Content-Type"))
	}
	var body struct {
		Status  string `json:"status"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "error" || body.Code != 40100 || body.Message != "unauthorized" || body.Data != nil {
		t.Fatalf("unexpected envelope: %+v", body)
	}
}
