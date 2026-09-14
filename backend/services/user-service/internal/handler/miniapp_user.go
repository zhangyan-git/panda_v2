package handler

import (
	"net/http"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// MiniappUserHandler 是小程序用户的个人资料与账号绑定接口。
//
// 与 MiniappAuthHandler 分开而不是塞进一个 handler：登录接口全部无需身份、
// 靠限流和凭据本身防护，这里的每个接口都必须先过 requireConsumer。两者的
// 前置条件正好相反，混在一起迟早会漏掉某一个的身份校验。
type MiniappUserHandler struct {
	userSvc *service.MiniappUserService
}

func NewMiniappUserHandler(userSvc *service.MiniappUserService) *MiniappUserHandler {
	return &MiniappUserHandler{userSvc: userSvc}
}

// Me godoc
//
//	@Summary     获取当前小程序用户资料
//	@Tags        miniapp-user
//	@Produce     json
//	@Security    BearerAuth
//	@Success     200 {object} api.Response{data=miniappUserResponse}
//	@Failure     401 {object} api.Response "未登录或登录已失效"
//	@Failure     403 {object} api.Response "账号已禁用或已注销，或用的不是小程序令牌"
//	@Router      /v1/miniapp/users/me [get]
func (h *MiniappUserHandler) Me(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	user, err := h.userSvc.Profile(r.Context(), identity.UserID)
	if err != nil {
		writeMiniappError(w, err, "获取资料失败")
		return
	}
	api.Success(w, toMiniappUserResponse(user))
}

// updateMiniappProfileRequest 是资料的部分更新，字段全部用指针。
//
// 不用指针就分不清「没传这个字段」和「把它改成空值」——用户填错了昵称想清空
// 重填，服务端会把空串当成没传而原样保留旧值，这个 bug 只有用户自己能发现。
type updateMiniappProfileRequest struct {
	Nickname   *string `json:"nickname"`
	AvatarURL  *string `json:"avatarUrl"`
	Gender     *string `json:"gender"`
	Birthday   *string `json:"birthday"`
	RegionCode *string `json:"regionCode"`
	RegionName *string `json:"regionName"`
}

// UpdateMe godoc
//
//	@Summary     更新当前小程序用户资料（只改传了的字段）
//	@Description birthday 传空串表示清空；gender 取值 unknown / male / female。
//	@Tags        miniapp-user
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body updateMiniappProfileRequest true "要更新的字段，不传的字段保持原值"
//	@Success     200 {object} api.Response{data=miniappUserResponse}
//	@Failure     400 {object} api.Response "昵称、性别、生日、头像或地区格式不合法"
//	@Failure     401 {object} api.Response
//	@Failure     403 {object} api.Response
//	@Router      /v1/miniapp/users/me [patch]
func (h *MiniappUserHandler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	var req updateMiniappProfileRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	user, err := h.userSvc.UpdateProfile(r.Context(), identity.UserID, service.ProfileUpdate{
		Nickname:   req.Nickname,
		AvatarURL:  req.AvatarURL,
		Gender:     req.Gender,
		Birthday:   req.Birthday,
		RegionCode: req.RegionCode,
		RegionName: req.RegionName,
	})
	if err != nil {
		writeMiniappError(w, err, "更新资料失败")
		return
	}
	api.Success(w, toMiniappUserResponse(user))
}

type bindPhoneRequest struct {
	Phone string `json:"phone"`
	Code  string `json:"code"`
}

// BindPhone godoc
//
//	@Summary     绑定手机号到当前小程序账号
//	@Description 需要一条 purpose=bind_phone 的验证码，先调用 /v1/miniapp/auth/sms 获取。手机号已被其他账号占用时返回 409。
//	@Tags        miniapp-user
//	@Accept      json
//	@Produce     json
//	@Security    BearerAuth
//	@Param       body body bindPhoneRequest true "手机号与验证码"
//	@Success     200 {object} api.Response{data=miniappUserResponse}
//	@Failure     400 {object} api.Response "手机号格式不对或未填验证码"
//	@Failure     401 {object} api.Response "未登录或验证码错误"
//	@Failure     403 {object} api.Response
//	@Failure     409 {object} api.Response "该手机号已绑定其他账号"
//	@Failure     429 {object} api.Response "验证码错误次数过多"
//	@Router      /v1/miniapp/users/me/phone [post]
func (h *MiniappUserHandler) BindPhone(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireConsumer(w, r)
	if !ok {
		return
	}
	var req bindPhoneRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	user, err := h.userSvc.BindPhone(r.Context(), identity.UserID, req.Phone, req.Code)
	if err != nil {
		writeMiniappError(w, err, "绑定手机号失败")
		return
	}
	api.Success(w, toMiniappUserResponse(user))
}
