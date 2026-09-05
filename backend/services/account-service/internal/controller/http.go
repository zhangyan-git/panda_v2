package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"io"
	"net/http"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api/codes"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/service"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/token"
)

type Handler struct {
	service *service.Service
	auth    *auth.Service
	jtis    token.Store
}

func New(service *service.Service, authorizer *auth.Service, stores ...any) *Handler {
	var store token.Store = token.NewMemoryStore()
	if len(stores) > 0 {
		switch provided := stores[0].(type) {
		case token.Store:
			if provided != nil {
				store = provided
			}
		case *token.JTIStore:
			if provided != nil {
				store = &token.MemoryStore{JTIStore: provided}
			}
		}
	}
	return &Handler{service: service, auth: authorizer, jtis: store}
}

type loginRequest = dto.LoginRequest
type loginResponse = dto.LoginResponse
type tokenRequest = dto.TokenRequest

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
		if errors.Is(err, service.ErrInvalidAccountType) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, service.ErrAccountDisabled) {
			status = http.StatusForbidden
		}
		code := 0
		switch {
		case errors.Is(err, service.ErrInvalidCredentials):
			code = codes.AccountInvalidCredentials
		case errors.Is(err, service.ErrInvalidAccountType):
			code = codes.AccountInvalidType
		case errors.Is(err, service.ErrAccountDisabled):
			code = codes.AccountDisabled
		}
		writeErrorCode(w, status, code, err.Error())
		return
	}
	access, err := h.auth.SignAccess(account.ID, account.UserID, account.ID, account.Tenant, model.CanonicalRoles(account))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	refresh, err := h.auth.SignRefresh(account.ID, account.UserID, account.ID, account.Tenant, model.CanonicalRoles(account))
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
	var account model.Account
	if err == nil {
		account, err = h.service.GetByID(r.Context(), claims.AccountID)
		if err == nil && account.Status != model.StatusActive {
			err = errors.New("invalid refresh account")
		}
	}
	if err != nil {
		writeErrorCode(w, http.StatusUnauthorized, codes.AccountInvalidRefresh, "invalid refresh token")
		return
	}
	// Rebuild claims from the current account so role, tenant, and user changes
	// take effect immediately after refresh.
	access, err := h.auth.SignAccess(account.ID, account.UserID, account.ID, account.Tenant, model.CanonicalRoles(account))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	refresh, err := h.auth.SignRefresh(account.ID, account.UserID, account.ID, account.Tenant, model.CanonicalRoles(account))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	newClaims, err := h.auth.ParseRefresh(refresh)
	if err != nil || newClaims.ExpiresAt == nil {
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	rotated, err := h.jtis.Rotate(r.Context(), claims.ID, token.TokenRecord{JTI: newClaims.ID, AccountID: account.ID, UserID: account.UserID, ExpiresAt: newClaims.ExpiresAt.Time}, time.Now())
	if err != nil || !rotated {
		writeErrorCode(w, http.StatusUnauthorized, codes.AccountInvalidRefresh, "invalid refresh token")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int64(h.auth.AccessTokenTTL().Seconds()), Account: account})
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
		writeErrorCode(w, http.StatusUnauthorized, codes.AccountInvalidRefresh, "invalid refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (h *Handler) storeRefresh(ctx context.Context, refreshToken string) error {
	claims, err := h.auth.ParseRefresh(refreshToken)
	if err != nil || claims.ExpiresAt == nil {
		if err != nil {
			return err
		}
		return errors.New("refresh token has no expiry")
	}
	return h.jtis.Register(ctx, token.TokenRecord{JTI: claims.ID, AccountID: claims.AccountID, UserID: claims.UserID, ExpiresAt: claims.ExpiresAt.Time})
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
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	api.Success(w, status, value)
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeErrorCode(w, status, api.CodeForStatus(status), message)
}

func writeErrorCode(w http.ResponseWriter, status, code int, message string) {
	if code == 0 {
		code = api.CodeForStatus(status)
	}
	api.Error(w, status, code, message)
}
