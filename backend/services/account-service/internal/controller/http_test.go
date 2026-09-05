package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/token"
)

func TestLoginIssuesTokens(t *testing.T) {
	hash, _ := model.HashPassword("secret")
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	h := New(service.NewService(repo{account: model.Account{ID: "a1", Username: "admin", PasswordHash: hash, Type: model.AdminAccount, Status: model.StatusActive}}), authorizer)
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "secret", "type": "admin"})
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Login(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	response, err := decodeLoginResponse(w.Body.Bytes())
	if err != nil || response.AccessToken == "" || response.RefreshToken == "" {
		t.Fatalf("invalid response: %s", w.Body.String())
	}
	if _, err := authorizer.ParseAccess(response.AccessToken); err != nil {
		t.Fatal(err)
	}
}

func TestLoginRejectsTrailingJSON(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	hash := mustHash(t, "secret")
	h := New(service.NewService(repo{account: model.Account{ID: "a1", Username: "admin", PasswordHash: hash, Type: model.AdminAccount, Status: model.StatusActive}}), authorizer)
	w := httptest.NewRecorder()
	h.Login(w, httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"username":"admin","password":"secret","type":"admin"}{}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
func TestLoginRejectsDisabled(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	hash, _ := model.HashPassword("secret")
	h := New(service.NewService(repo{account: model.Account{Username: "admin", PasswordHash: hash, Type: model.AdminAccount, Status: model.StatusDisabled}}), authorizer)
	body := bytes.NewBufferString(`{"username":"admin","password":"secret","type":"admin"}`)
	w := httptest.NewRecorder()
	h.Login(w, httptest.NewRequest(http.MethodPost, "/v1/auth/login", body))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d", w.Code)
	}
}

type repo struct{ account model.Account }

func (r repo) GetByUsername(_ context.Context, _ string, _ model.AccountType) (model.Account, error) {
	return r.account, nil
}

func (r repo) GetByID(_ context.Context, _ string) (model.Account, error) { return r.account, nil }

func TestRefreshRejectsDisabledAccount(t *testing.T) {
	authorizer, _ := auth.NewService([]byte("test-secret-that-is-at-least-32-bytes-long"), "test", time.Hour, 2*time.Hour)
	store := token.NewJTIStore()
	h := New(service.NewService(repo{account: model.Account{ID: "a1", Status: model.StatusDisabled}}), authorizer, store)
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
	h := New(service.NewService(repo{account: model.Account{ID: "a1", UserID: "u1", Username: "admin", PasswordHash: mustHash(t, "secret"), Type: model.AdminAccount, Status: model.StatusActive}}), authorizer)
	loginBody := bytes.NewBufferString(`{"username":"admin","password":"secret","type":"admin"}`)
	login := httptest.NewRecorder()
	h.Login(login, httptest.NewRequest(http.MethodPost, "/v1/auth/login", loginBody))
	issued, err := decodeLoginResponse(login.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	refresh := httptest.NewRecorder()
	h.Refresh(refresh, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", bytes.NewBufferString(`{"refresh_token":"`+issued.RefreshToken+`"}`)))
	if refresh.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body=%s", refresh.Code, refresh.Body.String())
	}
	rotated, err := decodeLoginResponse(refresh.Body.Bytes())
	if err != nil || rotated.AccessToken == "" || rotated.RefreshToken == issued.RefreshToken {
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
	store := token.NewJTIStore()
	h := New(service.NewService(repo{account: model.Account{ID: "a1", Username: "admin", PasswordHash: mustHash(t, "secret"), Type: model.AdminAccount, Status: model.StatusActive}}), authorizer, store)
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
	store := token.NewJTIStore()
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

func decodeLoginResponse(body []byte) (loginResponse, error) {
	var envelope struct {
		Data loginResponse `json:"data"`
	}
	err := json.Unmarshal(body, &envelope)
	return envelope.Data, err
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := model.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
