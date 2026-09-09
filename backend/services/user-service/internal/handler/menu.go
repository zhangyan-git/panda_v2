package handler

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminMenuHandler 后台菜单管理接口
type AdminMenuHandler struct {
	svc *service.AdminMenuService
}

func NewAdminMenuHandler(svc *service.AdminMenuService) *AdminMenuHandler {
	return &AdminMenuHandler{svc: svc}
}

// List godoc
//
//	@Summary     获取完整菜单树
//	@Tags        admin-menus
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=[]service.MenuNode}
//	@Router      /v1/admin/menus [get]
func (h *AdminMenuHandler) List(w http.ResponseWriter, r *http.Request) {
	tree, err := h.svc.ListTree(r.Context())
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	api.Success(w, tree)
}

// MeTree godoc
//
//	@Summary     获取当前用户可见的菜单树
//	@Tags        admin-menus
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=[]service.MenuNode}
//	@Router      /v1/admin/menus/me [get]
func (h *AdminMenuHandler) MeTree(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "未登录")
		return
	}
	tree, err := h.svc.TreeForIdentity(r.Context(), identity)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	api.Success(w, tree)
}

type menuRequest struct {
	ParentID string `json:"parentId"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Icon     string `json:"icon"`
	Sort     int    `json:"sort"`
}

// Create godoc
//
//	@Summary     创建菜单
//	@Tags        admin-menus
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body menuRequest true "菜单信息；parentId 为空表示顶级"
//	@Success     200 {object} api.Response{data=service.MenuNode}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/menus [post]
func (h *AdminMenuHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req menuRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	node, err := h.svc.Create(r.Context(), req.ParentID, req.Name, req.Path, req.Icon, req.Sort)
	if err != nil {
		writeMenuError(w, err, "创建失败")
		return
	}
	api.Success(w, node)
}

// Update godoc
//
//	@Summary     更新菜单
//	@Tags        admin-menus
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string     true "菜单ID"
//	@Param       body body menuRequest true "菜单信息"
//	@Success     200 {object} api.Response{data=service.MenuNode}
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/menus/{id} [put]
func (h *AdminMenuHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req menuRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	node, err := h.svc.Update(r.Context(), id, req.ParentID, req.Name, req.Path, req.Icon, req.Sort)
	if err != nil {
		writeMenuError(w, err, "更新失败")
		return
	}
	api.Success(w, node)
}

// Delete godoc
//
//	@Summary     删除菜单
//	@Tags        admin-menus
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "菜单ID"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/menus/{id} [delete]
func (h *AdminMenuHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeMenuError(w, err, "删除失败")
		return
	}
	api.Success(w, nil)
}

// RoleMenus godoc
//
//	@Summary     获取角色绑定的菜单 ID 列表
//	@Tags        admin-menus
//	@Produce     json
//	@Security    BearerAuth
//	@Param       roleId path string true "角色ID"
//	@Success     200 {object} api.Response{data=[]string}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/roles/{roleId}/menus [get]
func (h *AdminMenuHandler) RoleMenus(w http.ResponseWriter, r *http.Request) {
	roleID := pathVar(r, "roleId")
	ids, err := h.svc.RoleMenuIDs(r.Context(), roleID)
	if err != nil {
		writeMenuError(w, err, "服务内部错误")
		return
	}
	api.Success(w, ids)
}

type assignRoleMenusRequest struct {
	MenuIDs []string `json:"menuIds"`
}

// AssignRoleMenus godoc
//
//	@Summary     为角色分配菜单（全量覆盖）
//	@Tags        admin-menus
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       roleId path string                true "角色ID"
//	@Param       body   body assignRoleMenusRequest true "菜单ID列表"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/roles/{roleId}/menus [post]
func (h *AdminMenuHandler) AssignRoleMenus(w http.ResponseWriter, r *http.Request) {
	roleID := pathVar(r, "roleId")
	var req assignRoleMenusRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if err := h.svc.AssignMenusToRole(r.Context(), roleID, req.MenuIDs); err != nil {
		writeMenuError(w, err, "分配失败")
		return
	}
	api.Success(w, nil)
}

// writeMenuError 将菜单业务错误映射到 HTTP 状态码
func writeMenuError(w http.ResponseWriter, err error, internalMsg string) {
	switch {
	case errors.Is(err, service.ErrMenuNameRequired),
		errors.Is(err, service.ErrMenuParentCycle),
		errors.Is(err, service.ErrMenuHasChildren):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "菜单或角色不存在")
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, internalMsg)
	}
}
