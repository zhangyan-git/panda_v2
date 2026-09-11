package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

type meUsers struct {
	repository.AdminUserRepository
	user  *model.AdminUser
	err   error
	calls int
}

func (f *meUsers) FindByID(context.Context, string) (*model.AdminUser, error) {
	f.calls++
	return f.user, f.err
}

type meBindings struct {
	repository.AdminBindingRepository
	roles                      []*model.AdminRole
	permissions                []string
	roleErr, permissionErr     error
	roleCalls, permissionCalls int
}

func (f *meBindings) FindRolesByUser(context.Context, string) ([]*model.AdminRole, error) {
	f.roleCalls++
	return f.roles, f.roleErr
}
func (f *meBindings) FindPermissionCodesByUser(context.Context, string) ([]string, error) {
	f.permissionCalls++
	return f.permissions, f.permissionErr
}

func TestAdminMeRejectsBeforeLiveAccess(t *testing.T) {
	for _, tt := range []struct {
		name               string
		identity           *auth.Identity
		user               *model.AdminUser
		err                error
		want, profileCalls int
	}{
		{"unauthenticated", nil, nil, nil, 401, 0},
		{"tenant identity", &auth.Identity{Subject: "admin", UserID: "admin", Tenant: "merchant"}, nil, nil, 403, 0},
		{"whitespace tenant", &auth.Identity{Subject: "admin", UserID: "admin", Tenant: " "}, nil, nil, 403, 0},
		{"subject mismatch", &auth.Identity{Subject: "other", UserID: "admin"}, nil, nil, 401, 0},
		{"missing user id", &auth.Identity{Subject: "admin"}, nil, nil, 401, 0},
		{"disabled", &auth.Identity{Subject: "admin", UserID: "admin", IsSuper: true}, &model.AdminUser{ID: "admin", Status: "disabled"}, nil, 403, 1},
		{"unknown status", &auth.Identity{Subject: "admin", UserID: "admin"}, &model.AdminUser{ID: "admin"}, nil, 403, 1},
		{"missing admin", &auth.Identity{Subject: "admin", UserID: "admin"}, nil, pgx.ErrNoRows, 401, 1},
		{"repository unavailable", &auth.Identity{Subject: "admin", UserID: "admin"}, nil, errors.New("database unavailable"), 503, 1},
		{"nil profile", &auth.Identity{Subject: "admin", UserID: "admin"}, nil, nil, 503, 1},
		{"profile mismatch", &auth.Identity{Subject: "admin", UserID: "admin"}, &model.AdminUser{ID: "other", Status: "active"}, nil, 503, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			users := &meUsers{user: tt.user, err: tt.err}
			bindings := &meBindings{}
			h := NewAdminAuthHandler(service.NewAdminAuthService(users, bindings, nil))
			r := httptest.NewRequest(http.MethodGet, "/v1/admin/users/me", nil)
			if tt.identity != nil {
				r = r.WithContext(auth.WithIdentity(r.Context(), *tt.identity))
			}
			w := httptest.NewRecorder()
			h.Me(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if users.calls != tt.profileCalls || bindings.roleCalls != 0 || bindings.permissionCalls != 0 {
				t.Fatalf("unexpected repository calls: profile=%d roles=%d permissions=%d", users.calls, bindings.roleCalls, bindings.permissionCalls)
			}
		})
	}
}

func TestAdminMeReturnsCurrentAccessCompatibleResponse(t *testing.T) {
	users := &meUsers{user: &model.AdminUser{ID: "admin", Username: "alice", Name: "Alice", Email: "alice@example.test", Status: "active"}}
	bindings := &meBindings{roles: []*model.AdminRole{{Code: "editor"}}, permissions: []string{"admin:brands:view"}}
	h := NewAdminAuthHandler(service.NewAdminAuthService(users, bindings, nil))
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/users/me", nil)
	r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{Subject: "admin", UserID: "admin", IsSuper: true, Roles: []string{"super_admin"}, Permissions: []string{"revoked"}}))
	w := httptest.NewRecorder()
	h.Me(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var got struct {
		Success bool       `json:"success"`
		Data    meResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := meResponse{ID: "admin", Username: "alice", Name: "Alice", Email: "alice@example.test", Roles: []string{"editor"}, Permissions: []string{"admin:brands:view"}}
	if !got.Success || !reflect.DeepEqual(got.Data, want) {
		t.Fatalf("response=%+v want %+v", got, want)
	}
	if bindings.roleCalls != 1 || bindings.permissionCalls != 1 {
		t.Fatal("live bindings were not queried")
	}
}

func TestAdminMeLiveAccessFailure(t *testing.T) {
	for _, roleFailure := range []bool{true, false} {
		bindings := &meBindings{}
		if roleFailure {
			bindings.roleErr = errors.New("roles unavailable")
		} else {
			bindings.permissionErr = errors.New("permissions unavailable")
		}
		h := NewAdminAuthHandler(service.NewAdminAuthService(&meUsers{user: &model.AdminUser{ID: "admin", Status: "active"}}, bindings, nil))
		r := httptest.NewRequest(http.MethodGet, "/v1/admin/users/me", nil)
		r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{Subject: "admin", UserID: "admin"}))
		w := httptest.NewRecorder()
		h.Me(w, r)
		if w.Code != 500 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
}
