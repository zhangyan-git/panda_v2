package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
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

// List godoc
//
//	@Summary     获取商户列表（服务端分页；name 模糊、status 等值过滤，均可选）
//	@Tags        admin-merchants
//	@Produce     json
//	@Security    BearerAuth
//	@Param       name     query string false "商户名称模糊过滤"
//	@Param       status   query string false "状态过滤 pending/active/suspended"
//	@Param       page     query int    false "页码，从 1 开始，默认 1"
//	@Param       pageSize query int    false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]merchantResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/merchants [get]
func (h *AdminMerchantHandler) List(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	status := r.URL.Query().Get("status")
	page, pageSize, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	merchants, total, err := h.svc.List(r.Context(), name, status, page, pageSize)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]merchantResponse, len(merchants))
	for i, m := range merchants {
		resp[i] = toMerchantResponse(m)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
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
