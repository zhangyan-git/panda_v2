package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

// MiniappAuthHandler 小程序登录、登录态与验证码接口。
type MiniappAuthHandler struct {
	authSvc *service.MiniappAuthService
}

func NewMiniappAuthHandler(authSvc *service.MiniappAuthService) *MiniappAuthHandler {
	return &MiniappAuthHandler{authSvc: authSvc}
}

// miniappLoginRequest 三种登录方式共用一个请求体，由 type 决定读哪些字段：
// 微信一键登录读 code，微信手机号快捷登录读 phoneCode（可带 code），
// 短信验证码登录读 phone + smsCode。
type miniappLoginRequest struct {
	Type      string `json:"type"`
	Code      string `json:"code"`
	PhoneCode string `json:"phoneCode"`
	Phone     string `json:"phone"`
	SmsCode   string `json:"smsCode"`
}

type miniappTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

type miniappLoginResponse struct {
	AccessToken  string              `json:"accessToken"`
	RefreshToken string              `json:"refreshToken"`
	User         miniappUserResponse `json:"user"`
}

// Login godoc
//
//	@Summary     小程序登录（微信一键登录 / 微信手机号快捷登录 / 手机号短信验证码）
//	@Description type 取值：wechat_miniapp 传 code；wechat_phone 传 phoneCode，可另传 code 顺带绑定微信；sms_code 传 phone 与 smsCode。账号不存在时自动注册。
//	@Tags        miniapp-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body miniappLoginRequest true "登录参数"
//	@Success     200 {object} api.Response{data=miniappLoginResponse}
//	@Failure     400 {object} api.Response "登录方式不支持、凭据不完整或手机号格式不对"
//	@Failure     401 {object} api.Response "微信授权失败或验证码错误"
//	@Failure     403 {object} api.Response "账号已禁用或已注销"
//	@Failure     409 {object} api.Response "手机号与微信分属不同账号"
//	@Failure     429 {object} api.Response "验证码错误次数过多"
//	@Failure     503 {object} api.Response "微信或短信通道不可用"
//	@Router      /v1/miniapp/auth/login [post]
func (h *MiniappAuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req miniappLoginRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	result, err := h.authSvc.Login(r.Context(), service.MiniappLoginInput{
		Type:      req.Type,
		Code:      req.Code,
		PhoneCode: req.PhoneCode,
		Phone:     req.Phone,
		SmsCode:   req.SmsCode,
	}, loginIP(r), r.UserAgent())
	if err != nil {
		writeMiniappError(w, err, "登录失败")
		return
	}
	api.Success(w, miniappLoginResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		User:         toMiniappUserResponse(result.User),
	})
}

// sendSMSRequest purpose 取值 login / bind_phone，默认 login：
// 「我是老用户，登录」比「我要绑定手机号」常见得多，让调用方少写一个字段。
type sendSMSRequest struct {
	Phone   string `json:"phone"`
	Purpose string `json:"purpose"`
}

// SendSMS godoc
//
//	@Summary     发送短信验证码（同号 60 秒内只能发一条）
//	@Tags        miniapp-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body sendSMSRequest true "手机号与用途（login / bind_phone，默认 login）"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Failure     429 {object} api.Response "重发间隔未到，Retry-After 为剩余秒数"
//	@Failure     503 {object} api.Response "短信通道不可用"
//	@Router      /v1/miniapp/auth/sms [post]
func (h *MiniappAuthHandler) SendSMS(w http.ResponseWriter, r *http.Request) {
	var req sendSMSRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	purpose := req.Purpose
	if purpose == "" {
		purpose = model.SMSPurposeLogin
	}
	if err := h.authSvc.SendSMSCode(r.Context(), req.Phone, purpose); err != nil {
		writeMiniappError(w, err, "验证码发送失败")
		return
	}
	// 响应体里只有成功，没有验证码。验证码只经由短信通道到用户手机上——
	// 从这里回显等于任何能调用这个接口的人都能登录任意手机号。
	api.Success(w, nil)
}

type miniappRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// Refresh godoc
//
//	@Summary     刷新登录态（refresh token 轮换，旧的立即作废）
//	@Tags        miniapp-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body miniappRefreshRequest true "refresh token"
//	@Success     200 {object} api.Response{data=miniappTokenResponse}
//	@Failure     400 {object} api.Response
//	@Failure     401 {object} api.Response "refresh token 无效、过期或已被使用过"
//	@Failure     403 {object} api.Response "账号已禁用或已注销"
//	@Router      /v1/miniapp/auth/refresh [post]
func (h *MiniappAuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req miniappRefreshRequest
	if err := decodeJSON(r, &req); err != nil || req.RefreshToken == "" {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	result, err := h.authSvc.Refresh(r.Context(), req.RefreshToken, loginIP(r), r.UserAgent())
	if err != nil {
		writeMiniappError(w, err, "刷新失败")
		return
	}
	api.Success(w, miniappTokenResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
	})
}

// Logout godoc
//
//	@Summary     退出登录（撤销这枚 refresh token 对应的会话）
//	@Description 无需 access token：令牌过期时最需要退出，要求它有效反而让最需要的时候用不了。对未知令牌一律返回成功。
//	@Tags        miniapp-auth
//	@Accept      json
//	@Produce     json
//	@Param       body body miniappRefreshRequest true "refresh token"
//	@Success     200 {object} api.Response
//	@Failure     400 {object} api.Response
//	@Router      /v1/miniapp/auth/logout [post]
func (h *MiniappAuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	var req miniappRefreshRequest
	if err := decodeJSON(r, &req); err != nil {
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, "请求格式错误")
		return
	}
	if err := h.authSvc.Logout(r.Context(), req.RefreshToken); err != nil {
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, "退出失败")
		return
	}
	api.Success(w, nil)
}

// writeMiniappError 把 C 端登录链路的业务错误映射到状态码。
//
// 分界是：400 请求本身写错了；401 凭据不对；403 凭据对但账号不能用；
// 409 与另一个账号冲突；429 现在不行，等会儿再来；503 我们这边或上游不可用。
// 每一条都对应客户端一个不同的动作，所以不能合并成 500。
func writeMiniappError(w http.ResponseWriter, err error, internalMsg string) {
	switch {
	case errors.Is(err, service.ErrLoginTypeUnsupported),
		errors.Is(err, service.ErrLoginCredentialMissing),
		errors.Is(err, service.ErrInvalidPhone),
		errors.Is(err, service.ErrSmsPurposeInvalid),
		errors.Is(err, service.ErrNicknameInvalid),
		errors.Is(err, service.ErrGenderInvalid),
		errors.Is(err, service.ErrBirthdayInvalid),
		errors.Is(err, service.ErrAvatarInvalid),
		errors.Is(err, service.ErrRegionInvalid):
		api.Error(w, http.StatusBadRequest, api.CodeInvalidRequest, err.Error())
	case errors.Is(err, service.ErrSmsSendTooFrequent):
		// 带上 Retry-After，客户端就能显示「60 秒后重试」而不是让用户干等着猜。
		w.Header().Set("Retry-After", strconv.Itoa(int(service.SmsResendInterval.Seconds())))
		api.Error(w, http.StatusTooManyRequests, api.CodeTooManyRequests, err.Error())
	case errors.Is(err, service.ErrSmsCodeTooManyAttempts):
		api.Error(w, http.StatusTooManyRequests, api.CodeTooManyRequests, err.Error())
	case errors.Is(err, service.ErrSmsCodeInvalid),
		errors.Is(err, service.ErrWechatAuthFailed),
		errors.Is(err, service.ErrRefreshTokenInvalid),
		errors.Is(err, service.ErrRefreshTokenReused):
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, err.Error())
	case errors.Is(err, service.ErrConsumerDisabled),
		errors.Is(err, service.ErrConsumerDeleted):
		api.Error(w, http.StatusForbidden, api.CodeForbidden, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		// 令牌有效但用户行没了（注销走了硬删、或者库被手工清过）。这不是「没登录」，
		// 而是「登录的那个人不存在了」——对客户端来说处理方式一样：清掉本地令牌重登。
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "登录已失效，请重新登录")
	// 冲突这三条是仅有的「哨兵外面还裹着底层错误」的情况：repository 用 %w 把
	// pgconn 的错误链在哨兵后面（见 repository/user.go 的 userWriteError），
	// 原样回 err.Error() 会把表名和约束名（"users_phone_key"、SQLSTATE 23505）
	// 一起发给客户端。所以这三条回哨兵自己的文案——它本来就是写给用户看的那句。
	case errors.Is(err, model.ErrPhoneTaken):
		api.Error(w, http.StatusConflict, api.CodeConflict, model.ErrPhoneTaken.Error())
	case errors.Is(err, model.ErrWechatIdentityTaken):
		api.Error(w, http.StatusConflict, api.CodeConflict, model.ErrWechatIdentityTaken.Error())
	case errors.Is(err, service.ErrAccountConflict):
		api.Error(w, http.StatusConflict, api.CodeConflict, service.ErrAccountConflict.Error())
	case errors.Is(err, service.ErrWechatUnavailable),
		errors.Is(err, service.ErrSmsUnavailable):
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, err.Error())
	default:
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, internalMsg)
	}
}
