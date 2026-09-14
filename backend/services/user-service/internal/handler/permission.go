package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

type AdminPermissionHandler struct {
	svc *service.AdminPermissionService
}

func NewAdminPermissionHandler(svc *service.AdminPermissionService) *AdminPermissionHandler {
	return &AdminPermissionHandler{svc: svc}
}

// List godoc
//
//	@Summary     获取平台权限列表（服务端分页）
//	@Tags        admin-permissions
//	@Produce     json
//	@Security    BearerAuth
//	@Param       page     query int false "页码，从 1 开始，默认 1"
//	@Param       pageSize query int false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]permResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/permissions [get]
func (h *AdminPermissionHandler) List(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	perms, total, err := h.svc.List(r.Context(), page, pageSize)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]permResponse, len(perms))
	for i, p := range perms {
		resp[i] = toPermResponse(p)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
}

// ListByRole godoc
//
//	@Summary     获取角色已绑定的权限
//	@Tags        admin-permissions
//	@Produce     json
//	@Security    BearerAuth
//	@Param       roleId path string true "角色ID"
//	@Success     200 {object} api.Response{data=[]permResponse}
//	@Router      /v1/admin/roles/{roleId}/permissions [get]
//
// 刻意不分页：返回的是「该角色已绑定的权限集合」，前端拿它做授权回显与全选比对，
// 少一条就等于少显示一项已生效的授权。分页只用于平铺表格，集合语义的接口保持裸数组。
func (h *AdminPermissionHandler) ListByRole(w http.ResponseWriter, r *http.Request) {
	roleID := pathVar(r, "roleId")
	perms, err := h.svc.ListByRole(r.Context(), roleID)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]permResponse, len(perms))
	for i, p := range perms {
		resp[i] = toPermResponse(p)
	}
	api.Success(w, resp)
}

// Get godoc
//
//	@Summary     获取单个权限
//	@Tags        admin-permissions
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "权限ID"
//	@Success     200 {object} api.Response{data=permResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/permissions/{id} [get]
func (h *AdminPermissionHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	p, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "权限不存在")
		return
	}
	api.Success(w, toPermResponse(p))
}

type createPermRequest struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Group       string `json:"group"`
}

// Create godoc
//
//	@Summary     创建权限
//	@Tags        admin-permissions
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body createPermRequest true "权限信息"
//	@Success     200 {object} api.Response{data=permResponse}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/permissions [post]
func (h *AdminPermissionHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createPermRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Code == "" || req.Name == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "权限代码和名称不能为空")
		return
	}
	p, err := h.svc.Create(r.Context(), req.Code, req.Name, req.Description, req.Group)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "创建失败")
		return
	}
	api.Success(w, toPermResponse(p))
}

type updatePermRequest struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Group       string `json:"group"`
}

// Update godoc
//
//	@Summary     更新权限
//	@Tags        admin-permissions
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string           true "权限ID"
//	@Param       body body updatePermRequest true "权限信息"
//	@Success     200 {object} api.Response{data=permResponse}
//	@Router      /v1/admin/permissions/{id} [put]
func (h *AdminPermissionHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req updatePermRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Code == "" || req.Name == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "权限代码和名称不能为空")
		return
	}
	p, err := h.svc.Update(r.Context(), id, req.Code, req.Name, req.Description, req.Group)
	if err != nil {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "权限不存在")
		return
	}
	api.Success(w, toPermResponse(p))
}

// Delete godoc
//
//	@Summary     删除权限
//	@Tags        admin-permissions
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "权限ID"
//	@Success     200 {object} api.Response
//	@Router      /v1/admin/permissions/{id} [delete]
func (h *AdminPermissionHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "权限不存在")
		return
	}
	api.Success(w, nil)
}

// AdminBindingHandler 角色-权限、用户-角色绑定接口
