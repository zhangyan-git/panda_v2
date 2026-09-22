package handler

import (
	"errors"
	"net/http"

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
	// 平台管理员判定必须走 IsPlatformAdmin：C 端 token 同样满足
	// subject == user_id 且 tenant 为空，只查这两条会把小程序顾客当成管理员。
	if !identity.IsPlatformAdmin() {
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
//	@Summary     退出平台管理员登录（撤销这枚 refresh token 对应的会话）
//	@Description 无需 access token：令牌过期时最需要退出，要求它有效反而让最需要的时候用不了。对未知令牌一律返回成功；body 可以不带 refreshToken（老客户端不带），此时服务端无会话可撤销。
//	@Tags        admin-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body refreshRequest false "refresh token"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/auth/logout [post]
func (h *AdminAuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	// 空 body 是允许的：后台前端至今只发一个不带内容的 POST，把「没有令牌」
	// 当成请求格式错误会让退出登录直接失败。令牌为空时 service 按无会话可撤销
	// 处理，与它对未知令牌的态度一致。
	var req refreshRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
			return
		}
	}
	if err := h.authSvc.Logout(r.Context(), req.RefreshToken); err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "退出失败")
		return
	}
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
//	@Failure     401 {object} api.Response "refresh token 无效、过期或已被使用过"
//	@Failure     403 {object} api.Response "账号已禁用"
//	@Router      /v1/admin/auth/refresh [post]
func (h *AdminAuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil || req.RefreshToken == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	result, err := h.authSvc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		// 与 C 端刷新同一套映射：令牌不可用是 401，账号不能用是 403，
		// 两者客户端要做的事不同——一个是重新登录，一个是找管理员。
		switch {
		case errors.Is(err, service.ErrRefreshTokenInvalid),
			errors.Is(err, service.ErrRefreshTokenReused):
			api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, err.Error())
		case errors.Is(err, service.ErrAdminUserDisabled):
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

type meResponse struct {
	ID          string   `json:"id"`
	Username    string   `json:"username"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
}

// MerchantAuthHandler 商户员工登录相关接口
