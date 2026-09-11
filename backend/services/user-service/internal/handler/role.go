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
//	@Summary     获取所有平台角色
//	@Tags        admin-roles
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=[]roleResponse}
//	@Router      /v1/admin/roles [get]
func (h *AdminRoleHandler) List(w http.ResponseWriter, r *http.Request) {
	roles, err := h.svc.List(r.Context())
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]roleResponse, len(roles))
	for i, role := range roles {
		resp[i] = toRoleResponse(role)
	}
	api.Success(w, resp)
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

type AdminPermissionHandler struct {
	svc *service.AdminPermissionService
}

func NewAdminPermissionHandler(svc *service.AdminPermissionService) *AdminPermissionHandler {
	return &AdminPermissionHandler{svc: svc}
}

// List godoc
//
//	@Summary     获取所有平台权限
//	@Tags        admin-permissions
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=[]permResponse}
//	@Router      /v1/admin/permissions [get]
func (h *AdminPermissionHandler) List(w http.ResponseWriter, r *http.Request) {
	perms, err := h.svc.List(r.Context())
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

// ListByRole godoc
//
//	@Summary     获取角色已绑定的权限
//	@Tags        admin-permissions
//	@Produce     json
//	@Security    BearerAuth
//	@Param       roleId path string true "角色ID"
//	@Success     200 {object} api.Response{data=[]permResponse}
//	@Router      /v1/admin/roles/{roleId}/permissions [get]
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
type AdminBindingHandler struct {
	svc *service.AdminBindingService
}

func NewAdminBindingHandler(svc *service.AdminBindingService) *AdminBindingHandler {
	return &AdminBindingHandler{svc: svc}
}

type assignPermsRequest struct {
	PermissionIDs []string `json:"permissionIds"`
}

// AssignPermissions godoc
//
//	@Summary     整体替换角色权限（空列表清空）
//	@Tags        admin-bindings
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       roleId path string           true "角色ID"
//	@Param       body   body assignPermsRequest true "权限ID列表"
//	@Success     200 {object} api.Response
//	@Router      /v1/admin/roles/{roleId}/permissions [post]
func (h *AdminBindingHandler) AssignPermissions(w http.ResponseWriter, r *http.Request) {
	roleID := pathVar(r, "roleId")
	var req assignPermsRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if err := h.svc.AssignPermissionsToRole(r.Context(), roleID, req.PermissionIDs); err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "操作失败")
		return
	}
	api.Success(w, nil)
}

// RemovePermission godoc
//
//	@Summary     移除角色的某个权限
//	@Tags        admin-bindings
//	@Produce     json
//	@Security    BearerAuth
//	@Param       roleId       path string true "角色ID"
//	@Param       permissionId path string true "权限ID"
//	@Success     200 {object} api.Response
//	@Router      /v1/admin/roles/{roleId}/permissions/{permissionId} [delete]
func (h *AdminBindingHandler) RemovePermission(w http.ResponseWriter, r *http.Request) {
	roleID := pathVar(r, "roleId")
	permID := pathVar(r, "permissionId")
	if err := h.svc.RemovePermissionFromRole(r.Context(), roleID, permID); err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "操作失败")
		return
	}
	api.Success(w, nil)
}

type assignRolesRequest struct {
	RoleIDs []string `json:"roleIds"`
}

// AssignRoles godoc
//
//	@Summary     整体替换用户角色（空列表清空）
//	@Tags        admin-bindings
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       userId path string           true "用户ID"
//	@Param       body   body assignRolesRequest true "角色ID列表"
//	@Success     200 {object} api.Response
//	@Router      /v1/admin/users/{userId}/roles [post]
func (h *AdminBindingHandler) AssignRoles(w http.ResponseWriter, r *http.Request) {
	userID := pathVar(r, "userId")
	var req assignRolesRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if err := h.svc.AssignRolesToUser(r.Context(), userID, req.RoleIDs); err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "操作失败")
		return
	}
	api.Success(w, nil)
}

// RemoveRole godoc
//
//	@Summary     移除用户的某个角色
//	@Tags        admin-bindings
//	@Produce     json
//	@Security    BearerAuth
//	@Param       userId path string true "用户ID"
//	@Param       roleId path string true "角色ID"
//	@Success     200 {object} api.Response
//	@Router      /v1/admin/users/{userId}/roles/{roleId} [delete]
func (h *AdminBindingHandler) RemoveRole(w http.ResponseWriter, r *http.Request) {
	userID := pathVar(r, "userId")
	roleID := pathVar(r, "roleId")
	if err := h.svc.RemoveRoleFromUser(r.Context(), userID, roleID); err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "操作失败")
		return
	}
	api.Success(w, nil)
}

// ListRoles godoc
//
//	@Summary     查询用户已有的角色
//	@Tags        admin-bindings
//	@Produce     json
//	@Security    BearerAuth
//	@Param       userId path string true "用户ID"
//	@Success     200 {object} api.Response{data=[]roleResponse}
//	@Router      /v1/admin/users/{userId}/roles [get]
func (h *AdminBindingHandler) ListRoles(w http.ResponseWriter, r *http.Request) {
	userID := pathVar(r, "userId")
	roles, err := h.svc.ListRolesByUser(r.Context(), userID)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]roleResponse, len(roles))
	for i, role := range roles {
		resp[i] = toRoleResponse(role)
	}
	api.Success(w, resp)
}
