package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// 只实现被测分支用到的方法，其余留给内嵌接口——真被调到会 panic，
// 恰好说明这个用例越过了它自己声明的范围。
type stubSessionRepo struct {
	repository.UserSessionRepository
	session    *model.UserSession
	revokedAll []string
	revokedOne []string
}

func (s *stubSessionRepo) FindSessionByHash(context.Context, string) (*model.UserSession, error) {
	if s.session == nil {
		return nil, errors.New("no session")
	}
	return s.session, nil
}

func (s *stubSessionRepo) RevokeUserSessions(_ context.Context, _, reason string) error {
	s.revokedAll = append(s.revokedAll, reason)
	return nil
}

func (s *stubSessionRepo) RevokeSession(_ context.Context, _, reason string) error {
	s.revokedOne = append(s.revokedOne, reason)
	return nil
}

func (s *stubSessionRepo) TouchSessionUsed(context.Context, string) error { return nil }

type stubUserRepo struct {
	repository.UserRepository
	user *model.User
}

func (s *stubUserRepo) FindByID(context.Context, string) (*model.User, error) {
	return s.user, nil
}

// 撤销原因决定「旧令牌又出现」是不是盗用信号，这个判断错了会误伤：
// 把 logout 也当成盗用，用户在一台设备上退出登录，另一台上还开着的会话
// 会被一起撤销——而那条路径上没有任何攻击者。
func TestRefreshDistinguishesRevokeReasons(t *testing.T) {
	const userID = "u-1"
	for _, tc := range []struct {
		name          string
		revokeReason  string
		wantErr       error
		wantRevokeAll bool
	}{
		{
			name:          "轮换过的令牌再次出现 = 盗用",
			revokeReason:  model.RevokeReasonRotated,
			wantErr:       ErrRefreshTokenReused,
			wantRevokeAll: true,
		},
		{
			name:          "主动退出后客户端残留的刷新 = 普通失效，不连坐其他设备",
			revokeReason:  model.RevokeReasonLogout,
			wantErr:       ErrRefreshTokenInvalid,
			wantRevokeAll: false,
		},
		{
			name:          "已被判过盗用的令牌再来一次，不再重复连坐",
			revokeReason:  model.RevokeReasonReuseDetected,
			wantErr:       ErrRefreshTokenInvalid,
			wantRevokeAll: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jwtSvc, err := auth.NewService([]byte("test-secret-at-least-32-bytes-long!!"), "test", time.Hour, time.Hour)
			if err != nil {
				t.Fatalf("jwt: %v", err)
			}
			token, err := jwtSvc.SignRefreshGrant(auth.Grant{Subject: userID, UserID: userID, Realm: auth.RealmConsumer})
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			revokedAt := time.Now().Add(-time.Minute)
			sessions := &stubSessionRepo{session: &model.UserSession{
				ID:           "s-1",
				UserID:       userID,
				RevokedAt:    &revokedAt,
				RevokeReason: tc.revokeReason,
				ExpiresAt:    time.Now().Add(time.Hour),
			}}
			svc := NewMiniappAuthService(
				&stubUserRepo{user: &model.User{ID: userID, Status: model.UserStatusActive}},
				sessions, nil, nil, nil, jwtSvc,
			)

			_, err = svc.Refresh(context.Background(), token, "127.0.0.1", "test")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Refresh() error = %v, want %v", err, tc.wantErr)
			}
			if got := len(sessions.revokedAll) > 0; got != tc.wantRevokeAll {
				t.Fatalf("revoke-all called = %v (%v), want %v", got, sessions.revokedAll, tc.wantRevokeAll)
			}
		})
	}
}
