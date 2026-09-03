package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/domain"
)

type fakeRepository struct {
	user        domain.User
	err         error
	createCalls *int
}

func (f fakeRepository) Create(context.Context, string) (domain.User, error) {
	if f.createCalls != nil {
		*f.createCalls++
	}
	return f.user, f.err
}
func (f fakeRepository) GetByID(context.Context, int64) (domain.User, error) { return f.user, f.err }
func (f fakeRepository) Update(context.Context, int64, domain.UserUpdate) (domain.User, error) {
	return f.user, f.err
}

func TestRegister(t *testing.T) {
	h := New(domain.NewService(fakeRepository{user: domain.User{ID: 1, Name: "Alice", CreatedAt: time.Now()}}))
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`{"name":"Alice"}`))
	res := httptest.NewRecorder()
	h.Register(res, req)
	if res.Code != http.StatusCreated || !strings.Contains(res.Body.String(), `"name":"Alice"`) {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestRegisterRejectsTrailingJSONData(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"trailing garbage", `{"name":"Alice"} garbage`},
		{"multiple JSON values", `{"name":"Alice"}{"name":"Bob"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(domain.NewService(fakeRepository{user: domain.User{ID: 1, Name: "Alice"}}))
			res := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(tt.body))
			h.Register(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestRegisterRejectsOversizedBody(t *testing.T) {
	createCalls := 0
	h := New(domain.NewService(fakeRepository{createCalls: &createCalls}))
	body := `{"name":"` + strings.Repeat("a", maxRegisterBodyBytes) + `"}`
	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(body))
	h.Register(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if createCalls != 0 {
		t.Fatalf("create calls=%d, want 0", createCalls)
	}
}

func TestGetByID(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		err    error
		status int
	}{
		{"success", "/users/7", nil, http.StatusOK},
		{"invalid id", "/users/nope", nil, http.StatusBadRequest},
		{"not found", "/users/7", domain.ErrNotFound, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(domain.NewService(fakeRepository{user: domain.User{ID: 7, Name: "Alice"}, err: tt.err}))
			res := httptest.NewRecorder()
			h.GetByID(res, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if res.Code != tt.status {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestHandlersRejectWrongMethod(t *testing.T) {
	h := New(domain.NewService(fakeRepository{}))
	res := httptest.NewRecorder()
	h.Register(res, httptest.NewRequest(http.MethodGet, "/users", nil))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("register status=%d", res.Code)
	}
	res = httptest.NewRecorder()
	h.GetByID(res, httptest.NewRequest(http.MethodPost, "/users/1", nil))
	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get status=%d", res.Code)
	}
	if errors.Is(nil, domain.ErrNotFound) {
		t.Fatal("unexpected error")
	}
}

func TestProfileRequiresMatchingBearerUserToken(t *testing.T) {
	authorizer, err := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes"), "test", time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.User{ID: 1, Name: "Alice"}
	h := New(domain.NewService(fakeRepository{user: user}))

	token, err := authorizer.SignAccess("account-1", "1", "account-1", "tenant-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/users/1", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	auth.Bearer(authorizer)(http.HandlerFunc(h.Profile)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	for name, header := range map[string]string{
		"missing": "",
		"invalid": "Bearer invalid",
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/users/1", nil)
			request.Header.Set("Authorization", header)
			response := httptest.NewRecorder()
			auth.Bearer(authorizer)(http.HandlerFunc(h.Profile)).ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	otherToken, err := authorizer.SignAccess("account-2", "2", "account-2", "tenant-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/users/1", nil)
	request.Header.Set("Authorization", "Bearer "+otherToken)
	response = httptest.NewRecorder()
	auth.Bearer(authorizer)(http.HandlerFunc(h.Profile)).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUpdateRejectsInvalidPatchBodies(t *testing.T) {
	for _, body := range []string{"null", `{}`, `{"status":"disabled"}`} {
		t.Run(body, func(t *testing.T) {
			h := New(domain.NewService(fakeRepository{user: domain.User{ID: 1}}))
			res := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/v1/users/1", strings.NewReader(body))
			h.Update(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}
