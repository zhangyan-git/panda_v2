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

type MerchantAuthHandler struct {
	authSvc *service.MerchantAuthService
	// scope 与 authSvc 是同一件事的两半：authSvc 回答「这个账号能不能用」，
	// scope 回答「能用的话，边界在哪」。两者都必须现取，不能只取一个。
	scope *service.MerchantAccessService
}

func NewMerchantAuthHandler(authSvc *service.MerchantAuthService, scope *service.MerchantAccessService) *MerchantAuthHandler {
	return &MerchantAuthHandler{authSvc: authSvc, scope: scope}
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

	// 与 C 端两条登录路径一致，走 loginIP(r)：r.RemoteAddr 带端口，且经网关后
	// 是网关自己的地址，记进 last_login_ip 就再也按地址聚合不出任何东西。
	result, err := h.authSvc.Login(r.Context(), req.Username, req.Password, loginIP(r))
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
	// 数据范围这几列回显的就是列表接口真正用来过滤的那个边界，不是另算的一份描述。
	ScopeType string   `json:"scopeType"`
	ScopeIDs  []string `json:"scopeIds"`
	// ScopeNames 是范围目标的名称，与 scopeIds 同序等长：品牌档与门店档能查到，商户档
	// 是空数组——那一档的名字（「全部门店」）是界面文案，由前端自己出，服务端不替它定。
	// 范围目标在授权之后被删除时该位置留空串，那是展示数据，不该把一次登录态查询变成错误。
	ScopeNames []string `json:"scopeNames"`
}

// Me godoc
//
//	@Summary     获取当前商户账号信息（含所属商户名称与数据范围）
//	@Tags        merchant-auth
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=merchantMeResponse}
//	@Failure     401 {object} api.Response "身份无效或账号不存在"
//	@Failure     403 {object} api.Response "租户不符、账号禁用或商户不存在/待审核/已暂停"
//	@Failure     503 {object} api.Response "账号、商户或数据范围查询不可用"
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
	// 数据范围现取，不缓存也不签进令牌：商户端要拿它显示「你的数据范围」，而这一列
	// 必须是列表接口下一个请求真正会用的那个边界，否则界面会显示一个已经不成立的承诺。
	//
	// 边界给不出来时整条 Me 失败，而不是少回两列：少回两列会让界面显示成「没有范围」，
	// 而事实是「不知道范围」——那两句在用户那里读起来一样，在这里必须分开。
	access, err := h.scope.ScopeOf(r.Context(), user)
	if err != nil {
		if errors.Is(err, service.ErrScopeTypeInvalid) {
			api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "数据范围无法识别")
			return
		}
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "数据范围暂不可用")
		return
	}
	scopeNames, err := h.scope.MerchantScopeNames(r.Context(), access)
	if err != nil {
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "数据范围暂不可用")
		return
	}
	api.Success(w, merchantMeResponse{
		ID:           identity.UserID,
		Username:     user.Username,
		Name:         user.Name,
		Email:        user.Email,
		MerchantID:   user.MerchantID,
		MerchantName: merchantName,
		ScopeType:    access.ScopeType,
		ScopeIDs:     nonNilStrings(access.ScopeIDs),
		ScopeNames:   nonNilStrings(scopeNames),
	})
}
