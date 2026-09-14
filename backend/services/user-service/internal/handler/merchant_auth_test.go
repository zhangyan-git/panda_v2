package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
	"golang.org/x/crypto/bcrypt"
)

type merchantAuthUsers struct {
	repository.MerchantUserRepository
	user                                      *model.MerchantUser
	err                                       error
	profileIDs, usernames, touchIDs, touchIPs []string
	touchErr                                  error
}

func (f *merchantAuthUsers) FindByID(_ context.Context, id string) (*model.MerchantUser, error) {
	f.profileIDs = append(f.profileIDs, id)
	return f.user, f.err
}

func (f *merchantAuthUsers) FindByUsername(_ context.Context, username string) (*model.MerchantUser, error) {
	f.usernames = append(f.usernames, username)
	return f.user, f.err
}

func (f *merchantAuthUsers) TouchLogin(_ context.Context, id, ip string) error {
	f.touchIDs = append(f.touchIDs, id)
	f.touchIPs = append(f.touchIPs, ip)
	return f.touchErr
}

type merchantAuthAccess struct {
	status, name       string
	statusErr, nameErr error
	statusIDs, nameIDs []string
}

func (f *merchantAuthAccess) FindStatus(_ context.Context, id string) (string, error) {
	f.statusIDs = append(f.statusIDs, id)
	return f.status, f.statusErr
}

func (f *merchantAuthAccess) FindName(_ context.Context, id string) (string, error) {
	f.nameIDs = append(f.nameIDs, id)
	return f.name, f.nameErr
}

func TestMerchantMeFailClosed(t *testing.T) {
	identity := &auth.Identity{Subject: "user", UserID: "user", Tenant: "merchant"}
	active := &model.MerchantUser{ID: "user", MerchantID: "merchant", Status: "active"}
	missing := fmt.Errorf("lookup: %w", pgx.ErrNoRows)
	unavailable := errors.New("private dependency failure")
	for _, tt := range []struct {
		name                                 string
		identity                             *auth.Identity
		user                                 *model.MerchantUser
		profileErr                           error
		status                               string
		statusErr                            error
		merchantName                         string
		nameErr                              error
		want                                 int
		message                              string
		profileCalls, statusCalls, nameCalls int
	}{
		{name: "unauthenticated", want: 401, message: "未登录"},
		{name: "missing user id", identity: &auth.Identity{Subject: "user", Tenant: "merchant"}, want: 401, message: "未登录"},
		{name: "blank user id", identity: &auth.Identity{Subject: " ", UserID: " ", Tenant: "merchant"}, want: 401, message: "未登录"},
		{name: "missing subject", identity: &auth.Identity{UserID: "user", Tenant: "merchant"}, want: 401, message: "未登录"},
		{name: "subject mismatch", identity: &auth.Identity{Subject: "other", UserID: "user", Tenant: "merchant"}, want: 401, message: "未登录"},
		{name: "platform identity", identity: &auth.Identity{Subject: "user", UserID: "user", IsSuper: true}, want: 403, message: "非商户身份"},
		{name: "blank tenant", identity: &auth.Identity{Subject: "user", UserID: "user", Tenant: " "}, want: 403, message: "非商户身份"},
		{name: "missing account", identity: identity, profileErr: missing, want: 401, message: "账号不存在", profileCalls: 1},
		{name: "profile unavailable", identity: identity, profileErr: unavailable, want: 503, message: "账号服务暂不可用", profileCalls: 1},
		{name: "profile with error", identity: identity, user: active, profileErr: unavailable, want: 503, message: "账号服务暂不可用", profileCalls: 1},
		{name: "nil profile", identity: identity, want: 503, message: "账号服务暂不可用", profileCalls: 1},
		{name: "profile id mismatch", identity: identity, user: &model.MerchantUser{ID: "other", MerchantID: "merchant", Status: "active"}, want: 503, message: "账号服务暂不可用", profileCalls: 1},
		{name: "missing profile id", identity: identity, user: &model.MerchantUser{MerchantID: "merchant", Status: "active"}, want: 503, message: "账号服务暂不可用", profileCalls: 1},
		{name: "missing merchant id", identity: identity, user: &model.MerchantUser{ID: "user", Status: "active"}, want: 503, message: "账号服务暂不可用", profileCalls: 1},
		{name: "blank merchant id", identity: identity, user: &model.MerchantUser{ID: "user", MerchantID: " ", Status: "active"}, want: 503, message: "账号服务暂不可用", profileCalls: 1},
		{name: "tenant mismatch", identity: identity, user: &model.MerchantUser{ID: "user", MerchantID: "other", Status: "active"}, want: 403, message: "账号不属于当前商户", profileCalls: 1},
		{name: "disabled account", identity: identity, user: &model.MerchantUser{ID: "user", MerchantID: "merchant", Status: "disabled"}, want: 403, message: "账号已禁用", profileCalls: 1},
		{name: "unknown account status", identity: identity, user: &model.MerchantUser{ID: "user", MerchantID: "merchant"}, want: 403, message: "账号已禁用", profileCalls: 1},
		{name: "pending merchant", identity: identity, user: active, status: "pending", want: 403, message: "商户待审核", profileCalls: 1, statusCalls: 1},
		{name: "suspended merchant", identity: identity, user: active, status: "suspended", want: 403, message: "商户已暂停", profileCalls: 1, statusCalls: 1},
		{name: "missing merchant status", identity: identity, user: active, statusErr: missing, want: 403, message: "商户不存在", profileCalls: 1, statusCalls: 1},
		{name: "status unavailable", identity: identity, user: active, status: "active", statusErr: unavailable, want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1},
		{name: "status timeout", identity: identity, user: active, statusErr: context.DeadlineExceeded, want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1},
		{name: "empty status", identity: identity, user: active, want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1},
		{name: "unknown status", identity: identity, user: active, status: "deleted", want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1},
		{name: "missing merchant name", identity: identity, user: active, status: "active", nameErr: missing, want: 403, message: "商户不存在", profileCalls: 1, statusCalls: 1, nameCalls: 1},
		{name: "name unavailable", identity: identity, user: active, status: "active", merchantName: "stale", nameErr: unavailable, want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1, nameCalls: 1},
		{name: "name cancelled", identity: identity, user: active, status: "active", nameErr: context.Canceled, want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1, nameCalls: 1},
		{name: "empty name", identity: identity, user: active, status: "active", want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1, nameCalls: 1},
		{name: "blank name", identity: identity, user: active, status: "active", merchantName: " ", want: 503, message: "商户服务暂不可用", profileCalls: 1, statusCalls: 1, nameCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			users := &merchantAuthUsers{user: tt.user, err: tt.profileErr}
			access := &merchantAuthAccess{status: tt.status, statusErr: tt.statusErr, name: tt.merchantName, nameErr: tt.nameErr}
			h := NewMerchantAuthHandler(service.NewMerchantAuthService(users, access, nil))
			r := httptest.NewRequest(http.MethodGet, "/v1/merchant/users/me", nil)
			if tt.identity != nil {
				r = r.WithContext(auth.WithIdentity(r.Context(), *tt.identity))
			}
			w := httptest.NewRecorder()
			h.Me(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			var got api.Response
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			code := map[int]string{401: api.CodeUnauthorized, 403: api.CodeForbidden, 503: api.CodeUnavailable}[tt.want]
			if got.Success || got.Data != nil || got.ErrorCode != code || got.ErrorMessage != tt.message {
				t.Fatalf("unexpected error envelope: %+v", got)
			}
			if len(users.profileIDs) != tt.profileCalls || len(access.statusIDs) != tt.statusCalls || len(access.nameIDs) != tt.nameCalls || len(users.touchIDs) != 0 {
				t.Fatalf("unexpected calls: users=%+v access=%+v", users, access)
			}
			for _, id := range append(access.statusIDs, access.nameIDs...) {
				if id != "merchant" {
					t.Fatalf("lookup used wrong merchant %q", id)
				}
			}
		})
	}
}

func TestMerchantMeRechecksSameIdentityAndPreservesResponse(t *testing.T) {
	for _, change := range []string{"account disabled", "merchant pending", "merchant suspended", "dependency failure"} {
		t.Run(change, func(t *testing.T) {
			users := &merchantAuthUsers{user: &model.MerchantUser{ID: "user", MerchantID: "merchant", Username: "alice", Name: "Alice", Email: "alice@example.test", Status: "active"}}
			access := &merchantAuthAccess{status: "active", name: "Coffee"}
			h := NewMerchantAuthHandler(service.NewMerchantAuthService(users, access, nil))
			r := httptest.NewRequest(http.MethodGet, "/v1/merchant/users/me", nil)
			r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{Subject: "user", UserID: "user", Tenant: "merchant", IsSuper: true}))
			w := httptest.NewRecorder()
			h.Me(w, r)
			var got struct {
				Success bool              `json:"success"`
				Data    map[string]string `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"id": "user", "username": "alice", "name": "Alice", "email": "alice@example.test", "merchantId": "merchant", "merchantName": "Coffee"}
			if w.Code != 200 || !got.Success || !reflect.DeepEqual(got.Data, want) {
				t.Fatalf("incompatible response: status=%d body=%s", w.Code, w.Body)
			}
			wantStatus := 403
			switch change {
			case "account disabled":
				users.user.Status = "disabled"
			case "merchant pending":
				access.status = "pending"
			case "merchant suspended":
				access.status = "suspended"
			case "dependency failure":
				access.statusErr = context.DeadlineExceeded
				wantStatus = 503
			}
			w = httptest.NewRecorder()
			h.Me(w, r)
			wantStatusCalls := 2
			if change == "account disabled" {
				wantStatusCalls = 1
			}
			if w.Code != wantStatus || !reflect.DeepEqual(users.profileIDs, []string{"user", "user"}) || len(access.statusIDs) != wantStatusCalls || len(access.nameIDs) != 1 {
				t.Fatalf("live check failed: status=%d users=%+v access=%+v", w.Code, users, access)
			}
		})
	}
}

func TestMerchantLoginCompatibility(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	jwtSvc, err := auth.NewService([]byte(strings.Repeat("x", 32)), "merchant-auth-test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, password, userStatus, merchantStatus string
		userErr, statusErr, touchErr               error
		nilUser                                    bool
		want                                       int
		message                                    string
		statusCalls                                int
	}{
		{name: "success", password: "correct-password", userStatus: "active", merchantStatus: "active", want: 200, statusCalls: 1},
		{name: "touch best effort", password: "correct-password", userStatus: "active", merchantStatus: "active", touchErr: errors.New("touch failed"), want: 200, statusCalls: 1},
		{name: "account missing", userErr: pgx.ErrNoRows, want: 401, message: "用户名或密码错误"},
		{name: "lookup failure mapping unchanged", userErr: errors.New("database unavailable"), want: 401, message: "用户名或密码错误"},
		{name: "nil account", nilUser: true, want: 401, message: "用户名或密码错误"},
		{name: "wrong password before disabled", password: "wrong-password", userStatus: "disabled", want: 401, message: "用户名或密码错误"},
		{name: "disabled", password: "correct-password", userStatus: "disabled", want: 403, message: "账号已禁用"},
		{name: "unknown account status", password: "correct-password", userStatus: "unknown", want: 403, message: "账号已禁用"},
		{name: "pending", password: "correct-password", userStatus: "active", merchantStatus: "pending", want: 403, message: "商户待审核", statusCalls: 1},
		{name: "suspended", password: "correct-password", userStatus: "active", merchantStatus: "suspended", want: 403, message: "商户已暂停", statusCalls: 1},
		{name: "merchant missing mapping unchanged", password: "correct-password", userStatus: "active", statusErr: pgx.ErrNoRows, want: 500, message: "服务内部错误", statusCalls: 1},
		{name: "merchant unavailable mapping unchanged", password: "correct-password", userStatus: "active", statusErr: context.DeadlineExceeded, want: 500, message: "服务内部错误", statusCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			users := &merchantAuthUsers{user: &model.MerchantUser{ID: "user", MerchantID: "merchant", PasswordHash: string(hash), Status: tt.userStatus}, err: tt.userErr, touchErr: tt.touchErr}
			if tt.nilUser {
				users.user = nil
			}
			access := &merchantAuthAccess{status: tt.merchantStatus, statusErr: tt.statusErr}
			h := NewMerchantAuthHandler(service.NewMerchantAuthService(users, access, jwtSvc))
			password := tt.password
			if password == "" {
				password = "correct-password"
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/merchant/auth/login", strings.NewReader(fmt.Sprintf(`{"username":"alice","password":%q}`, password)))
			r.RemoteAddr = "192.0.2.1:1234"
			w := httptest.NewRecorder()
			h.Login(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.want, w.Body)
			}
			if !reflect.DeepEqual(users.usernames, []string{"alice"}) || len(users.profileIDs) != 0 || len(access.statusIDs) != tt.statusCalls || len(access.nameIDs) != 0 {
				t.Fatalf("unexpected calls: users=%+v access=%+v", users, access)
			}
			if tt.want != 200 {
				var got api.Response
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				code := map[int]string{401: api.CodeUnauthorized, 403: api.CodeForbidden, 500: api.CodeInternal}[tt.want]
				if got.Success || got.Data != nil || got.ErrorCode != code || got.ErrorMessage != tt.message || len(users.touchIDs) != 0 {
					t.Fatalf("incompatible failure: %+v", got)
				}
				return
			}
			var got struct {
				Success bool              `json:"success"`
				Data    map[string]string `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !got.Success || len(got.Data) != 2 || got.Data["accessToken"] == "" || got.Data["refreshToken"] == "" {
				t.Fatalf("incompatible token response: %+v", got)
			}
			for _, token := range []struct {
				value string
				parse func(string) (*auth.Claims, error)
			}{{got.Data["accessToken"], jwtSvc.ParseAccess}, {got.Data["refreshToken"], jwtSvc.ParseRefresh}} {
				claims, err := token.parse(token.value)
				if err != nil {
					t.Fatal(err)
				}
				if claims.Subject != "user" || claims.UserID != "user" || claims.AccountID != "user" || claims.Tenant != "merchant" || len(claims.Roles) != 0 || len(claims.Permissions) != 0 || claims.IsSuper || claims.Scope != nil {
					t.Fatalf("incompatible claims: %+v", claims)
				}
			}
			if !reflect.DeepEqual(users.touchIDs, []string{"user"}) || !reflect.DeepEqual(users.touchIPs, []string{r.RemoteAddr}) {
				t.Fatalf("login audit changed: %+v", users)
			}
		})
	}
}
