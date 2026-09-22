package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

// 商户登录链路的状态类错误，handler 映射为 403 + 明确提示
var (
	ErrMerchantUserDisabled = errors.New("账号已禁用")
	ErrMerchantPending      = errors.New("商户待审核")
	ErrMerchantSuspended    = errors.New("商户已暂停")
)

// ErrAdminUserDisabled 平台管理员账号被停用。与 C 端的 ErrConsumerDisabled 同义，
// handler 映射为 403；措辞与后台登录/详情页保持一致。
var ErrAdminUserDisabled = errors.New("账号已禁用")

// AdminAuthService 平台管理员登录/token 签发
type AdminAuthService struct {
	users    repository.AdminUserRepository
	bindings repository.AdminBindingRepository
	// 后台管理员有自己的会话表（admin_sessions），**不是** user_sessions：
	// 那张表的外键指 users(id)，而管理员住在 admin_users 里，写进去就是 23503。
	sessions repository.AdminSessionRepository
	jwtSvc   *auth.Service
}

func NewAdminAuthService(users repository.AdminUserRepository, bindings repository.AdminBindingRepository, sessions repository.AdminSessionRepository, jwtSvc *auth.Service) *AdminAuthService {
	return &AdminAuthService{users: users, bindings: bindings, sessions: sessions, jwtSvc: jwtSvc}
}

// Profile 返回当前登录管理员的账号信息
func (s *AdminAuthService) Profile(ctx context.Context, userID string) (*model.AdminUser, error) {
	return s.users.FindByID(ctx, userID)
}

// LiveAccess 返回用户在库中当前绑定的角色代码与权限码。
// JWT claims 里的角色/权限是登录时签发的，分配变更后不会同步；
// /users/me 用实时数据回显，前端权限与菜单才不会停留在旧 token 的状态。
func (s *AdminAuthService) LiveAccess(ctx context.Context, userID string) (roles, perms []string, err error) {
	boundRoles, err := s.bindings.FindRolesByUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	roles = make([]string, 0, len(boundRoles))
	for _, r := range boundRoles {
		roles = append(roles, r.Code)
	}
	perms, err = s.bindings.FindPermissionCodesByUser(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	if perms == nil {
		perms = []string{}
	}
	return roles, perms, nil
}

type LoginResult struct {
	AccessToken  string
	RefreshToken string
}

func (s *AdminAuthService) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	user, err := s.users.FindByUsername(ctx, username)
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	roles, err := s.bindings.FindRolesByUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	roleCodes := make([]string, len(roles))
	for i, r := range roles {
		roleCodes[i] = r.Code
	}
	isSuper := false
	for _, role := range roles {
		if role.Code == "super_admin" {
			isSuper = true
			break
		}
	}

	permCodes, err := s.bindings.FindPermissionCodesByUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	grant := auth.Grant{
		Subject:     user.ID,
		UserID:      user.ID,
		AccountID:   user.ID,
		Tenant:      "",
		Realm:       auth.RealmPlatform,
		Roles:       roleCodes,
		Permissions: permCodes,
		IsSuper:     isSuper,
	}
	return s.issuePair(ctx, grant)
}

// issuePair 签一对令牌，并把新签出的 refresh 落成一条会话记录。
//
// 落库这一步是撤销能力的前提：没有它，「退出登录」和「账号被停用」都只是签发方
// 一厢情愿——令牌还能拿去换新的。C 端的 issue 做的是同一件事。
func (s *AdminAuthService) issuePair(ctx context.Context, grant auth.Grant) (*LoginResult, error) {
	accessToken, err := s.jwtSvc.SignAccessGrant(grant)
	if err != nil {
		return nil, err
	}
	refreshToken, err := s.jwtSvc.SignRefreshGrant(grant)
	if err != nil {
		return nil, err
	}
	// 后台登录链路目前不采集 IP/UA（handler 的 Login 不传），这两个字段留空；
	// 它们只进审计，不参与任何判定。
	session, err := newSessionFor(s.jwtSvc, grant.UserID, refreshToken, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.sessions.CreateSession(ctx, session); err != nil {
		return nil, err
	}
	return &LoginResult{AccessToken: accessToken, RefreshToken: refreshToken}, nil
}

// Refresh 用后台的刷新令牌换一对新令牌，并轮换掉旧的。
//
// 与 C 端同一套规则：令牌本身只是一张入场券，能不能换出东西由库里的会话和账号
// 状态决定。少了下面任何一道，一枚已经登出、已经被停用或根本不属于后台的令牌
// 都能在这个接口上换出一对全新令牌，而撤销就永远只是「客户端忘了它」。
func (s *AdminAuthService) Refresh(ctx context.Context, refreshToken string) (*LoginResult, error) {
	claims, err := s.jwtSvc.ParseRefresh(refreshToken)
	if err != nil {
		return nil, ErrRefreshTokenInvalid
	}
	// realm 断言必须落在常量上。ParseRefresh 只验签名，凡是本服务签发的 refresh
	// token 都能通过——包括 C 端顾客和商户员工的那两种。旧实现把 claims.Realm
	// 原样抄进新令牌，看上去「没有换域」，但那不是断言：抄来的值恒等于来令牌
	// 自己的值，它对「这枚令牌该不该被这个接口接受」一个字都没说。真正的后果是
	// C 端的刷新令牌可以打到后台接口，在这条既没有会话查询也没有账号状态检查的
	// 路径上换出一对新令牌——一次已经登出、已被撤销的会话就这样复活了。
	if claims.Realm != auth.RealmPlatform {
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
	// 撤销原因决定「旧令牌又出现」是不是盗用信号，判断口径与 C 端一致：
	// rotated 是被新令牌换掉的，再出现只有一个解释；logout 之后客户端里残留的
	// 旧令牌再刷一次是正常时序，不连坐其他设备。
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
	// 账号状态在刷新时重新读一次，而不是信 token 里的快照。这是「停用」那道闸的
	// 第二道锁：第一道是改状态时撤销所有会话，但撤销可能失败，而这里一旦读到
	// 非 active，令牌再新也换不出东西来。
	user, err := s.users.FindByID(ctx, session.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRefreshTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if user.Status != "active" {
		_ = s.sessions.RevokeUserSessions(ctx, user.ID, model.RevokeReasonDisabled)
		return nil, ErrAdminUserDisabled
	}
	// 记下这枚令牌最后一次被使用的时刻，再撤销它——前者是「什么时候没的」，
	// 后者是「最后一次被拿来用是什么时候」，两者相差很大时很可疑。
	_ = s.sessions.TouchSessionUsed(ctx, session.ID)
	if err := s.sessions.RevokeSession(ctx, session.ID, model.RevokeReasonRotated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_ = s.sessions.RevokeUserSessions(ctx, session.UserID, model.RevokeReasonReuseDetected)
			return nil, ErrRefreshTokenReused
		}
		return nil, err
	}
	// 重新查询权限，保证 token 刷新后权限是最新的
	roles, err := s.bindings.FindRolesByUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	roleCodes := make([]string, len(roles))
	for i, r := range roles {
		roleCodes[i] = r.Code
	}
	isSuper := false
	for _, role := range roles {
		if role.Code == "super_admin" {
			isSuper = true
			break
		}
	}
	permCodes, err := s.bindings.FindPermissionCodesByUser(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	grant := auth.Grant{
		Subject:   user.ID,
		UserID:    user.ID,
		AccountID: user.ID,
		Tenant:    "",
		// 写死平台域，不从来令牌继承：这一行以上的 realm 断言已经把来令牌验成
		// 平台域了，再抄一次只是把一个已经验证过的事实重新变成输入。
		Realm:       auth.RealmPlatform,
		Roles:       roleCodes,
		Permissions: permCodes,
		IsSuper:     isSuper,
	}
	return s.issuePair(ctx, grant)
}

// Logout 撤销与这枚刷新令牌对应的那一个会话。
//
// 与 C 端同一条约定：不带鉴权中间件，也不需要 access token——退出登录发生在
// access token 刚好过期的时候最需要。凭据就是这枚令牌本身，拿到它的人本来就能
// 用它换出新令牌，所以对未知/已失效的令牌一律返回成功，不给出「这枚令牌是否
// 存在」的区分。
//
// 令牌为空时直接放行：后台前端的登出请求至今不带 body，而它的语义（清掉本地
// 令牌）已经完成，把这种调用判成 400 只会让「退出登录」在升级后的第一次点击
// 就失败。等前端带上 refreshToken 后，这条路径才真正撤销服务端会话。
func (s *AdminAuthService) Logout(ctx context.Context, refreshToken string) error {
	if strings.TrimSpace(refreshToken) == "" {
		return nil
	}
	claims, err := s.jwtSvc.ParseRefresh(refreshToken)
	if err != nil || claims.Realm != auth.RealmPlatform {
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

// AdminUserService 平台管理员信息管理
