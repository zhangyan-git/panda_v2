package handler

import (
	"net/http"

	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminRoleHandler 平台角色管理接口
type AdminRoleHandler struct {
	svc *service.AdminRoleService
}

func NewAdminRoleHandler(svc *service.AdminRoleService) *AdminRoleHandler {
	return &AdminRoleHandler{svc: svc}
}

// List godoc
//
//	@Summary     获取平台角色列表（服务端分页）
//	@Tags        admin-roles
//	@Produce     json
//	@Security    BearerAuth
//	@Param       page     query int false "页码，从 1 开始，默认 1"
//	@Param       pageSize query int false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]roleResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/roles [get]
func (h *AdminRoleHandler) List(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	roles, total, err := h.svc.List(r.Context(), page, pageSize)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]roleResponse, len(roles))
	for i, role := range roles {
		resp[i] = toRoleResponse(role)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
}

// Get godoc
//
//	@Summary     获取单个平台角色
//	@Tags        admin-roles
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "角色ID"
//	@Success     200 {object} api.Response{data=roleResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/roles/{id} [get]
func (h *AdminRoleHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	role, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "角色不存在")
		return
	}
	api.Success(w, toRoleResponse(role))
}

type createRoleRequest struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Create godoc
//
//	@Summary     创建平台角色
//	@Tags        admin-roles
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body createRoleRequest true "角色信息"
//	@Success     200 {object} api.Response{data=roleResponse}
//	@Failure     400 {object} api.Response "角色代码为空、含 CSV 特殊字符或为保留代码"
//	@Failure     409 {object} api.Response "角色代码已存在或存在同名平台授权规则"
//	@Failure     500 {object} api.Response
//	@Router      /v1/admin/roles [post]
func (h *AdminRoleHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Code == "" || req.Name == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "角色代码和名称不能为空")
		return
	}
	role, err := h.svc.Create(r.Context(), req.Code, req.Name, req.Description)
	if err != nil {
		status, code, message := roleHTTPError(err, "创建失败")
		api.Error(w, status, code, message)
		return
	}
	api.Success(w, toRoleResponse(role))
}

type updateRoleRequest struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Update godoc
//
//	@Summary     更新平台角色
//	@Tags        admin-roles
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string           true "角色ID"
//	@Param       body body updateRoleRequest true "角色信息"
//	@Success     200 {object} api.Response{data=roleResponse}
//	@Failure     400 {object} api.Response "角色代码为空、含 CSV 特殊字符或为保留代码"
//	@Failure     404 {object} api.Response "角色不存在"
//	@Failure     409 {object} api.Response "角色代码已存在或存在同名平台授权规则"
//	@Failure     500 {object} api.Response "数据库失败或提交后权限刷新失败"
//	@Router      /v1/admin/roles/{id} [put]
func (h *AdminRoleHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req updateRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Code == "" || req.Name == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "角色代码和名称不能为空")
		return
	}
	role, err := h.svc.Update(r.Context(), id, req.Code, req.Name, req.Description)
	if err != nil {
		status, code, message := roleHTTPError(err, "更新失败")
		api.Error(w, status, code, message)
		return
	}
	api.Success(w, toRoleResponse(role))
}

// Delete godoc
//
//	@Summary     删除平台角色
//	@Tags        admin-roles
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "角色ID"
//	@Success     200 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/roles/{id} [delete]
func (h *AdminRoleHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "角色不存在")
		return
	}
	api.Success(w, nil)
}

func roleHTTPError(err error, fallback string) (int, string, string) {
	if errors.Is(err, model.ErrInvalidRoleCode) || errors.Is(err, model.ErrReservedRoleCode) {
		return http.StatusBadRequest, api.CodeInvalidRequest, err.Error()
	}
	if errors.Is(err, model.ErrRoleCodeConflict) {
		// 冲突原因可能包装了底层驱动的原始错误，只返回稳定文案，避免泄漏 SQL 细节。
		return http.StatusConflict, api.CodeConflict, model.ErrRoleCodeConflict.Error()
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return http.StatusNotFound, api.CodeNotFound, "角色不存在"
	}
	if errors.Is(err, service.ErrRolePolicyReload) {
		return http.StatusInternalServerError, api.CodeInternal, err.Error()
	}
	return http.StatusInternalServerError, api.CodeInternal, fallback
}
