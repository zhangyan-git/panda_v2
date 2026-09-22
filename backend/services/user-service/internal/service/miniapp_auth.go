package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

const (
	// smsCodeTTL 是验证码的有效期。短到被翻出来也基本过期，长到用户来得及
	// 看完短信回来输。
	smsCodeTTL = 5 * time.Minute
	// SmsResendInterval 是同一手机号两次发送之间的最小间隔。它是短信费用的
	// 第一道闸，也是「拿别人手机号刷验证码」的第一道闸。导出是为了让 handler
	// 能把这个秒数写进 429 的 Retry-After——客户端要据此显示倒计时，
	// 而这个数字只能有一处定义。
	SmsResendInterval = 60 * time.Second
	// defaultConsumerNickname 是新注册账号的默认昵称。
	//
	// 不拿手机号打码当昵称：昵称会出现在商户侧的列表里，把手机号塞进去等于
	// 顺手把用户手机号泄露给商户；而且用户换绑手机号之后它就过期了。
	defaultConsumerNickname = "咖啡用户"
	// maxFailReasonLen 限制写进 user_login_events.fail_reason 的长度。
	// 失败原因来自错误链，可能很长；这条记录是给安全侧看的信号，
	// 不是错误日志的副本。
	maxFailReasonLen = 200
)

// cnMobilePattern 是大陆手机号。在发短信之前就拦掉，垃圾号就不会真的产生
// 一条短信账单。
var cnMobilePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// ValidCNMobile 报告 phone 是否是一个可下发短信的大陆手机号。
func ValidCNMobile(phone string) bool {
	return cnMobilePattern.MatchString(phone)
}

// C 端登录与登录态相关的错误。handler 按它们决定状态码。
var (
	// ErrLoginTypeUnsupported 不认识的登录方式。
	ErrLoginTypeUnsupported = errors.New("不支持的登录方式")
	// ErrLoginCredentialMissing 该登录方式需要的凭据没给全。
	ErrLoginCredentialMissing = errors.New("登录凭据不完整")
	// ErrInvalidPhone 手机号格式不对。
	ErrInvalidPhone = errors.New("手机号格式不正确")
	// ErrSmsPurposeInvalid 验证码用途不在允许的取值里。
	ErrSmsPurposeInvalid = errors.New("验证码用途不合法")
	// ErrSmsCodeInvalid 验证码不存在、已过期、用途不符或比对失败。
	// 这几种情况刻意合成一个错误：区分它们等于告诉尝试者「码是对的，只是
	// 用途不对」，而他没有理由知道这个。
	ErrSmsCodeInvalid = errors.New("验证码错误或已过期")
	// ErrSmsCodeTooManyAttempts 这条验证码的失败次数已用尽，必须重新获取。
	ErrSmsCodeTooManyAttempts = errors.New("验证码错误次数过多，请重新获取")
	// ErrSmsSendTooFrequent 重发间隔未到。
	ErrSmsSendTooFrequent = errors.New("验证码发送过于频繁，请稍后再试")
	// ErrConsumerDisabled 账号被禁用。
	ErrConsumerDisabled = errors.New("账号已禁用")
	// ErrConsumerDeleted 账号已注销。
	ErrConsumerDeleted = errors.New("账号已注销")
	// ErrAccountConflict 手机号所属的账号与微信所属的账号不是同一个。
	//
	// V2 首期不做自动合并：合并要把券、订单、余额跨账号搬过去，首期没有能安全
	// 做这件事的地方。所以这里给出一个确定的分支，把选择权交回用户。
	ErrAccountConflict = errors.New("该手机号已绑定其他微信，请用手机号验证码登录后再绑定微信")
	// ErrRefreshTokenInvalid 刷新令牌不可用（不存在、过期、域不对）。
	ErrRefreshTokenInvalid = errors.New("登录状态已失效，请重新登录")
	// ErrRefreshTokenReused 刷新令牌已被撤销却再次被使用，按泄露处理，
	// 该用户所有活着的会话已被一起撤销。
	ErrRefreshTokenReused = errors.New("检测到登录状态异常，已退出全部设备，请重新登录")
)

// MiniappLoginInput 是一次小程序登录请求。
//
// 三种登录方式共用同一个接口，因为它们的响应、限流、审计完全相同，只有取凭据
// 的方式不同。Type 决定读哪些字段，缺了对应字段就是 ErrLoginCredentialMissing。
type MiniappLoginInput struct {
	// Type 取值同 model.LoginType*。
	Type string
	// Code 是 wx.login 拿到的 js_code。wechat_miniapp 必填；wechat_phone 选填——
	// 带上了就在这次登录里顺手把微信身份绑上，少一次往返。
	Code string
	// PhoneCode 是 getPhoneNumber 拿到的 code，wechat_phone 必填。
	PhoneCode string
	// Phone / SmsCode 供 sms_code 使用。
	Phone   string
	SmsCode string
}

// MiniappLoginResult 是一次登录的结果。带上 User 是因为小程序登录成功后
// 几乎总要立刻渲染个人资料，分成两个请求只是多一次往返。
type MiniappLoginResult struct {
	AccessToken  string
	RefreshToken string
	User         *model.User
}

// MiniappAuthService 是小程序侧的登录、登录态与验证码。
type MiniappAuthService struct {
	users    repository.UserRepository
	sessions repository.UserSessionRepository
	smsCodes repository.SMSCodeRepository
	wechat   WechatGateway
	sms      SmsSender
	jwtSvc   *auth.Service
}

func NewMiniappAuthService(
	users repository.UserRepository,
	sessions repository.UserSessionRepository,
	smsCodes repository.SMSCodeRepository,
	wechat WechatGateway,
	sms SmsSender,
	jwtSvc *auth.Service,
) *MiniappAuthService {
	return &MiniappAuthService{
		users: users, sessions: sessions, smsCodes: smsCodes,
		wechat: wechat, sms: sms, jwtSvc: jwtSvc,
	}
}

// Login 按 Type 分派到三种登录方式。
func (s *MiniappAuthService) Login(ctx context.Context, in MiniappLoginInput, ip, userAgent string) (*MiniappLoginResult, error) {
	switch in.Type {
	case model.LoginTypeWechatMiniapp:
		return s.loginByWechat(ctx, in.Code, ip, userAgent)
	case model.LoginTypeWechatPhone:
		return s.loginByWechatPhone(ctx, in.PhoneCode, in.Code, ip, userAgent)
	case model.LoginTypeSMSCode:
		return s.loginBySms(ctx, in.Phone, in.SmsCode, ip, userAgent)
	default:
		return nil, ErrLoginTypeUnsupported
	}
}

// SendSMSCode 下发一条验证码，并施加同号重发间隔。
//
// purpose 由调用方指定而不是由服务推断：登录和换绑手机号是两条独立的链路，
// 一条链路的码不该能在另一条上用。
func (s *MiniappAuthService) SendSMSCode(ctx context.Context, phone, purpose string) error {
	if !ValidCNMobile(phone) {
		return ErrInvalidPhone
	}
	if !model.ValidSMSPurpose(purpose) {
		return ErrSmsPurposeInvalid
	}
	code, err := randomSMSCode()
	if err != nil {
		return err
	}
	// 验证码用 bcrypt 而不是裸哈希：它只有 6 位，空间是 10^6，对着一张导出表
	// 把候选全算一遍是瞬间的事。刷新令牌的选择相反，理由见 model.UserSession。
	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now()
	rec := &model.UserSMSCode{
		Phone: phone, Purpose: purpose, CodeHash: string(hash),
		ExpiresAt: now.Add(smsCodeTTL), CreatedAt: now,
	}
	// 频控在库里的 ON CONFLICT ... WHERE 上，不在这里先读后判。
	if err := s.smsCodes.UpsertSMSCode(ctx, rec, now.Add(-SmsResendInterval)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSmsSendTooFrequent
		}
		return err
	}
	if err := s.sms.Send(ctx, phone, code); err != nil {
		// 发不出去就把这行删掉。留着它只会让用户在 60 秒内连重试都被拒，
		// 而他从头到尾没收到过任何东西。
		_ = s.smsCodes.DeleteSMSCode(ctx, phone)
		return err
	}
	return nil
}

// Refresh 用刷新令牌换一对新令牌，并轮换掉旧的。
//
// 轮换而不是复用：一枚 refresh token 只在一次刷新里有效，之后立刻作废。
// 这样「已经作废的令牌又被拿来用」就成了盗用的唯一信号——如果允许重复使用，
// 攻击者拿着偷来的令牌刷新，正主那边不会有任何异常。
func (s *MiniappAuthService) Refresh(ctx context.Context, refreshToken, ip, userAgent string) (*MiniappLoginResult, error) {
	claims, err := s.jwtSvc.ParseRefresh(refreshToken)
	if err != nil {
		return nil, ErrRefreshTokenInvalid
	}
	// 只管 C 端。平台/商户的刷新令牌走它们自己的接口，令牌本身也换不出库里的
	// 会话行；显式判一次是为了让这个接口的适用范围写在代码里，而不是靠
	// 「目录下碰巧没有对应记录」这种隐含前提。
	if claims.Realm != auth.RealmConsumer {
		return nil, ErrRefreshTokenInvalid
	}
	session, err := s.sessions.FindSessionByHash(ctx, hashRefreshToken(refreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRefreshTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	// 令牌声称的身份和库里那行记录对不上，说明这枚令牌的载荷被换过或拼错了。
	if session.UserID != claims.UserID {
		return nil, ErrRefreshTokenInvalid
	}
	// 已经撤销过的令牌又被拿来用。这里必须看撤销原因，不能一概当成盗用：
	//
	//   rotated —— 这枚是被新令牌换掉的，换出来的那枚只在合法客户端手里，
	//              所以「旧的又出现」确实只有一个解释：有人拿了被换下来的那枚。
	//              这是盗用信号，把该用户所有活着的会话一起撤销。
	//   logout  —— 用户自己退出登录（或账号被禁用时的连带撤销）。他退出后
	//              客户端里残留的旧令牌再发一次刷新，是完全正常的时序，不是
	//              盗用。按盗用处理会顺手把他在平板/网页上还开着的会话也撤掉，
	//              变成「手机退出登录，平板被踢下线」。
	//
	// 其余原因（含已判过的 reuse_detected）同理：没有新证据，就不再连坐一次。
	if session.RevokedAt != nil {
		if session.RevokeReason == model.RevokeReasonRotated {
			_ = s.sessions.RevokeUserSessions(ctx, session.UserID, model.RevokeReasonReuseDetected)
			return nil, ErrRefreshTokenReused
		}
		return nil, ErrRefreshTokenInvalid
	}
	if time.Now().After(session.ExpiresAt) {
		return nil, ErrRefreshTokenInvalid
	}
	user, err := s.users.FindByID(ctx, session.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRefreshTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	// 账号状态在刷新时重新读一次，而不是信 token 里的快照。这是「禁用/注销」
	// 那道闸的第二道锁：第一道是写状态时撤销所有会话，但撤销可能失败，
	// 而这里一旦读到非 active，令牌再新也换不出东西来。
	if err := ensureConsumerActive(user); err != nil {
		_ = s.sessions.RevokeUserSessions(ctx, user.ID, model.RevokeReasonLogout)
		return nil, err
	}
	// 记下这枚令牌最后一次被使用的时刻，再撤销它。撤销记录「什么时候没的」，
	// 这一行记录「最后一次被拿来用是什么时候」——两者相差很大时很可疑。
	_ = s.sessions.TouchSessionUsed(ctx, session.ID)
	// 抢占式轮换：只有一条请求能把 revoked_at 从 NULL 改掉。并发刷新同一枚
	// 令牌时，另一方会拿到 ErrNoRows。正常的客户端不会拿同一个 refresh token
	// 发两次，所以这里也按盗用处理，而不是放它过去。
	if err := s.sessions.RevokeSession(ctx, session.ID, model.RevokeReasonRotated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_ = s.sessions.RevokeUserSessions(ctx, session.UserID, model.RevokeReasonReuseDetected)
			return nil, ErrRefreshTokenReused
		}
		return nil, err
	}
	return s.issue(ctx, user, ip, userAgent)
}

// Logout 撤销与这枚刷新令牌对应的那一个会话。
//
// 不带鉴权中间件，也不需要 access token：退出登录发生在 access token 刚好过期的
// 时候最需要，要求它还有效等于在最需要的时候用不了。凭据就是这枚刷新令牌本身，
// 拿到它的人本来就能用它换出新令牌，「能撤销它」不构成额外的权力。因此对
// 未知/已失效的令牌一律返回成功，不给出「这枚令牌是否存在」的区分。
func (s *MiniappAuthService) Logout(ctx context.Context, refreshToken string) error {
	if strings.TrimSpace(refreshToken) == "" {
		return nil
	}
	claims, err := s.jwtSvc.ParseRefresh(refreshToken)
	if err != nil || claims.Realm != auth.RealmConsumer {
		return nil
	}
	session, err := s.sessions.FindSessionByHash(ctx, hashRefreshToken(refreshToken))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if session.UserID != claims.UserID || session.RevokedAt != nil {
		return nil
	}
	if err := s.sessions.RevokeSession(ctx, session.ID, model.RevokeReasonLogout); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

func (s *MiniappAuthService) loginByWechat(ctx context.Context, code, ip, userAgent string) (*MiniappLoginResult, error) {
	if strings.TrimSpace(code) == "" {
		return nil, ErrLoginCredentialMissing
	}
	openID, unionID, err := s.wechat.Code2Session(ctx, code)
	if err != nil {
		s.recordLogin(ctx, "", model.LoginTypeWechatMiniapp, "", false, err.Error(), ip, userAgent)
		return nil, err
	}
	user, err := s.ensureWechatUser(ctx, openID, unionID)
	if err != nil {
		s.recordLogin(ctx, "", model.LoginTypeWechatMiniapp, openID, false, err.Error(), ip, userAgent)
		return nil, err
	}
	if err := ensureConsumerActive(user); err != nil {
		s.recordLogin(ctx, user.ID, model.LoginTypeWechatMiniapp, openID, false, err.Error(), ip, userAgent)
		return nil, err
	}
	_ = s.users.TouchWechatLogin(ctx, model.WechatAppMiniapp, openID)
	_ = s.users.TouchLogin(ctx, user.ID, ip)
	s.recordLogin(ctx, user.ID, model.LoginTypeWechatMiniapp, openID, true, "", ip, userAgent)
	return s.issue(ctx, user, ip, userAgent)
}

func (s *MiniappAuthService) loginByWechatPhone(ctx context.Context, phoneCode, jsCode, ip, userAgent string) (*MiniappLoginResult, error) {
	if strings.TrimSpace(phoneCode) == "" {
		return nil, ErrLoginCredentialMissing
	}
	phone, err := s.wechat.ResolvePhone(ctx, phoneCode)
	if err != nil {
		s.recordLogin(ctx, "", model.LoginTypeWechatPhone, "", false, err.Error(), ip, userAgent)
		return nil, err
	}
	// js_code 不是必须的：getPhoneNumber 已经证明了手机号归属，微信授权只是
	// 顺带把身份绑上。没给就只按手机号登录。
	var openID, unionID string
	if strings.TrimSpace(jsCode) != "" {
		openID, unionID, err = s.wechat.Code2Session(ctx, jsCode)
		if err != nil {
			s.recordLogin(ctx, "", model.LoginTypeWechatPhone, maskPhoneNumber(phone), false, err.Error(), ip, userAgent)
			return nil, err
		}
	}
	user, err := s.ensurePhoneUser(ctx, phone, model.LoginTypeWechatPhone)
	if err != nil {
		s.recordLogin(ctx, "", model.LoginTypeWechatPhone, maskPhoneNumber(phone), false, err.Error(), ip, userAgent)
		return nil, err
	}
	if err := ensureConsumerActive(user); err != nil {
		s.recordLogin(ctx, user.ID, model.LoginTypeWechatPhone, maskPhoneNumber(phone), false, err.Error(), ip, userAgent)
		return nil, err
	}
	if openID != "" {
		if err := s.linkWechatIdentity(ctx, user, openID, unionID); err != nil {
			s.recordLogin(ctx, user.ID, model.LoginTypeWechatPhone, maskPhoneNumber(phone), false, err.Error(), ip, userAgent)
			return nil, err
		}
		_ = s.users.TouchWechatLogin(ctx, model.WechatAppMiniapp, openID)
	}
	_ = s.users.TouchLogin(ctx, user.ID, ip)
	s.recordLogin(ctx, user.ID, model.LoginTypeWechatPhone, maskPhoneNumber(phone), true, "", ip, userAgent)
	return s.issue(ctx, user, ip, userAgent)
}

func (s *MiniappAuthService) loginBySms(ctx context.Context, phone, code, ip, userAgent string) (*MiniappLoginResult, error) {
	if strings.TrimSpace(phone) == "" || strings.TrimSpace(code) == "" {
		return nil, ErrLoginCredentialMissing
	}
	if !ValidCNMobile(phone) {
		return nil, ErrInvalidPhone
	}
	if err := verifyAndConsumeSMSCode(ctx, s.smsCodes, phone, model.SMSPurposeLogin, code); err != nil {
		s.recordLogin(ctx, "", model.LoginTypeSMSCode, maskPhoneNumber(phone), false, err.Error(), ip, userAgent)
		return nil, err
	}
	user, err := s.ensurePhoneUser(ctx, phone, model.LoginTypeSMSCode)
	if err != nil {
		s.recordLogin(ctx, "", model.LoginTypeSMSCode, maskPhoneNumber(phone), false, err.Error(), ip, userAgent)
		return nil, err
	}
	if err := ensureConsumerActive(user); err != nil {
		s.recordLogin(ctx, user.ID, model.LoginTypeSMSCode, maskPhoneNumber(phone), false, err.Error(), ip, userAgent)
		return nil, err
	}
	_ = s.users.TouchLogin(ctx, user.ID, ip)
	s.recordLogin(ctx, user.ID, model.LoginTypeSMSCode, maskPhoneNumber(phone), true, "", ip, userAgent)
	return s.issue(ctx, user, ip, userAgent)
}

// ensureWechatUser 按 openid 找账号，找不到就建一个（用户主体 + 微信身份同事务）。
// 返回的 user 一定已经落库。
func (s *MiniappAuthService) ensureWechatUser(ctx context.Context, openID, unionID string) (*model.User, error) {
	ident, err := s.users.FindWechatIdentity(ctx, model.WechatAppMiniapp, openID)
	switch {
	case err == nil:
		return s.users.FindByID(ctx, ident.UserID)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	now := time.Now()
	user := &model.User{
		ID:             uuid.NewString(),
		Nickname:       defaultConsumerNickname,
		Gender:         "unknown",
		Status:         model.UserStatusActive,
		RegisterSource: model.LoginTypeWechatMiniapp,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	newIdent := &model.UserWechatIdentity{
		ID: uuid.NewString(), UserID: user.ID, AppType: model.WechatAppMiniapp,
		OpenID: openID, UnionID: unionID, CreatedAt: now, UpdatedAt: now,
	}
	err = s.users.RegisterWithWechat(ctx, user, newIdent)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, model.ErrWechatIdentityTaken) {
		return nil, err
	}
	// 撞唯一键说明另一个并发请求刚建好了同一个 openid 的账号。这不是失败：
	// 小程序会在启动时并发发出几个登录请求，把它当错误的话用户看到的是
	// 一次莫名其妙的登录失败。回到「已存在」那条路。
	ident, err = s.users.FindWechatIdentity(ctx, model.WechatAppMiniapp, openID)
	if err != nil {
		return nil, err
	}
	return s.users.FindByID(ctx, ident.UserID)
}

// ensurePhoneUser 按手机号找账号，找不到就建一个。
func (s *MiniappAuthService) ensurePhoneUser(ctx context.Context, phone, registerSource string) (*model.User, error) {
	user, err := s.users.FindByPhone(ctx, phone)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	now := time.Now()
	user = &model.User{
		ID:             uuid.NewString(),
		Phone:          phone,
		Nickname:       defaultConsumerNickname,
		Gender:         "unknown",
		Status:         model.UserStatusActive,
		RegisterSource: registerSource,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.users.CreateWithPhone(ctx, user); err != nil {
		// 并发下另一个请求先建好了：重查即可，这和上面 openid 那条是同一个处理。
		if errors.Is(err, model.ErrPhoneTaken) {
			return s.users.FindByPhone(ctx, phone)
		}
		return nil, err
	}
	return user, nil
}

// linkWechatIdentity 把一个微信身份并到已知账号上，并保证不会把两个账号并起来。
//
// 归属不一致时返回 ErrAccountConflict，不做自动合并——合并意味着券、订单、余额
// 要跨账号搬，首期没有能安全做这件事的地方，而这条路径一旦写错就是把两个人的
// 资产并到一个人名下。
func (s *MiniappAuthService) linkWechatIdentity(ctx context.Context, user *model.User, openID, unionID string) error {
	ident, err := s.users.FindWechatIdentity(ctx, model.WechatAppMiniapp, openID)
	switch {
	case err == nil:
		if ident.UserID != user.ID {
			return ErrAccountConflict
		}
		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	now := time.Now()
	err = s.users.BindWechatIdentity(ctx, &model.UserWechatIdentity{
		ID: uuid.NewString(), UserID: user.ID, AppType: model.WechatAppMiniapp,
		OpenID: openID, UnionID: unionID, CreatedAt: now, UpdatedAt: now,
	})
	if errors.Is(err, model.ErrWechatIdentityTaken) {
		return ErrAccountConflict
	}
	return err
}

// issue 建一条会话并签一对令牌。
func (s *MiniappAuthService) issue(ctx context.Context, user *model.User, ip, userAgent string) (*MiniappLoginResult, error) {
	// realm 必须是 consumer。SignGrant 会拒绝不带 realm 的授权，所以这里
	// 漏写不是「签出一个平台令牌」，而是直接失败——这正是那个字段存在的意义。
	grant := auth.Grant{
		Subject:   user.ID,
		UserID:    user.ID,
		AccountID: user.ID,
		Tenant:    "",
		Realm:     auth.RealmConsumer,
	}
	accessToken, err := s.jwtSvc.SignAccessGrant(grant)
	if err != nil {
		return nil, err
	}
	refreshToken, err := s.jwtSvc.SignRefreshGrant(grant)
	if err != nil {
		return nil, err
	}
	// 会话的过期时间取自刚签出来的令牌本身，而不是在配置里再写一个 TTL，
	// 见 newSessionFor。
	session, err := newSessionFor(s.jwtSvc, user.ID, refreshToken, ip, userAgent)
	if err != nil {
		return nil, err
	}
	if err := s.sessions.CreateSession(ctx, session); err != nil {
		return nil, err
	}
	return &MiniappLoginResult{AccessToken: accessToken, RefreshToken: refreshToken, User: user}, nil
}

// recordLogin 追加一条登录审计。
//
// 它刻意不返回错误：审计写不进去不该把一个已经成功的登录变成失败。失败和成功
// 都记，因为「某个手机号被反复试码」这种事只有失败记录才看得出来。
func (s *MiniappAuthService) recordLogin(ctx context.Context, userID, loginType, identifier string, success bool, failReason, ip, userAgent string) {
	if utf8.RuneCountInString(failReason) > maxFailReasonLen {
		failReason = string([]rune(failReason)[:maxFailReasonLen])
	}
	if success {
		failReason = ""
	}
	_ = s.users.RecordLoginEvent(ctx, &model.UserLoginEvent{
		ID:         uuid.NewString(),
		UserID:     userID,
		LoginType:  loginType,
		Identifier: identifier,
		Success:    success,
		FailReason: failReason,
		IP:         ip,
		UserAgent:  userAgent,
		CreatedAt:  time.Now(),
	})
}

// verifyAndConsumeSMSCode 校验并消费一条验证码。
//
// 做成自由函数是因为登录和换绑手机号都要用它，而它们分属两个 service。
func verifyAndConsumeSMSCode(ctx context.Context, codes repository.SMSCodeRepository, phone, purpose, code string) error {
	rec, err := codes.FindSMSCode(ctx, phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSmsCodeInvalid
	}
	if err != nil {
		return err
	}
	// 用途不符按「无效」处理，不单独报：拿登录的码去换绑手机号必须被拦下，
	// 但没必要告诉尝试者「码是对的，只是用途不对」。
	if rec.Purpose != purpose || time.Now().After(rec.ExpiresAt) {
		return ErrSmsCodeInvalid
	}
	// 顺序是「先自增、再比对」。反过来的话，越界的那些请求在自增之前就已经把
	// 码验完了，上限拦住的只是已经试对的请求。自增由库做原子加法并返回新值，
	// 并发猜测各自拿到不同的数字，五次就是五次。
	attempts, err := codes.IncrementSMSCodeAttempts(ctx, phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSmsCodeInvalid
	}
	if err != nil {
		return err
	}
	if attempts > model.MaxSMSCodeAttempts {
		return ErrSmsCodeTooManyAttempts
	}
	if bcrypt.CompareHashAndPassword([]byte(rec.CodeHash), []byte(code)) != nil {
		return ErrSmsCodeInvalid
	}
	// 一次性：验过就删。并发提交同一个正确验证码时只有一方删得掉，另一方
	// 拿到 ErrNoRows，按已失效处理——同一个码不能用两次。
	if err := codes.DeleteSMSCode(ctx, phone); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSmsCodeInvalid
		}
		return err
	}
	return nil
}

// ensureConsumerActive 判定账号当前是否可用。
//
// 未知状态按不可用处理：库里有 CHECK 约束，走到 default 说明约束被绕过了，
// 那种情况下放行比拒绝危险得多。
func ensureConsumerActive(u *model.User) error {
	switch u.Status {
	case model.UserStatusActive:
		return nil
	case model.UserStatusDeleted:
		return ErrConsumerDeleted
	default:
		return ErrConsumerDisabled
	}
}

// newSessionFor 由刚签出的 refresh token 造一条会话记录。
//
// C 端与后台两条签发路径共用它：会话的过期时间只能有一处来源（就是令牌本身），
// 两处各读一次配置迟早会出现「令牌还有效，但刷新被拒」。ip/userAgent 只进审计
// 字段，缺了不影响校验。
func newSessionFor(jwtSvc *auth.Service, userID, refreshToken, ip, userAgent string) (*model.UserSession, error) {
	claims, err := jwtSvc.ParseRefresh(refreshToken)
	if err != nil {
		return nil, err
	}
	if claims.ExpiresAt == nil {
		return nil, errors.New("signed refresh token carries no expiry")
	}
	now := time.Now()
	return &model.UserSession{
		ID:               uuid.NewString(),
		UserID:           userID,
		RefreshTokenHash: hashRefreshToken(refreshToken),
		IssuedAt:         now,
		ExpiresAt:        claims.ExpiresAt.Time,
		IP:               ip,
		UserAgent:        userAgent,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// hashRefreshToken 是刷新令牌落库前的确定性哈希。
//
// 必须无盐且确定：刷新时只有令牌原文，要靠哈希反查那一行。这是安全的，因为
// 原文是 256 位随机字节，没有可枚举的空间——和 6 位验证码必须用慢哈希的理由
// 正好相反（见 model.UserSession、model.UserSMSCode）。
func hashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randomSMSCode 生成一个 6 位验证码。用 crypto/rand 而不是 math/rand：
// 验证码是凭据，可预测的随机数等于没有验证码。
func randomSMSCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// maskPhoneNumber 保留前 3 位和后 2 位，中间打码。写进审计记录的手机号用这个
// 形式：那张表是给安全侧看的，不是客服工具，不需要完整号码。
//
// 实现放在 model：后台审计（repository/admin_miniapp_user.go）也要用同一个口径，
// 两处各写一份迟早会出现「一个打码、一个没打」的分叉。
func maskPhoneNumber(phone string) string {
	return model.MaskPhone(phone)
}
