package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

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
		CreatedAt: u.CreatedAt.Format(time.RFC3339),
	}
}

// List godoc
//
//	@Summary     获取平台管理员列表（服务端分页）
//	@Tags        admin-users
//	@Produce     json
//	@Security    BearerAuth
//	@Param       page     query int false "页码，从 1 开始，默认 1"
//	@Param       pageSize query int false "每页条数，1..200，默认 20"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]adminUserResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/users [get]
func (h *AdminUserHandler) List(w http.ResponseWriter, r *http.Request) {
	page, pageSize, ok, msg := api.ParsePage(r.URL.Query().Get("page"), r.URL.Query().Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	users, total, err := h.svc.List(r.Context(), page, pageSize)
	if err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]adminUserResponse, len(users))
	for i, u := range users {
		resp[i] = toAdminUserResponse(u)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
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
		status, code, msg := adminUserHTTPError(err, "创建失败")
		api.Error(w, status, code, msg)
		return
	}
	api.Success(w, toAdminUserResponse(u))
}

// adminUserHTTPError 把管理员账号链路的错误映射到状态码，做法与 roleHTTPError 一致：
// 能分支的走哨兵（409 冲突、404 不存在），其余归 500。
func adminUserHTTPError(err error, fallback string) (int, string, string) {
	switch {
	case errors.Is(err, model.ErrAdminUsernameTaken):
		// 冲突原因包装了底层驱动错误，只回哨兵自己的文案：err.Error() 里带着
		// 约束名和 SQLSTATE 23505，那是给运维看的，不是给后台用户看的。
		return http.StatusConflict, api.CodeConflict, model.ErrAdminUsernameTaken.Error()
	case errors.Is(err, model.ErrAdminEmailTaken):
		return http.StatusConflict, api.CodeConflict, model.ErrAdminEmailTaken.Error()
	case errors.Is(err, pgx.ErrNoRows):
		return http.StatusNotFound, api.CodeNotFound, "用户不存在"
	default:
		return http.StatusInternalServerError, api.CodeInternal, fallback
	}
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
		status, code, msg := adminUserHTTPError(err, "操作失败")
		api.Error(w, status, code, msg)
		return
	}
	api.Success(w, nil)
}
