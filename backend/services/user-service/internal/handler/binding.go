package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

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
//
// 不分页：它是「这个用户当前有哪些角色」的集合，admin_auth 的 /users/me 也复用
// 这条链来填 roles 字段——分页会让登录后的身份信息缺角色。
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
