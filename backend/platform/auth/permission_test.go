package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func permissionRequest(t *testing.T, service *Service, grant Grant) *http.Request {
	t.Helper()
	token, err := service.SignAccessGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func runPermission(t *testing.T, service *Service, grant Grant, codes ...string) *httptest.ResponseRecorder {
	t.Helper()
	called := false
	handler := Middleware(service)(RequirePermission(codes...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, permissionRequest(t, service, grant))
	if called != (response.Code == http.StatusNoContent) {
		t.Fatalf("handler invocation (%v) disagrees with status %d", called, response.Code)
	}
	return response
}

func TestRequirePermissionAllowsGrantedCode(t *testing.T) {
	service := newTestService(t)
	grant := Grant{Subject: "s1", Permissions: []string{"role:manage", "menu:manage"}}

	if response := runPermission(t, service, grant, "menu:manage"); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRequirePermissionRejectsMissingCode(t *testing.T) {
	service := newTestService(t)
	grant := Grant{Subject: "s1", Permissions: []string{"role:manage"}}

	response := runPermission(t, service, grant, "admin:manage")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

// A super administrator bypasses the check with no codes at all; the legacy
// console relied on this to bootstrap the very first role assignment.
func TestRequirePermissionAllowsSuperAdminWithoutCodes(t *testing.T) {
	service := newTestService(t)
	grant := Grant{Subject: "s1", IsSuper: true}

	if response := runPermission(t, service, grant, "admin:manage"); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

// Any one of the listed codes suffices, matching the legacy OR semantics.
func TestRequirePermissionAcceptsAnyOfSeveralCodes(t *testing.T) {
	service := newTestService(t)
	grant := Grant{Subject: "s1", Permissions: []string{"log:manage"}}

	if response := runPermission(t, service, grant, "admin:manage", "log:manage"); response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRequirePermissionRejectsUnauthenticatedRequest(t *testing.T) {
	called := false
	handler := RequirePermission("admin:manage")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if called {
		t.Fatal("handler ran without an identity")
	}
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}
