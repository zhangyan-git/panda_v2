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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
)

type roleRepoStub struct {
	existing  *model.AdminRole
	createErr error
	updateErr error
	created   *model.AdminRole
}

func (r *roleRepoStub) FindPage(context.Context, int, int) ([]*model.AdminRole, error) {
	return nil, nil
}

func (r *roleRepoStub) Count(context.Context) (int64, error) { return 0, nil }

func (r *roleRepoStub) FindByID(context.Context, string) (*model.AdminRole, error) {
	if r.existing == nil {
		return nil, pgx.ErrNoRows
	}
	copied := *r.existing
	return &copied, nil
}

func (r *roleRepoStub) Create(_ context.Context, role *model.AdminRole) error {
	if r.createErr != nil {
		return r.createErr
	}
	r.created = role
	return nil
}

func (r *roleRepoStub) Update(_ context.Context, role *model.AdminRole) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	r.created = role
	return nil
}

func (r *roleRepoStub) Delete(context.Context, string) error { return nil }

// okReloader 让保存路径走通，不覆盖 reload 失败场景（该场景由 service 层测试覆盖）。
type okReloader struct{ calls int }

func (o *okReloader) Reload() error {
	o.calls++
	return nil
}

func newRoleHandler(repo *roleRepoStub) *AdminRoleHandler {
	return NewAdminRoleHandler(service.NewAdminRoleService(repo, &okReloader{}))
}

func doRoleRequest(t *testing.T, h *AdminRoleHandler, method, body string) (*httptest.ResponseRecorder, api.Response) {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/admin/roles", strings.NewReader(body))
	rec := httptest.NewRecorder()
	if method == http.MethodPost {
		h.Create(rec, req)
	} else {
		h.Update(rec, req)
	}
	var resp api.Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rec, resp
}

// duplicateKeyErr 复现仓储把 PostgreSQL 23505 包进冲突错误的情形。
func duplicateKeyErr() error {
	return fmt.Errorf("%w: %w", model.ErrRoleCodeConflict,
		&pgconn.PgError{Code: "23505", ConstraintName: "admin_roles_code_key", Message: "duplicate key value violates unique constraint \"admin_roles_code_key\""})
}

func TestCreateRoleConflictReturns409WithoutLeakingSQL(t *testing.T) {
	h := newRoleHandler(&roleRepoStub{createErr: duplicateKeyErr()})
	rec, resp := doRoleRequest(t, h, http.MethodPost, `{"code":"ops_read","name":"只读","description":""}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if resp.ErrorCode != api.CodeConflict {
		t.Errorf("errorCode = %q, want %q", resp.ErrorCode, api.CodeConflict)
	}
	if resp.ErrorMessage != model.ErrRoleCodeConflict.Error() {
		t.Errorf("errorMessage = %q, want %q", resp.ErrorMessage, model.ErrRoleCodeConflict.Error())
	}
	for _, leak := range []string{"SQLSTATE", "23505", "duplicate key", "admin_roles_code_key", "pgconn"} {
		if strings.Contains(resp.ErrorMessage, leak) {
			t.Errorf("errorMessage leaks driver detail %q: %q", leak, resp.ErrorMessage)
		}
	}
}

func TestUpdateRoleConflictReturns409WithoutLeakingSQL(t *testing.T) {
	h := newRoleHandler(&roleRepoStub{
		existing:  &model.AdminRole{ID: "11111111-1111-1111-1111-111111111111", Code: "ops_read_old"},
		updateErr: duplicateKeyErr(),
	})
	rec, resp := doRoleRequest(t, h, http.MethodPut, `{"code":"ops_read","name":"只读","description":""}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	if resp.ErrorCode != api.CodeConflict {
		t.Errorf("errorCode = %q, want %q", resp.ErrorCode, api.CodeConflict)
	}
	if strings.Contains(resp.ErrorMessage, "23505") {
		t.Errorf("errorMessage leaks SQLSTATE: %q", resp.ErrorMessage)
	}
}

func TestRoleErrorsMapToStatusAndCode(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"invalid code", model.ErrInvalidRoleCode, http.StatusBadRequest, api.CodeInvalidRequest},
		{"reserved code", model.ErrReservedRoleCode, http.StatusBadRequest, api.CodeInvalidRequest},
		{"code conflict", model.ErrRoleCodeConflict, http.StatusConflict, api.CodeConflict},
		{"wrapped conflict", duplicateKeyErr(), http.StatusConflict, api.CodeConflict},
		{"missing role", pgx.ErrNoRows, http.StatusNotFound, api.CodeNotFound},
		{"wrapped missing role", fmt.Errorf("load role: %w", pgx.ErrNoRows), http.StatusNotFound, api.CodeNotFound},
		{"reload failure", fmt.Errorf("%w: %w", service.ErrRolePolicyReload, errors.New("boom")), http.StatusInternalServerError, api.CodeInternal},
		{"unknown failure", errors.New("boom"), http.StatusInternalServerError, api.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, message := roleHTTPError(tc.err, "创建失败")
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("got (%d, %q), want (%d, %q)", status, code, tc.wantStatus, tc.wantCode)
			}
			if message == "" {
				t.Error("message must not be empty")
			}
		})
	}
}

func TestCreateRoleReservedCodeIsRejected(t *testing.T) {
	repo := &roleRepoStub{}
	h := newRoleHandler(repo)
	rec, resp := doRoleRequest(t, h, http.MethodPost, `{"code":"super_admin","name":"冒充超管","description":""}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if repo.created != nil {
		t.Fatal("reserved code must not reach the repository")
	}
	if resp.ErrorMessage == "" {
		t.Error("expected a user-facing message")
	}
}
