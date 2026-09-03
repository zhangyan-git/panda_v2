package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/domain"
)

type Handler struct {
	service *domain.Service
	auth    *auth.Service
	jtis    Store
}

func New(service *domain.Service, authorizer *auth.Service, stores ...any) *Handler {
	var store Store = NewMemoryStore()
	if len(stores) > 0 {
		switch provided := stores[0].(type) {
		case Store:
			if provided != nil {
				store = provided
			}
		case *JTIStore:
			if provided != nil {
				store = &MemoryStore{provided}
			}
		}
	}
	return &Handler{service: service, auth: authorizer, jtis: store}
}

type loginRequest struct {
	Username string             `json:"username"`
	Password string             `json:"password"`
	Type     domain.AccountType `json:"type"`
}
type loginResponse struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	TokenType    string         `json:"token_type"`
	ExpiresIn    int64          `json:"expires_in"`
	Account      domain.Account `json:"account,omitempty"`
}

type tokenRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req loginRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	account, err := h.service.Authenticate(r.Context(), req.Username, req.Password, req.Type)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, domain.ErrInvalidAccountType) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, domain.ErrAccountDisabled) {
			status = http.StatusForbidden
		}
		writeError(w, status, err.Error())
		return
	}
	access, err := h.auth.SignAccess(account.ID, account.UserID, account.ID, account.Tenant, account.Roles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	refresh, err := h.auth.SignRefresh(account.ID, account.UserID, account.ID, account.Tenant, account.Roles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := h.storeRefresh(r.Context(), refresh); err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int64(h.auth.AccessTokenTTL().Seconds()), Account: account})
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req tokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	claims, err := h.auth.ParseRefresh(req.RefreshToken)
	// 先校验账号仍然有效再消费 JTI：账号被禁用或查询失败时直接拒绝，
	// 避免把禁用账号的 refresh token 白白消耗掉。JTI 的一次性由下面的
	// 原子 Consume 保证，所以此处并发的重复请求仍只有一个能成功轮换。
	if err == nil {
		account, lookupErr := h.service.GetByID(r.Context(), claims.AccountID)
		if lookupErr != nil || account.Status != domain.StatusActive {
			err = errors.New("invalid refresh account")
		}
	}
	consumed := false
	if err == nil {
		consumed, err = h.jtis.Consume(r.Context(), claimsOrEmptyID(claims), time.Now())
	}
	if err != nil || !consumed {
		writeError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	access, err := h.auth.SignAccess(claims.Subject, claims.UserID, claims.AccountID, claims.Tenant, claims.Roles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	refresh, err := h.auth.SignRefresh(claims.Subject, claims.UserID, claims.AccountID, claims.Tenant, claims.Roles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := h.storeRefresh(r.Context(), refresh); err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int64(h.auth.AccessTokenTTL().Seconds())})
}

func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req tokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	claims, err := h.auth.ParseRefresh(req.RefreshToken)
	revoked := false
	if err == nil {
		revoked, err = h.jtis.Revoke(r.Context(), claimsOrEmptyID(claims), time.Now())
	}
	if err != nil || !revoked {
		writeError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (h *Handler) storeRefresh(ctx context.Context, token string) error {
	claims, err := h.auth.ParseRefresh(token)
	if err != nil || claims.ExpiresAt == nil {
		if err != nil {
			return err
		}
		return errors.New("refresh token has no expiry")
	}
	return h.jtis.Register(ctx, TokenRecord{JTI: claims.ID, AccountID: claims.AccountID, UserID: claims.UserID, ExpiresAt: claims.ExpiresAt.Time})
}

func claimsOrEmptyID(claims *auth.Claims) string {
	if claims == nil {
		return ""
	}
	return claims.ID
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func RegisterRoutes(mux *http.ServeMux, h *Handler) {
	mux.HandleFunc("/v1/auth/login", h.Login)
	mux.HandleFunc("/v1/auth/refresh", h.Refresh)
	mux.HandleFunc("/v1/auth/revoke", h.Revoke)
	mux.Handle("/v1/auth/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/login" && r.URL.Path != "/v1/auth/refresh" && r.URL.Path != "/v1/auth/revoke" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		switch r.URL.Path {
		case "/v1/auth/login":
			h.Login(w, r)
		case "/v1/auth/refresh":
			h.Refresh(w, r)
		case "/v1/auth/revoke":
			h.Revoke(w, r)
		}
	}))
}
