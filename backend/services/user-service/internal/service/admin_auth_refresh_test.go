package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// 只实现被测分支用到的方法，其余留给内嵌接口——真被调到会 panic，
// 恰好说明这个用例越过了它自己声明的范围。
type adminAuthSessions struct {
	repository.UserSessionRepository
	session    *model.UserSession
	findErr    error
	created    []*model.UserSession
	revokedOne []string
	revokedAll []string
}

func (s *adminAuthSessions) FindSessionByHash(context.Context, string) (*model.UserSession, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.session == nil {
		return nil, pgx.ErrNoRows
	}
	return s.session, nil
}

// RevokeSession 复刻真实仓储的两条语义：revoked_at IS NULL 是抢占条件（已撤销的行
// 返回 ErrNoRows），撤销原因只写一次、不被后来的覆盖。
func (s *adminAuthSessions) RevokeSession(_ context.Context, id, reason string) error {
	if s.session == nil || s.session.ID != id || s.session.RevokedAt != nil {
		return pgx.ErrNoRows
	}
	now := time.Now()
	s.session.RevokedAt = &now
	s.session.RevokeReason = reason
	s.revokedOne = append(s.revokedOne, reason)
	return nil
}

func (s *adminAuthSessions) RevokeUserSessions(_ context.Context, _, reason string) error {
	s.revokedAll = append(s.revokedAll, reason)
	if s.session != nil && s.session.RevokedAt == nil {
		now := time.Now()
		s.session.RevokedAt = &now
		s.session.RevokeReason = reason
	}
	return nil
}

func (s *adminAuthSessions) TouchSessionUsed(context.Context, string) error { return nil }

func (s *adminAuthSessions) CreateSession(_ context.Context, session *model.UserSession) error {
	s.created = append(s.created, session)
	return nil
}

type adminAuthUsers struct {
	repository.AdminUserRepository
	user *model.AdminUser
	err  error
}

func (f *adminAuthUsers) FindByID(context.Context, string) (*model.AdminUser, error) {
	return f.user, f.err
}

type adminAuthBindings struct {
	repository.AdminBindingRepository
	roles []*model.AdminRole
	perms []string
}

func (f *adminAuthBindings) FindRolesByUser(context.Context, string) ([]*model.AdminRole, error) {
	return f.roles, nil
}

func (f *adminAuthBindings) FindPermissionCodesByUser(context.Context, string) ([]string, error) {
	return f.perms, nil
}

type adminAuthFixture struct {
	svc      *AdminAuthService
	users    *adminAuthUsers
	sessions *adminAuthSessions
	jwtSvc   *auth.Service
}

func newAdminAuthFixture(t *testing.T) *adminAuthFixture {
	t.Helper()
	jwtSvc, err := auth.NewService([]byte("test-secret-at-least-32-bytes-long!!"), "test", time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}
	users := &adminAuthUsers{user: &model.AdminUser{ID: adminUserID, Username: "alice", Status: "active"}}
	sessions := &adminAuthSessions{}
	svc := NewAdminAuthService(users, &adminAuthBindings{roles: []*model.AdminRole{{Code: "editor"}}, perms: []string{"admin:brands:view"}}, sessions, jwtSvc)
	return &adminAuthFixture{svc: svc, users: users, sessions: sessions, jwtSvc: jwtSvc}
}

const adminUserID = "admin-1"

// refreshTokenFor 签一枚指定域的 refresh token，并（可选）在会话表里落一行。
func (f *adminAuthFixture) refreshTokenFor(t *testing.T, realm auth.Realm, session *model.UserSession) string {
	t.Helper()
	token, err := f.jwtSvc.SignRefreshGrant(auth.Grant{Subject: adminUserID, UserID: adminUserID, Realm: realm})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if session != nil {
		claims, err := f.jwtSvc.ParseRefresh(token)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		session.ID = "s-1"
		session.UserID = adminUserID
		session.RefreshTokenHash = hashRefreshToken(token)
		session.IssuedAt = time.Now()
		// 用例自己指定的过期时间优先：测「过期会话」时要能把它设到过去。
		if session.ExpiresAt.IsZero() {
			session.ExpiresAt = claims.ExpiresAt.Time
		}
		f.sessions.session = session
	}
	return token
}

// C 端顾客与商户员工的 refresh token 同样由本服务签发，ParseRefresh 只验签名。
// 它们必须在这个接口上被拒——否则那条没有会话查询的路径会把一枚已登出的
// C 端令牌重新变成登录态。
func TestAdminRefreshRejectsNonPlatformRealm(t *testing.T) {
	// 空 realm 签不出来（SignGrant 直接拒绝），所以这里只列另外两种真实存在的域。
	for _, realm := range []auth.Realm{auth.RealmConsumer, auth.RealmMerchant} {
		t.Run(string(realm), func(t *testing.T) {
			f := newAdminAuthFixture(t)
			token := f.refreshTokenFor(t, realm, &model.UserSession{RevokedAt: nil})

			if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrRefreshTokenInvalid) {
				t.Fatalf("Refresh() error = %v, want %v", err, ErrRefreshTokenInvalid)
			}
			// realm 断言在查会话之前就该失败：不许为别的域去碰会话表。
			if len(f.sessions.revokedOne) != 0 || len(f.sessions.revokedAll) != 0 {
				t.Fatalf("非平台域不该走到撤销: %+v", f.sessions)
			}
		})
	}
}

// 已登出（或已被撤销）的令牌不能再换到 token。
func TestAdminRefreshRejectsLoggedOutSession(t *testing.T) {
	f := newAdminAuthFixture(t)
	revokedAt := time.Now().Add(-time.Minute)
	token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{
		RevokedAt: &revokedAt, RevokeReason: model.RevokeReasonLogout,
	})

	if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Fatalf("Refresh() error = %v, want %v", err, ErrRefreshTokenInvalid)
	}
	// logout 之后客户端残留的旧令牌再刷一次是正常时序，不该连坐其他设备。
	if len(f.sessions.revokedAll) != 0 {
		t.Fatalf("logout 不该触发全量撤销: %+v", f.sessions.revokedAll)
	}
}

// 被换掉的令牌再次出现是盗用信号，按 C 端同一口径处理。
func TestAdminRefreshTreatsRotatedTokenAsReuse(t *testing.T) {
	f := newAdminAuthFixture(t)
	revokedAt := time.Now().Add(-time.Minute)
	token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{
		RevokedAt: &revokedAt, RevokeReason: model.RevokeReasonRotated,
	})

	if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrRefreshTokenReused) {
		t.Fatalf("Refresh() error = %v, want %v", err, ErrRefreshTokenReused)
	}
	if len(f.sessions.revokedAll) != 1 || f.sessions.revokedAll[0] != model.RevokeReasonReuseDetected {
		t.Fatalf("盗用应当撤销该用户全部会话: %+v", f.sessions.revokedAll)
	}
}

// 令牌有效但账号已停用：换不出新令牌，并且顺手把还活着的会话撤掉。
func TestAdminRefreshRejectsDisabledAccount(t *testing.T) {
	f := newAdminAuthFixture(t)
	f.users.user.Status = "disabled"
	token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{})

	if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrAdminUserDisabled) {
		t.Fatalf("Refresh() error = %v, want %v", err, ErrAdminUserDisabled)
	}
	if len(f.sessions.revokedAll) != 1 || f.sessions.revokedAll[0] != model.RevokeReasonDisabled {
		t.Fatalf("停用应当撤销会话: %+v", f.sessions.revokedAll)
	}
	if len(f.sessions.created) != 0 {
		t.Fatal("停用的账号不该被签发新会话")
	}
}

// 账号被删（或查不到）时，令牌再新也没用。
func TestAdminRefreshRejectsMissingAccount(t *testing.T) {
	f := newAdminAuthFixture(t)
	f.users.user, f.users.err = nil, pgx.ErrNoRows
	token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{})

	if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Fatalf("Refresh() error = %v, want %v", err, ErrRefreshTokenInvalid)
	}
}

// 过期会话不能用；库里的行与令牌声称的身份对不上也不能用。
func TestAdminRefreshRejectsExpiredOrMismatchedSession(t *testing.T) {
	t.Run("过期", func(t *testing.T) {
		f := newAdminAuthFixture(t)
		token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{
			ExpiresAt: time.Now().Add(-time.Minute),
		})
		if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrRefreshTokenInvalid) {
			t.Fatalf("Refresh() error = %v, want %v", err, ErrRefreshTokenInvalid)
		}
	})
	t.Run("身份对不上", func(t *testing.T) {
		f := newAdminAuthFixture(t)
		token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{})
		f.sessions.session.UserID = "someone-else"
		if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrRefreshTokenInvalid) {
			t.Fatalf("Refresh() error = %v, want %v", err, ErrRefreshTokenInvalid)
		}
	})
}

// 正常刷新：轮换旧会话，并把新签发的 refresh 落库——不落库，下一次登出就吊不掉它。
func TestAdminRefreshRotatesAndPersistsSession(t *testing.T) {
	f := newAdminAuthFixture(t)
	token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{})

	result, err := f.svc.Refresh(context.Background(), token)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if result.AccessToken == "" || result.RefreshToken == "" {
		t.Fatal("刷新应当签出一对新令牌")
	}
	if len(f.sessions.revokedOne) != 1 || f.sessions.revokedOne[0] != model.RevokeReasonRotated {
		t.Fatalf("旧会话应当被轮换掉: %+v", f.sessions.revokedOne)
	}
	if len(f.sessions.created) != 1 {
		t.Fatalf("新签发的 refresh 必须落库: %+v", f.sessions.created)
	}
	if got, want := f.sessions.created[0].RefreshTokenHash, hashRefreshToken(result.RefreshToken); got != want {
		t.Fatalf("落库的哈希与返回的令牌对不上: %s != %s", got, want)
	}
	if f.sessions.created[0].UserID != adminUserID {
		t.Fatalf("会话挂到了错误的账号上: %+v", f.sessions.created[0])
	}
	// 新令牌的域必须是平台域：断言过的事实不该再从来令牌抄回来。
	claims, err := f.jwtSvc.ParseRefresh(result.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Realm != auth.RealmPlatform {
		t.Fatalf("realm = %q, want %q", claims.Realm, auth.RealmPlatform)
	}
}

// 登出必须真的撤销会话：撤完再用这枚令牌刷新，就换不出东西了。
func TestAdminLogoutRevokesSessionAndBlocksRefresh(t *testing.T) {
	f := newAdminAuthFixture(t)
	token := f.refreshTokenFor(t, auth.RealmPlatform, &model.UserSession{})

	if err := f.svc.Logout(context.Background(), token); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if len(f.sessions.revokedOne) != 1 || f.sessions.revokedOne[0] != model.RevokeReasonLogout {
		t.Fatalf("登出应当撤销会话: %+v", f.sessions.revokedOne)
	}
	if _, err := f.svc.Refresh(context.Background(), token); !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Fatalf("登出后 Refresh() error = %v, want %v", err, ErrRefreshTokenInvalid)
	}
	if len(f.sessions.created) != 0 {
		t.Fatal("登出后不该再签出会话")
	}
}

// 登出对未知令牌一律成功，也不去撤销别人的会话：它凭的是令牌本身，
// 所以「这枚令牌是否存在」「它属于哪个域」都不构成返回值上的区别。
func TestAdminLogoutIgnoresUnknownAndForeignTokens(t *testing.T) {
	f := newAdminAuthFixture(t)
	consumer := f.refreshTokenFor(t, auth.RealmConsumer, &model.UserSession{})

	for name, token := range map[string]string{"空令牌": "", "空白令牌": "   ", "乱码": "not-a-token", "C 端域": consumer} {
		t.Run(name, func(t *testing.T) {
			if err := f.svc.Logout(context.Background(), token); err != nil {
				t.Fatalf("Logout() error = %v", err)
			}
			if len(f.sessions.revokedOne) != 0 || len(f.sessions.revokedAll) != 0 {
				t.Fatalf("不该撤销任何会话: %+v", f.sessions)
			}
		})
	}
}
