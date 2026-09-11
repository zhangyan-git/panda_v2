package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminAuthHandler 平台管理员登录相关接口
type AdminAuthHandler struct {
	authSvc *service.AdminAuthService
}

func NewAdminAuthHandler(authSvc *service.AdminAuthService) *AdminAuthHandler {
	return &AdminAuthHandler{authSvc: authSvc}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type tokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// Login godoc
//
//	@Summary     平台管理员登录
//	@Tags        admin-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body loginRequest true "登录参数"
//	@Success     200 {object} api.Response{data=tokenResponse}
//	@Failure     400 {object} api.Response
//	@Failure     401 {object} api.Response
//	@Router      /v1/admin/auth/login [post]
func (h *AdminAuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "用户名和密码不能为空")
		return
	}

	result, err := h.authSvc.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "用户名或密码错误")
			return
		}
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}

	api.Success(w, tokenResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
	})
}

// Me godoc
//
//	@Summary     获取当前平台管理员信息
//	@Tags        admin-auth
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=meResponse}
//	@Failure     401 {object} api.Response
//	@Failure     403 {object} api.Response "非平台身份或账号已禁用"
//	@Failure     503 {object} api.Response "账号状态查询不可用"
//	@Router      /v1/admin/users/me [get]
func (h *AdminAuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || identity.UserID == "" || identity.Subject != identity.UserID {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
		return
	}
	if identity.Tenant != "" {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "非平台管理员")
		return
	}
	user, err := h.authSvc.Profile(r.Context(), identity.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "账号不存在")
		return
	}
	if err != nil || user == nil || user.ID != identity.UserID {
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "账号服务暂不可用")
		return
	}
	if user.Status != "active" {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "账号已禁用")
		return
	}
	// 角色/权限取库中实时绑定，不用 token claims（登录时签发，会过期失真）
	roles, perms, err := h.authSvc.LiveAccess(r.Context(), identity.UserID)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	api.Success(w, meResponse{
		ID:          identity.UserID,
		Username:    user.Username,
		Name:        user.Name,
		Email:       user.Email,
		Roles:       roles,
		Permissions: perms,
	})
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// Logout godoc
//
//	@Summary     退出平台管理员登录
//	@Tags        admin-auth
//	@Accept      json
//	@Produce     json
//	@Success     200 {object} api.Response
//	@Router      /v1/admin/auth/logout [post]
func (h *AdminAuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	api.Success(w, nil)
}

// Refresh godoc
//
//	@Summary     刷新 access token
//	@Tags        admin-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body refreshRequest true "refresh token"
//	@Success     200 {object} api.Response{data=tokenResponse}
//	@Failure     400 {object} api.Response
//	@Failure     401 {object} api.Response
//	@Router      /v1/admin/auth/refresh [post]
func (h *AdminAuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil || req.RefreshToken == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	result, err := h.authSvc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "token 无效或已过期")
		return
	}
	api.Success(w, tokenResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
	})
}

type meResponse struct {
	ID          string   `json:"id"`
	Username    string   `json:"username"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
}

// MerchantAuthHandler 商户员工登录相关接口
type MerchantAuthHandler struct {
	authSvc *service.MerchantAuthService
}

func NewMerchantAuthHandler(authSvc *service.MerchantAuthService) *MerchantAuthHandler {
	return &MerchantAuthHandler{authSvc: authSvc}
}

type merchantLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login godoc
//
//	@Summary     商户账号登录（账号由平台在商户管理中创建，username 全局唯一）
//	@Tags        merchant-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body merchantLoginRequest true "登录参数"
//	@Success     200 {object} api.Response{data=tokenResponse}
//	@Failure     400 {object} api.Response
//	@Failure     401 {object} api.Response
//	@Failure     403 {object} api.Response
//	@Router      /v1/merchant/auth/login [post]
func (h *MerchantAuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req merchantLoginRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "用户名和密码不能为空")
		return
	}

	result, err := h.authSvc.Login(r.Context(), req.Username, req.Password, r.RemoteAddr)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidCredentials):
			api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "用户名或密码错误")
		case errors.Is(err, service.ErrMerchantUserDisabled),
			errors.Is(err, service.ErrMerchantPending),
			errors.Is(err, service.ErrMerchantSuspended):
			api.Error(w, http.StatusForbidden, api.CodeForbidden, err.Error())
		default:
			api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		}
		return
	}

	api.Success(w, tokenResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
	})
}

type merchantMeResponse struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	MerchantID   string `json:"merchantId"`
	MerchantName string `json:"merchantName"`
}

// Me godoc
//
//	@Summary     获取当前商户账号信息（含所属商户名称）
//	@Tags        merchant-auth
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=merchantMeResponse}
//	@Failure     401 {object} api.Response "身份无效或账号不存在"
//	@Failure     403 {object} api.Response "租户不符、账号禁用或商户不存在/待审核/已暂停"
//	@Failure     503 {object} api.Response "账号或商户查询不可用"
//	@Router      /v1/merchant/users/me [get]
func (h *MerchantAuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" || identity.Subject != identity.UserID {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
		return
	}
	if strings.TrimSpace(identity.Tenant) == "" {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "非商户身份")
		return
	}
	user, err := h.authSvc.Profile(r.Context(), identity.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "账号不存在")
		return
	}
	if err != nil || user == nil || user.ID != identity.UserID || strings.TrimSpace(user.MerchantID) == "" {
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "账号服务暂不可用")
		return
	}
	if user.MerchantID != identity.Tenant {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "账号不属于当前商户")
		return
	}
	if err := h.authSvc.CheckAccess(r.Context(), user); err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			api.Error(w, http.StatusForbidden, api.CodeForbidden, "商户不存在")
		case errors.Is(err, service.ErrMerchantUserDisabled),
			errors.Is(err, service.ErrMerchantPending),
			errors.Is(err, service.ErrMerchantSuspended):
			api.Error(w, http.StatusForbidden, api.CodeForbidden, err.Error())
		default:
			api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户服务暂不可用")
		}
		return
	}
	merchantName, err := h.authSvc.MerchantName(r.Context(), user.MerchantID)
	if errors.Is(err, pgx.ErrNoRows) {
		api.Error(w, http.StatusForbidden, api.CodeForbidden, "商户不存在")
		return
	}
	if err != nil || strings.TrimSpace(merchantName) == "" {
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户服务暂不可用")
		return
	}
	api.Success(w, merchantMeResponse{
		ID:           identity.UserID,
		Username:     user.Username,
		Name:         user.Name,
		Email:        user.Email,
		MerchantID:   user.MerchantID,
		MerchantName: merchantName,
	})
}
