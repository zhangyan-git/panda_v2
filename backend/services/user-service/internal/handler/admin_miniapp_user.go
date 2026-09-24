package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// AdminMiniappUserHandler 后台的小程序用户管理。
//
// 与 MiniappUserHandler 是两回事：那个是 C 端用户看自己的资料（身份来自
// requireConsumer 的 realm 判定），这个是平台管理员看别人（身份来自 permMW 的
// 权限码判定）。两者除了都读写 users 表之外没有共同点，合并只会让两套判定
// 混在一个文件里。
type AdminMiniappUserHandler struct {
	svc *service.AdminMiniappUserService
}

func NewAdminMiniappUserHandler(svc *service.AdminMiniappUserService) *AdminMiniappUserHandler {
	return &AdminMiniappUserHandler{svc: svc}
}

// adminMiniappUserResponse 是后台列表与详情共用的账号字段。
//
// 比 C 端的 miniappUserResponse 多出 status / registerSource / 最后登录信息：
// 那些正是后台看这个列表的理由（谁被禁过、从哪个入口注册、最近有没有登录），
// 而在 C 端是审计信息、不该给自己看。
type adminMiniappUserResponse struct {
	ID             string `json:"id"`
	Phone          string `json:"phone"`
	Nickname       string `json:"nickname"`
	AvatarURL      string `json:"avatarUrl"`
	Gender         string `json:"gender"`
	Birthday       string `json:"birthday"`
	RegionCode     string `json:"regionCode"`
	RegionName     string `json:"regionName"`
	Status         string `json:"status"`
	RegisterSource string `json:"registerSource"`
	LastLoginAt    string `json:"lastLoginAt"`
	LastLoginIP    string `json:"lastLoginIp"`
	LoginCount     int    `json:"loginCount"`
	CreatedAt      string `json:"createdAt"`
}

type adminWechatIdentityResponse struct {
	AppType     string `json:"appType"`
	OpenID      string `json:"openId"`
	UnionID     string `json:"unionId"`
	LastLoginAt string `json:"lastLoginAt"`
	CreatedAt   string `json:"createdAt"`
}

type adminLoginEventResponse struct {
	LoginType  string `json:"loginType"`
	Identifier string `json:"identifier"`
	Success    bool   `json:"success"`
	FailReason string `json:"failReason"`
	IP         string `json:"ip"`
	UserAgent  string `json:"userAgent"`
	CreatedAt  string `json:"createdAt"`
}

// adminMiniappUserDetailResponse 内嵌账号字段，JSON 里是平铺的，前端拿到的
// 结构和列表行一样，只多了下面三项。
type adminMiniappUserDetailResponse struct {
	adminMiniappUserResponse
	WechatIdentities []adminWechatIdentityResponse `json:"wechatIdentities"`
	ActiveSessions   int64                         `json:"activeSessions"`
	RecentLogins     []adminLoginEventResponse     `json:"recentLogins"`
}

// formatTimePtr 可空时间统一成空串，与 dto.go 里生日的处理一致：前端只需判空。
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func toAdminMiniappUserResponse(u *model.User) adminMiniappUserResponse {
	birthday := ""
	if u.Birthday != nil {
		birthday = u.Birthday.Format("2006-01-02")
	}
	return adminMiniappUserResponse{
		ID:             u.ID,
		Phone:          u.Phone,
		Nickname:       u.Nickname,
		AvatarURL:      u.AvatarURL,
		Gender:         u.Gender,
		Birthday:       birthday,
		RegionCode:     u.RegionCode,
		RegionName:     u.RegionName,
		Status:         u.Status,
		RegisterSource: u.RegisterSource,
		LastLoginAt:    formatTimePtr(u.LastLoginAt),
		LastLoginIP:    u.LastLoginIP,
		LoginCount:     u.LoginCount,
		CreatedAt:      u.CreatedAt.Format(time.RFC3339),
	}
}

// List godoc
//
//	@Summary     获取小程序用户列表（服务端分页）
//	@Tags        admin-miniapp-users
//	@Produce     json
//	@Security    BearerAuth
//	@Param       page     query int    false "页码，从 1 开始，默认 1"
//	@Param       pageSize query int    false "每页条数，1..200，默认 20"
//	@Param       status   query string false "状态筛选：active/disabled/deleted，留空为全部"
//	@Param       keyword  query string false "关键词：手机号前缀或昵称片段"
//	@Success     200 {object} api.Response{data=api.PageResponse{items=[]adminMiniappUserResponse}}
//	@Failure     400 {object} api.Response
//	@Router      /v1/admin/miniapp-users [get]
func (h *AdminMiniappUserHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, pageSize, ok, msg := api.ParsePage(q.Get("page"), q.Get("pageSize"), api.MaxPageSize)
	if !ok {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, msg)
		return
	}
	users, total, err := h.svc.List(r.Context(), page, pageSize, q.Get("status"), q.Get("keyword"))
	switch {
	case errors.Is(err, service.ErrUserStatusFilterInvalid), errors.Is(err, service.ErrUserKeywordTooLong):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	case err != nil:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}
	resp := make([]adminMiniappUserResponse, len(users))
	for i, u := range users {
		resp[i] = toAdminMiniappUserResponse(u)
	}
	api.Success(w, api.PageResponse{Items: resp, Total: total, Page: page, PageSize: pageSize})
}

// Get godoc
//
//	@Summary     获取小程序用户详情
//	@Tags        admin-miniapp-users
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id path string true "用户ID"
//	@Success     200 {object} api.Response{data=adminMiniappUserDetailResponse}
//	@Failure     404 {object} api.Response
//	@Router      /v1/admin/miniapp-users/{id} [get]
func (h *AdminMiniappUserHandler) Get(w http.ResponseWriter, r *http.Request) {
	detail, err := h.svc.Detail(r.Context(), pathVar(r, "id"))
	switch {
	case errors.Is(err, service.ErrUserNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, service.ErrUserNotFound.Error())
		return
	case err != nil:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "服务内部错误")
		return
	}

	identities := make([]adminWechatIdentityResponse, len(detail.WechatIdentities))
	for i, ident := range detail.WechatIdentities {
		identities[i] = adminWechatIdentityResponse{
			AppType:     ident.AppType,
			OpenID:      ident.OpenID,
			UnionID:     ident.UnionID,
			LastLoginAt: formatTimePtr(ident.LastLoginAt),
			CreatedAt:   ident.CreatedAt.Format(time.RFC3339),
		}
	}
	logins := make([]adminLoginEventResponse, len(detail.LoginEvents))
	for i, e := range detail.LoginEvents {
		logins[i] = adminLoginEventResponse{
			LoginType:  e.LoginType,
			Identifier: e.Identifier,
			Success:    e.Success,
			FailReason: e.FailReason,
			IP:         e.IP,
			UserAgent:  e.UserAgent,
			CreatedAt:  e.CreatedAt.Format(time.RFC3339),
		}
	}
	api.Success(w, adminMiniappUserDetailResponse{
		adminMiniappUserResponse: toAdminMiniappUserResponse(detail.User),
		WechatIdentities:         identities,
		ActiveSessions:           detail.ActiveSessions,
		RecentLogins:             logins,
	})
}

type updateMiniappUserStatusRequest struct {
	Status string `json:"status"` // active | disabled
}

// updateMiniappUserStatusResponse 把连带撤销的会话数回给前端，好让提示写得出
// 「已禁用，同时把 3 个登录态踢下线」——不给这个数字的话，界面只能说「成功」，
// 而管理员真正想知道的是那个人现在还能不能用。
type updateMiniappUserStatusResponse struct {
	RevokedSessions int64 `json:"revokedSessions"`
}

// UpdateStatus godoc
//
//	@Summary     启用/禁用小程序用户
//	@Description 禁用会在同一事务里撤销该用户全部有效会话；已注销（deleted）的账号不可修改
//	@Tags        admin-miniapp-users
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       id   path string                         true "用户ID"
//	@Param       body body updateMiniappUserStatusRequest true "状态"
//	@Success     200 {object} api.Response{data=updateMiniappUserStatusResponse}
//	@Failure     400 {object} api.Response
//	@Failure     404 {object} api.Response
//	@Failure     409 {object} api.Response
//	@Router      /v1/admin/miniapp-users/{id}/status [patch]
func (h *AdminMiniappUserHandler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	var req updateMiniappUserStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	revoked, err := h.svc.UpdateStatus(r.Context(), pathVar(r, "id"), req.Status)
	switch {
	case errors.Is(err, service.ErrUserStatusInvalid):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
		return
	case errors.Is(err, model.ErrUserDeleted):
		// 409 而不是 400：请求本身没写错，是目标账号的状态不允许这次变更。
		api.Error(w, http.StatusConflict, api.CodeConflict, model.ErrUserDeleted.Error())
		return
	case errors.Is(err, service.ErrUserNotFound):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, service.ErrUserNotFound.Error())
		return
	case err != nil:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "操作失败")
		return
	}
	api.Success(w, updateMiniappUserStatusResponse{RevokedSessions: revoked})
}
