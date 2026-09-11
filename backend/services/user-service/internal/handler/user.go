package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminUserHandler 平台管理员用户管理接口
type AdminUserHandler struct {
	svc *service.AdminUserService
}

func NewAdminUserHandler(svc *service.AdminUserService) *AdminUserHandler {
	return &AdminUserHandler{svc: svc}
}

type adminUserResponse struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

func toAdminUserResponse(u *model.AdminUser) adminUserResponse {
	return adminUserResponse{
		ID:        u.ID,
		Username:  u.Username,
		Name:      u.Name,
		Email:     u.Email,
		Status:    u.Status,
		CreatedAt: u.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

// List godoc
//
//	@Summary     获取所有平台管理员
//	@Tags        admin-users
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=[]adminUserResponse}
//	@Router      /v1/admin/users [get]
func (h *AdminUserHandler) List(w http.ResponseWriter, r *http.Request) {
	users, err := h.svc.List(r.Context())
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]adminUserResponse, len(users))
	for i, u := range users {
		resp[i] = toAdminUserResponse(u)
	}
	api.Success(w, resp)
}

// Get godoc
//
//	@Summary     获取单个平台管理员
//	@Tags        admin-users
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "用户ID"
//	@Success     200 {object} api.Response{data=adminUserResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/users/{id} [get]
func (h *AdminUserHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	u, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		api.Error(w, http.StatusNotFound, api.CodeNotFound, "用户不存在")
		return
	}
	api.Success(w, toAdminUserResponse(u))
}

type createAdminUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

// Create godoc
//
//	@Summary     创建平台管理员
//	@Tags        admin-users
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body createAdminUserRequest true "用户信息"
//	@Success     200 {object} api.Response{data=adminUserResponse}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/users [post]
func (h *AdminUserHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createAdminUserRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "用户名和密码不能为空")
		return
	}
	// name / email 在库里都是 NOT NULL，漏了校验就会变成一次 500「创建失败」，
	// 把「少填了一个字段」伪装成服务端故障。这里挡在写库之前。
	if req.Name == "" || req.Email == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "姓名和邮箱不能为空")
		return
	}
	u, err := h.svc.CreateUser(r.Context(), req.Username, req.Password, req.Name, req.Email)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "创建失败")
		return
	}
	api.Success(w, toAdminUserResponse(u))
}

type updateStatusRequest struct {
	Status string `json:"status"` // active | disabled
}

// UpdateStatus godoc
//
//	@Summary     启用/禁用平台管理员
//	@Tags        admin-users
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string              true "用户ID"
//	@Param       body body updateStatusRequest true "状态"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/users/{id}/status [patch]
func (h *AdminUserHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	id := pathVar(r, "id")
	var req updateStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if req.Status != "active" && req.Status != "disabled" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "status 只能为 active 或 disabled")
		return
	}
	if err := h.svc.UpdateStatus(r.Context(), id, req.Status); err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "操作失败")
		return
	}
	api.Success(w, nil)
}
