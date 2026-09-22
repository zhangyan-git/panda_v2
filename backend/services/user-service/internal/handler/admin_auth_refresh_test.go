package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

type adminAuthSessions struct {
	repository.UserSessionRepository
	session *model.UserSession
	created []*model.UserSession
	revoked []string
}

func (s *adminAuthSessions) FindSessionByHash(context.Context, string) (*model.UserSession, error) {
	if s.session == nil {
		return nil, pgx.ErrNoRows
	}
	return s.session, nil
}

func (s *adminAuthSessions) RevokeSession(_ context.Context, _, reason string) error {
	s.revoked = append(s.revoked, reason)
	return nil
}

func (s *adminAuthSessions) RevokeUserSessions(_ context.Context, _, reason string) error {
	s.revoked = append(s.revoked, reason)
	return nil
}

func (s *adminAuthSessions) TouchSessionUsed(context.Context, string) error { return nil }

func (s *adminAuthSessions) CreateSession(_ context.Context, session *model.UserSession) error {
	s.created = append(s.created, session)
	return nil
}

func newAdminAuthHandlerFixture(t *testing.T) (*AdminAuthHandler, *meUsers, *adminAuthSessions, *auth.Service) {
	t.Helper()
	jwtSvc, err := auth.NewService([]byte(strings.Repeat("x", 32)), "admin-auth-test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	users := &meUsers{user: &model.AdminUser{ID: "admin", Username: "alice", Status: "active"}}
	sessions := &adminAuthSessions{}
	svc := service.NewAdminAuthService(users, &meBindings{}, sessions, jwtSvc)
	return NewAdminAuthHandler(svc), users, sessions, jwtSvc
}

func postAdminAuth(t *testing.T, h *AdminAuthHandler, call string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/"+call, strings.NewReader(body))
	if body == "" {
		// httptest 会把空串 body 当成有内容；真实客户端不带 body 时 ContentLength 是 0。
		r.ContentLength = 0
	}
	rec := httptest.NewRecorder()
	if call == "logout" {
		h.Logout(rec, r)
	} else {
		h.Refresh(rec, r)
	}
	return rec
}

// 后台前端的登出请求至今不带任何 body。它以前是个空操作，现在也不能因为
// 「没给令牌」就回 400——那会让退出登录在第一次点击就失败。
func TestAdminLogoutToleratesMissingBody(t *testing.T) {
	h, _, sessions, _ := newAdminAuthHandlerFixture(t)

	rec := postAdminAuth(t, h, "logout", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if len(sessions.revoked) != 0 {
		t.Fatalf("没有令牌就没有可撤销的会话: %+v", sessions.revoked)
	}
}

// 带上令牌时，登出要真的把服务端会话撤掉。
func TestAdminLogoutRevokesWhenTokenGiven(t *testing.T) {
	h, _, sessions, jwtSvc := newAdminAuthHandlerFixture(t)
	token, err := jwtSvc.SignRefreshGrant(auth.Grant{Subject: "admin", UserID: "admin", Realm: auth.RealmPlatform})
	if err != nil {
		t.Fatal(err)
	}
	sessions.session = &model.UserSession{ID: "s-1", UserID: "admin", ExpiresAt: time.Now().Add(time.Hour), RevokedAt: nil}

	rec := postAdminAuth(t, h, "logout", `{"refreshToken":"`+token+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if len(sessions.revoked) != 1 || sessions.revoked[0] != model.RevokeReasonLogout {
		t.Fatalf("登出应当撤销会话: %+v", sessions.revoked)
	}
}

// C 端令牌打后台刷新接口：必须 401，且不能为它碰任何状态。
func TestAdminRefreshRejectsConsumerToken(t *testing.T) {
	h, _, sessions, jwtSvc := newAdminAuthHandlerFixture(t)
	token, err := jwtSvc.SignRefreshGrant(auth.Grant{Subject: "user-1", UserID: "user-1", Realm: auth.RealmConsumer})
	if err != nil {
		t.Fatal(err)
	}

	rec := postAdminAuth(t, h, "refresh", `{"refreshToken":"`+token+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body)
	}
	if len(sessions.created) != 0 {
		t.Fatal("非平台域不该被签发新会话")
	}
}

// 账号被停用时是 403（找管理员），令牌不可用是 401（重新登录）——客户端要做的事不同。
func TestAdminRefreshMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		wantStatus int
		wantCode   string
	}{
		{"账号被停用", "disabled", http.StatusForbidden, api.CodeForbidden},
		{"平台账号可用", "active", http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, users, sessions, jwtSvc := newAdminAuthHandlerFixture(t)
			users.user.Status = tc.status
			token, err := jwtSvc.SignRefreshGrant(auth.Grant{Subject: "admin", UserID: "admin", Realm: auth.RealmPlatform})
			if err != nil {
				t.Fatal(err)
			}
			sessions.session = &model.UserSession{ID: "s-1", UserID: "admin", ExpiresAt: time.Now().Add(time.Hour)}

			rec := postAdminAuth(t, h, "refresh", `{"refreshToken":"`+token+`"}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantCode == "" {
				return
			}
			var resp api.Response
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			if resp.ErrorCode != tc.wantCode {
				t.Fatalf("unexpected envelope: %+v", resp)
			}
		})
	}
}
