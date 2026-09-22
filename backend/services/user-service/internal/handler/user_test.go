package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

type adminUserRepoStub struct {
	repository.AdminUserRepository
	created     *model.AdminUser
	createErr   error
	statusErr   error
	statusCalls int
}

func (r *adminUserRepoStub) Create(_ context.Context, u *model.AdminUser) error {
	if r.createErr != nil {
		return r.createErr
	}
	r.created = u
	return nil
}

func (r *adminUserRepoStub) UpdateStatus(context.Context, string, string) error {
	r.statusCalls++
	return r.statusErr
}

func newAdminUserHandler(repo *adminUserRepoStub) *AdminUserHandler {
	return NewAdminUserHandler(service.NewAdminUserService(repo))
}

// duplicateAdminUserErr 复现仓储把 PostgreSQL 23505 包进哨兵错误的情形，
// 形状与 repository/admin_user.go 的 adminUserWriteError 一致。
func duplicateAdminUserErr(sentinel error, constraint string) error {
	return fmt.Errorf("%w: %w", sentinel, &pgconn.PgError{
		Code:           "23505",
		ConstraintName: constraint,
		Message:        fmt.Sprintf("duplicate key value violates unique constraint %q", constraint),
	})
}

func TestCreateAdminUserConflictReturns409WithoutLeakingSQL(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		wantMessage   string
		wantNoLeakTag string
	}{
		{
			name:        "用户名重名",
			err:         duplicateAdminUserErr(model.ErrAdminUsernameTaken, "admin_users_username_key"),
			wantMessage: model.ErrAdminUsernameTaken.Error(),
		},
		{
			name:        "邮箱重名",
			err:         duplicateAdminUserErr(model.ErrAdminEmailTaken, "admin_users_email_key"),
			wantMessage: model.ErrAdminEmailTaken.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAdminUserHandler(&adminUserRepoStub{createErr: tc.err})
			body := `{"username":"alice","password":"pw123456","name":"Alice","email":"alice@example.test"}`
			rec := httptest.NewRecorder()
			h.Create(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/users", strings.NewReader(body)))

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusConflict, rec.Body)
			}
			var resp api.Response
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			if resp.ErrorCode != api.CodeConflict || resp.ErrorMessage != tc.wantMessage {
				t.Fatalf("unexpected envelope: %+v", resp)
			}
			// 回显驱动细节既暴露表结构，对后台用户也毫无用处。
			for _, leak := range []string{"SQLSTATE", "23505", "duplicate key", "admin_users_username_key", "admin_users_email_key", "pgconn"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("body leaked %q: %s", leak, rec.Body)
				}
			}
		})
	}
}

func TestCreateAdminUserUnknownFailureStays500(t *testing.T) {
	h := newAdminUserHandler(&adminUserRepoStub{createErr: errors.New("database unavailable")})
	body := `{"username":"alice","password":"pw123456","name":"Alice","email":"alice@example.test"}`
	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/users", strings.NewReader(body)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body)
	}
}

func TestCreateAdminUserSucceeds(t *testing.T) {
	repo := &adminUserRepoStub{}
	h := newAdminUserHandler(repo)
	body := `{"username":"alice","password":"pw123456","name":"Alice","email":"alice@example.test"}`
	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/users", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if repo.created == nil || repo.created.Username != "alice" {
		t.Fatalf("创建没有落到仓储: %+v", repo.created)
	}
	// 口令只以散列形式落库。
	if repo.created.PasswordHash == "pw123456" || repo.created.PasswordHash == "" {
		t.Fatal("口令散列不对")
	}
}

func TestUpdateAdminUserStatusMapsErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{"不存在的 id", fmt.Errorf("load admin user: %w", pgx.ErrNoRows), http.StatusNotFound, api.CodeNotFound, "用户不存在"},
		{"仓库不可用", errors.New("database unavailable"), http.StatusInternalServerError, api.CodeInternal, "操作失败"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &adminUserRepoStub{statusErr: tc.err}
			h := newAdminUserHandler(repo)
			r := httptest.NewRequest(http.MethodPatch, "/v1/admin/users/x/status", strings.NewReader(`{"status":"disabled"}`))
			r = mux.SetURLVars(r, map[string]string{"id": "11111111-1111-1111-1111-111111111111"})
			rec := httptest.NewRecorder()
			h.UpdateStatus(rec, r)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			var resp api.Response
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatal(err)
			}
			if resp.ErrorCode != tc.wantCode || resp.ErrorMessage != tc.wantMsg {
				t.Fatalf("unexpected envelope: %+v", resp)
			}
		})
	}
}
