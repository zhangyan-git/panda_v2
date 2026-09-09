package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminMerchantHandler 平台侧商户管理接口：商户 CRUD + 状态流转 + 商户账号维护
type AdminMerchantHandler struct {
	svc *service.AdminMerchantService
}

func NewAdminMerchantHandler(svc *service.AdminMerchantService) *AdminMerchantHandler {
	return &AdminMerchantHandler{svc: svc}
}

type merchantResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	ContactName  string `json:"contactName"`
	ContactPhone string `json:"contactPhone"`
	ContactEmail string `json:"contactEmail"`
	CreatedAt    string `json:"createdAt"`
}

func toMerchantResponse(m *model.Merchant) merchantResponse {
	return merchantResponse{
		ID:           m.ID,
		Name:         m.Name,
		Status:       m.Status,
		ContactName:  m.ContactName,
		ContactPhone: m.ContactPhone,
		ContactEmail: m.ContactEmail,
		CreatedAt:    m.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
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
	ScopeName   string `json:"scopeName"` // 联表计算列：范围品牌/门店名称
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

// List godoc
//
//	@Summary     获取商户列表（name 模糊、status 等值过滤，均可选）
//	@Tags        admin-merchants
//	@Produce     json
//	@Security    BearerAuth
//	@Param       name   query string false "商户名称模糊过滤"
//	@Param       status query string false "状态过滤 pending/active/suspended"
//	@Success     200 {object} api.Response{data=[]merchantResponse}
//	@Router      /v1/admin/merchants [get]
func (h *AdminMerchantHandler) List(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	status := r.URL.Query().Get("status")
	merchants, err := h.svc.List(r.Context(), name, status)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]merchantResponse, len(merchants))
	for i, m := range merchants {
		resp[i] = toMerchantResponse(m)
	}
	api.Success(w, resp)
}

// Get godoc
//
//	@Summary     获取单个商户
//	@Tags        admin-merchants
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "商户ID"
//	@Success     200 {object} api.Response{data=merchantResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchants/{id} [get]
func (h *AdminMerchantHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	m, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		writeMerchantError(w, err, "服务内部错误")
		return
	}
	api.Success(w, toMerchantResponse(m))
}

// merchantRequest 创建/更新商户；status 不在 body 中，只走 PATCH 状态接口
type merchantRequest struct {
	Name         string `json:"name"`
	ContactName  string `json:"contactName"`
	ContactPhone string `json:"contactPhone"`
	ContactEmail string `json:"contactEmail"`
}

// Create godoc
//
//	@Summary     创建商户（初始状态 pending 待审核）
//	@Tags        admin-merchants
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body merchantRequest true "商户信息"
//	@Success     200 {object} api.Response{data=merchantResponse}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/merchants [post]
func (h *AdminMerchantHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req merchantRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	m, err := h.svc.Create(r.Context(), req.Name, req.ContactName, req.ContactPhone, req.ContactEmail)
	if err != nil {
		writeMerchantError(w, err, "创建失败")
		return
	}
	api.Success(w, toMerchantResponse(m))
}

// Update godoc
//
//	@Summary     更新商户（名称与联系人，不含状态）
//	@Tags        admin-merchants
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string         true "商户ID"
//	@Param       body body merchantRequest true "商户信息"
//	@Success     200 {object} api.Response{data=merchantResponse}
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchants/{id} [put]
func (h *AdminMerchantHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req merchantRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	m, err := h.svc.Update(r.Context(), id, req.Name, req.ContactName, req.ContactPhone, req.ContactEmail)
	if err != nil {
		writeMerchantError(w, err, "更新失败")
		return
	}
	api.Success(w, toMerchantResponse(m))
}

// updateMerchantStatusRequest 状态流转（updateStatusRequest 已被 handler/user.go 占用）
type updateMerchantStatusRequest struct {
	Status string `json:"status"` // active | suspended
}

// UpdateStatus godoc
//
//	@Summary     商户状态流转（pending→active 审核通过 / active→suspended 暂停 / suspended→active 恢复）
//	@Tags        admin-merchants
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string                       true "商户ID"
//	@Param       body body updateMerchantStatusRequest true "目标状态"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchants/{id}/status [patch]
func (h *AdminMerchantHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req updateMerchantStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Status != "active" && req.Status != "suspended" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "status 只能为 active 或 suspended")
		return
	}
	if err := h.svc.UpdateStatus(r.Context(), id, req.Status); err != nil {
		writeMerchantError(w, err, "操作失败")
		return
	}
	api.Success(w, nil)
}

// Delete godoc
//
//	@Summary     删除商户（名下存在账号时拒绝）
//	@Tags        admin-merchants
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "商户ID"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchants/{id} [delete]
func (h *AdminMerchantHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeMerchantError(w, err, "删除失败")
		return
	}
	api.Success(w, nil)
}

// ListUsers godoc
//
//	@Summary     获取商户下的登录账号列表
//	@Tags        admin-merchants
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "商户ID"
//	@Success     200 {object} api.Response{data=[]merchantUserResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/merchants/{id}/users [get]
func (h *AdminMerchantHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	users, err := h.svc.ListUsers(r.Context(), id)
	if err != nil {
		writeMerchantError(w, err, "服务内部错误")
		return
	}
	resp := make([]merchantUserResponse, len(users))
	for i, u := range users {
		resp[i] = toMerchantUserResponse(u)
	}
	api.Success(w, resp)
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
	u, err := h.svc.CreateUser(r.Context(), id, req.Username, req.Password, req.Name, req.Email, req.Phone, req.IsAdmin, req.ScopeType, req.ScopeID)
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
	if err := h.svc.UpdateUserScope(r.Context(), id, req.ScopeType, req.ScopeID, req.IsAdmin); err != nil {
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
	if err := h.svc.UpdateUserStatus(r.Context(), id, req.Status); err != nil {
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
	if err := h.svc.DeleteUser(r.Context(), id); err != nil {
		writeMerchantError(w, err, "删除失败")
		return
	}
	api.Success(w, nil)
}

// writeMerchantError 将商户业务错误映射到 HTTP 状态码
func writeMerchantError(w http.ResponseWriter, err error, internalMsg string) {
	switch {
	case errors.Is(err, service.ErrMerchantNameRequired),
		errors.Is(err, service.ErrMerchantStatusTransition),
		errors.Is(err, service.ErrMerchantHasUsers),
		errors.Is(err, service.ErrMerchantUsernameTaken),
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
