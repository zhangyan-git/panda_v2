package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminMerchantHandler 平台侧商户账号管理接口：商户登录账号维护，商户主体本身由 merchant-service 负责
type AdminMerchantHandler struct {
	accountSvc *service.MerchantAccountService
}

func NewAdminMerchantHandler(accountSvc *service.MerchantAccountService) *AdminMerchantHandler {
	return &AdminMerchantHandler{accountSvc: accountSvc}
}

type merchantUserResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	Status      string `json:"status"`
	IsAdmin     bool   `json:"isAdmin"`
	ScopeType   string `json:"scopeType"`
	ScopeID     string `json:"scopeId"`
	ScopeName   string `json:"scopeName"` // 范围品牌/门店名称，经 merchant-service 解析
	LastLoginAt string `json:"lastLoginAt"`
	CreatedAt   string `json:"createdAt"`
}

func toMerchantUserResponse(u *model.MerchantUser) merchantUserResponse {
	lastLogin := ""
	if u.LastLoginAt != nil {
		lastLogin = u.LastLoginAt.Format("2006-01-02T15:04:05Z")
	}
	return merchantUserResponse{
		ID:          u.ID,
		Username:    u.Username,
		Name:        u.Name,
		Email:       u.Email,
		Phone:       u.Phone,
		Status:      u.Status,
		IsAdmin:     u.IsAdmin,
		ScopeType:   u.ScopeType,
		ScopeID:     u.ScopeID,
		ScopeName:   u.ScopeName,
		LastLoginAt: lastLogin,
		CreatedAt:   u.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

// ListUsers godoc
//
//	@Summary     获取商户下的登录账号列表（服务端分页）
//	@Tags        admin-merchants
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id       path  string true  "商户ID"
//	@Param       page     query int    false "页码，从 1 开始，默认 1"
//	@Param       pageSize query int    false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]merchantUserResponse}}
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchants/{id}/users [get]
func (h *AdminMerchantHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	page, pageSize, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	users, total, err := h.accountSvc.ListUsers(r.Context(), id, page, pageSize)
	if err != nil {
		writeMerchantError(w, err, "服务内部错误")
		return
	}
	resp := make([]merchantUserResponse, len(users))
	for i, u := range users {
		resp[i] = toMerchantUserResponse(u)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
}

// createMerchantUserRequest 创建商户账号；数据范围单点三选一：
// scopeType=merchant（默认，看整个商户）/ brand / store（brand/store 时 scopeId 必填）
type createMerchantUserRequest struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	IsAdmin   bool   `json:"isAdmin"`
	ScopeType string `json:"scopeType"`
	ScopeID   string `json:"scopeId"`
}

// CreateUser godoc
//
//	@Summary     为商户创建登录账号（username 全局唯一；数据范围 merchant/brand/store 单点关联）
//	@Tags        admin-merchants
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string                     true "商户ID"
//	@Param       body body createMerchantUserRequest true "账号信息"
//	@Success     200 {object} api.Response{data=merchantUserResponse}
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchants/{id}/users [post]
func (h *AdminMerchantHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req createMerchantUserRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "用户名和密码不能为空")
		return
	}
	u, err := h.accountSvc.CreateUser(r.Context(), id, req.Username, req.Password, req.Name, req.Email, req.Phone, req.IsAdmin, req.ScopeType, req.ScopeID)
	if err != nil {
		writeMerchantError(w, err, "创建失败")
		return
	}
	api.Success(w, toMerchantUserResponse(u))
}

// updateMerchantUserScopeRequest 调整账号数据范围（单点三选一）
type updateMerchantUserScopeRequest struct {
	ScopeType string `json:"scopeType"` // merchant | brand | store
	ScopeID   string `json:"scopeId"`   // brand/store 时必填
	IsAdmin   bool   `json:"isAdmin"`
}

// UpdateUserScope godoc
//
//	@Summary     调整商户账号数据范围（merchant/brand/store 单点关联，目标必须属于该商户）
//	@Tags        admin-merchants
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string                            true "账号ID"
//	@Param       body body updateMerchantUserScopeRequest true "数据范围"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchant-users/{id}/scope [patch]
func (h *AdminMerchantHandler) UpdateUserScope(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req updateMerchantUserScopeRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if err := h.accountSvc.UpdateUserScope(r.Context(), id, req.ScopeType, req.ScopeID, req.IsAdmin); err != nil {
		writeMerchantError(w, err, "操作失败")
		return
	}
	api.Success(w, nil)
}

type updateMerchantUserStatusRequest struct {
	Status string `json:"status"` // active | disabled
}

// UpdateUserStatus godoc
//
//	@Summary     启用/禁用商户账号
//	@Tags        admin-merchants
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string                             true "账号ID"
//	@Param       body body updateMerchantUserStatusRequest true "状态"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchant-users/{id}/status [patch]
func (h *AdminMerchantHandler) UpdateUserStatus(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req updateMerchantUserStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Status != "active" && req.Status != "disabled" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "status 只能为 active 或 disabled")
		return
	}
	if err := h.accountSvc.UpdateUserStatus(r.Context(), id, req.Status); err != nil {
		writeMerchantError(w, err, "操作失败")
		return
	}
	api.Success(w, nil)
}

// DeleteUser godoc
//
//	@Summary     删除商户账号
//	@Tags        admin-merchants
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "账号ID"
//	@Success     200 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchant-users/{id} [delete]
func (h *AdminMerchantHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	if err := h.accountSvc.DeleteUser(r.Context(), id); err != nil {
		writeMerchantError(w, err, "删除失败")
		return
	}
	api.Success(w, nil)
}

// writeMerchantError 将商户账号业务错误映射到 HTTP 状态码
func writeMerchantError(w http.ResponseWriter, err error, internalMsg string) {
	switch {
	case errors.Is(err, service.ErrMerchantUsernameTaken),
		errors.Is(err, service.ErrScopeTypeInvalid),
		errors.Is(err, service.ErrScopeIDRequired),
		errors.Is(err, service.ErrScopeOutOfMerchant):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "商户或账号不存在")
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, internalMsg)
	}
}
