package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/domain"
)

func TestLoginIssuesTokens(t *testing.T) {
	hash, _ := domain.HashPassword("secret")
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	h := New(domain.NewService(repo{account: domain.Account{ID: "a1", Username: "admin", PasswordHash: hash, Type: domain.AdminAccount, Status: domain.StatusActive}}), authorizer)
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "secret", "type": "admin"})
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Login(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var response loginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.AccessToken == "" || response.RefreshToken == "" {
		t.Fatalf("invalid response: %s", w.Body.String())
	}
	if _, err := authorizer.ParseAccess(response.AccessToken); err != nil {
		t.Fatal(err)
	}
}

func TestLoginRejectsTrailingJSON(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	hash := mustHash(t, "secret")
	h := New(domain.NewService(repo{account: domain.Account{ID: "a1", Username: "admin", PasswordHash: hash, Type: domain.AdminAccount, Status: domain.StatusActive}}), authorizer)
	w := httptest.NewRecorder()
	h.Login(w, httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"username":"admin","password":"secret","type":"admin"}{}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
func TestLoginRejectsDisabled(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	hash, _ := domain.HashPassword("secret")
	h := New(domain.NewService(repo{account: domain.Account{Username: "admin", PasswordHash: hash, Type: domain.AdminAccount, Status: domain.StatusDisabled}}), authorizer)
	body := bytes.NewBufferString(`{"username":"admin","password":"secret","type":"admin"}`)
	w := httptest.NewRecorder()
	h.Login(w, httptest.NewRequest(http.MethodPost, "/v1/auth/login", body))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d", w.Code)
	}
}

type repo struct{ account domain.Account }

func (r repo) GetByUsername(_ context.Context, _ string, _ domain.AccountType) (domain.Account, error) {
	return r.account, nil
}

func (r repo) GetByID(_ context.Context, _ string) (domain.Account, error) { return r.account, nil }

func TestRefreshRejectsDisabledAccount(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	store := NewJTIStore()
	h := New(domain.NewService(repo{account: domain.Account{ID: "a1", Status: domain.StatusDisabled}}), authorizer, store)
	token, err := authorizer.SignRefresh("a1", "u1", "a1", "tenant", nil)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := authorizer.ParseRefresh(token)
	store.Add(claims.ID, claims.ExpiresAt.Time)
	w := httptest.NewRecorder()
	h.Refresh(w, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewBufferString(`{"refresh_token":"`+token+`"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}
func TestRefreshRotatesTokenAndRejectsReuse(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	h := New(domain.NewService(repo{account: domain.Account{ID: "a1", UserID: "u1", Username: "admin", PasswordHash: mustHash(t, "secret"), Type: domain.AdminAccount, Status: domain.StatusActive}}), authorizer)
	loginBody := bytes.NewBufferString(`{"username":"admin","password":"secret","type":"admin"}`)
	login := httptest.NewRecorder()
	h.Login(login, httptest.NewRequest(http.MethodPost, "/v1/auth/login", loginBody))
	var issued loginResponse
	if err := json.Unmarshal(login.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}

	refresh := httptest.NewRecorder()
	h.Refresh(refresh, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewBufferString(`{"refresh_token":"`+issued.RefreshToken+`"}`)))
	if refresh.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body=%s", refresh.Code, refresh.Body.String())
	}
	var rotated loginResponse
	if err := json.Unmarshal(refresh.Body.Bytes(), &rotated); err != nil || rotated.AccessToken == "" || rotated.RefreshToken == issued.RefreshToken {
		t.Fatalf("invalid rotated response: %s", refresh.Body.String())
	}

	reuse := httptest.NewRecorder()
	h.Refresh(reuse, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewBufferString(`{"refresh_token":"`+issued.RefreshToken+`"}`)))
	if reuse.Code != http.StatusUnauthorized {
		t.Fatalf("reuse status = %d, want %d", reuse.Code, http.StatusUnauthorized)
	}
}

func TestRevokeInvalidatesRefreshToken(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	store := NewJTIStore()
	h := New(domain.NewService(repo{account: domain.Account{ID: "a1", Username: "admin", PasswordHash: mustHash(t, "secret"), Type: domain.AdminAccount, Status: domain.StatusActive}}), authorizer, store)
	token, err := authorizer.SignRefresh("a1", "u1", "a1", "tenant", nil)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := authorizer.ParseRefresh(token)
	store.Add(claims.ID, claims.ExpiresAt.Time)

	revoke := httptest.NewRecorder()
	h.Revoke(revoke, httptest.NewRequest(http.MethodPost, "/v1/auth/revoke", bytes.NewBufferString(`{"refresh_token":"`+token+`"}`)))
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke status = %d", revoke.Code)
	}
	if store.Consume(claims.ID, time.Now()) {
		t.Fatal("revoked token remained active")
	}
}

func TestJTIStoreExpiryAndOneTimeConsume(t *testing.T) {
	store := NewJTIStore()
	now := time.Now()
	store.Add("expired", now.Add(-time.Second))
	if store.Consume("expired", now) {
		t.Fatal("expired JTI was consumed")
	}
	store.Add("active", now.Add(time.Minute))
	if !store.Consume("active", now) || store.Consume("active", now) {
		t.Fatal("JTI was not one-time consumable")
	}
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := domain.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
