package rpc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const (
	testServiceToken = "test-only-service-token-32-bytes-long"
	testJWTSecret    = "test-only-jwt-secret-at-least-32-bytes"
	testIssuer       = "user-service-test"
)

type fakeMerchantUsers struct {
	repository.MerchantUserRepository
	hasUsers bool
	hasErr   error
	hasIDs   []string
	resets   [][2]string
	resetErr error
}

func (f *fakeMerchantUsers) HasUsers(_ context.Context, merchantID string) (bool, error) {
	f.hasIDs = append(f.hasIDs, merchantID)
	return f.hasUsers, f.hasErr
}

func (f *fakeMerchantUsers) ResetScopeByTarget(_ context.Context, scopeType, scopeID string) error {
	f.resets = append(f.resets, [2]string{scopeType, scopeID})
	return f.resetErr
}

type fakeAdminUsers struct {
	repository.AdminUserRepository
	user   *model.AdminUser
	err    error
	lookup []string
}

func (f *fakeAdminUsers) FindByID(_ context.Context, id string) (*model.AdminUser, error) {
	f.lookup = append(f.lookup, id)
	return f.user, f.err
}

type fakeAdminBindings struct {
	repository.AdminBindingRepository
	roles    []*model.AdminRole
	perms    []string
	rolesErr error
	permsErr error
}

func (f *fakeAdminBindings) FindRolesByUser(context.Context, string) ([]*model.AdminRole, error) {
	return f.roles, f.rolesErr
}

func (f *fakeAdminBindings) FindPermissionCodesByUser(context.Context, string) ([]string, error) {
	return f.perms, f.permsErr
}

// harness 用 bufconn 起真实 gRPC 服务并装上与 main.go 相同的认证拦截器，
// 因此测试覆盖的就是生产链路里的服务 token / 用户 token 判定。
type harness struct {
	users      *fakeMerchantUsers
	adminUsers *fakeAdminUsers
	bindings   *fakeAdminBindings
	jwtSvc     *auth.Service
	userSvc    userv1.UserServiceClient
	access     userv1.AdminAccessServiceClient
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	jwtSvc, err := auth.NewService([]byte(testJWTSecret), testIssuer, time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		users:      &fakeMerchantUsers{},
		adminUsers: &fakeAdminUsers{user: &model.AdminUser{ID: "admin-1", Username: "admin", Status: "active"}},
		bindings:   &fakeAdminBindings{perms: []string{}},
		jwtSvc:     jwtSvc,
	}

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(jwtSvc, testServiceToken)))
	userv1.RegisterUserServiceServer(server, NewUserServiceServer(h.users))
	userv1.RegisterAdminAccessServiceServer(server, NewAdminAccessServiceServer(service.NewAdminAuthService(h.adminUsers, h.bindings, jwtSvc)))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	h.userSvc = userv1.NewUserServiceClient(conn)
	h.access = userv1.NewAdminAccessServiceClient(conn)
	return h
}

// serviceCtx 模拟 merchant-service 的服务身份调用。
func serviceCtx() context.Context {
	return auth.WithServiceToken(context.Background(), testServiceToken)
}

// userCtx 模拟携带终端用户 access token 的调用，grant 决定身份内容。
func (h *harness) userCtx(t *testing.T, grant auth.Grant) context.Context {
	t.Helper()
	token, err := h.jwtSvc.SignAccessGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithAccessToken(context.Background(), token)
}

func platformGrant() auth.Grant {
	return auth.Grant{Subject: "admin-1", UserID: "admin-1", AccountID: "admin-1"}
}

func TestUserServiceInternalRPCsRequireServiceToken(t *testing.T) {
	h := newHarness(t)
	userCtx := h.userCtx(t, platformGrant())
	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{"HasUsers", func(ctx context.Context) error {
			_, err := h.userSvc.HasUsers(ctx, &userv1.HasUsersRequest{MerchantId: "merchant-1"})
			return err
		}},
		{"ResetAccountScope", func(ctx context.Context) error {
			_, err := h.userSvc.ResetAccountScope(ctx, &userv1.ResetAccountScopeRequest{ScopeType: "brand", ScopeId: "brand-1"})
			return err
		}},
	} {
		t.Run(tc.name+"/no credentials", func(t *testing.T) {
			if code := status.Code(tc.call(context.Background())); code != codes.Unauthenticated {
				t.Fatalf("code = %v; want Unauthenticated", code)
			}
		})
		// 内部 RPC 不能被合法终端用户 token 调用
		t.Run(tc.name+"/user token", func(t *testing.T) {
			if code := status.Code(tc.call(userCtx)); code != codes.PermissionDenied {
				t.Fatalf("code = %v; want PermissionDenied", code)
			}
		})
	}
	if len(h.users.hasIDs) != 0 || len(h.users.resets) != 0 {
		t.Fatalf("rejected calls reached the repository: %v %v", h.users.hasIDs, h.users.resets)
	}
}

func TestGetAdminAccessRejectsServiceToken(t *testing.T) {
	h := newHarness(t)
	// 服务 token 没有用户身份，必须被拒绝，且不得查询任何账号数据
	_, err := h.access.GetAdminAccess(serviceCtx(), &userv1.GetAdminAccessRequest{})
	if code := status.Code(err); code != codes.Unauthenticated {
		t.Fatalf("code = %v; want Unauthenticated", code)
	}
	if len(h.adminUsers.lookup) != 0 {
		t.Fatalf("service-token caller reached account storage: %v", h.adminUsers.lookup)
	}
	if _, err := h.access.GetAdminAccess(context.Background(), &userv1.GetAdminAccessRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing credentials code = %v; want Unauthenticated", status.Code(err))
	}
}

func TestGetAdminAccessReturnsLiveGrants(t *testing.T) {
	h := newHarness(t)
	h.bindings.roles = []*model.AdminRole{{Code: "super_admin"}, {Code: "ops"}}
	h.bindings.perms = []string{"admin:users:view", "admin:merchants:manage"}

	resp, err := h.access.GetAdminAccess(h.userCtx(t, platformGrant()), &userv1.GetAdminAccessRequest{})
	if err != nil {
		t.Fatalf("GetAdminAccess: %v", err)
	}
	if resp.GetUserId() != "admin-1" {
		t.Fatalf("user_id = %q; want admin-1", resp.GetUserId())
	}
	if len(resp.GetRoles()) != 2 || resp.GetRoles()[0] != "super_admin" || resp.GetRoles()[1] != "ops" {
		t.Fatalf("roles = %v; want the live role codes", resp.GetRoles())
	}
	if len(resp.GetPermissions()) != 2 || resp.GetPermissions()[0] != "admin:users:view" {
		t.Fatalf("permissions = %v; want the live permission codes", resp.GetPermissions())
	}
	if len(h.adminUsers.lookup) != 1 || h.adminUsers.lookup[0] != "admin-1" {
		t.Fatalf("account lookups = %v; want the token's own user id", h.adminUsers.lookup)
	}
}

// GetAdminAccess 复用 /v1/admin/users/me 的判定：身份不符、账号缺失或禁用都必须拒绝
func TestGetAdminAccessFailClosed(t *testing.T) {
	missing := &model.AdminUser{}
	for _, tc := range []struct {
		name       string
		grant      auth.Grant
		user       *model.AdminUser
		userErr    error
		rolesErr   error
		permsErr   error
		wantCode   codes.Code
		wantLookup int
	}{
		{
			name: "subject mismatch", grant: auth.Grant{Subject: "other", UserID: "admin-1"},
			user: &model.AdminUser{ID: "admin-1", Status: "active"}, wantCode: codes.Unauthenticated,
		},
		{
			name: "merchant tenant", grant: auth.Grant{Subject: "admin-1", UserID: "admin-1", Tenant: "merchant-1"},
			user: &model.AdminUser{ID: "admin-1", Status: "active"}, wantCode: codes.PermissionDenied,
		},
		{
			name: "missing account", grant: platformGrant(), userErr: pgx.ErrNoRows,
			wantCode: codes.Unauthenticated, wantLookup: 1,
		},
		{
			name: "account unavailable", grant: platformGrant(), userErr: errors.New("lookup failed"),
			wantCode: codes.Unavailable, wantLookup: 1,
		},
		{
			name: "id mismatch", grant: platformGrant(), user: missing,
			wantCode: codes.Unavailable, wantLookup: 1,
		},
		{
			name: "disabled account", grant: platformGrant(), user: &model.AdminUser{ID: "admin-1", Status: "disabled"},
			wantCode: codes.PermissionDenied, wantLookup: 1,
		},
		{
			name: "roles unavailable", grant: platformGrant(), user: &model.AdminUser{ID: "admin-1", Status: "active"},
			rolesErr: errors.New("roles failed"), wantCode: codes.Internal, wantLookup: 1,
		},
		{
			name: "permissions unavailable", grant: platformGrant(), user: &model.AdminUser{ID: "admin-1", Status: "active"},
			permsErr: errors.New("perms failed"), wantCode: codes.Internal, wantLookup: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.adminUsers.user, h.adminUsers.err = tc.user, tc.userErr
			h.bindings.rolesErr, h.bindings.permsErr = tc.rolesErr, tc.permsErr
			resp, err := h.access.GetAdminAccess(h.userCtx(t, tc.grant), &userv1.GetAdminAccessRequest{})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v (%v); want %v", status.Code(err), err, tc.wantCode)
			}
			if resp != nil || err == nil {
				t.Fatalf("response = %v, %v; want no response and an error", resp, err)
			}
			if len(h.adminUsers.lookup) != tc.wantLookup {
				t.Fatalf("account lookups = %d; want %d", len(h.adminUsers.lookup), tc.wantLookup)
			}
		})
	}
}

func TestHasUsersDelegatesToRepository(t *testing.T) {
	for _, hasUsers := range []bool{true, false} {
		h := newHarness(t)
		h.users.hasUsers = hasUsers
		resp, err := h.userSvc.HasUsers(serviceCtx(), &userv1.HasUsersRequest{MerchantId: "merchant-1"})
		if err != nil {
			t.Fatalf("HasUsers: %v", err)
		}
		if resp.GetHasUsers() != hasUsers {
			t.Fatalf("has_users = %v; want %v", resp.GetHasUsers(), hasUsers)
		}
		if len(h.users.hasIDs) != 1 || h.users.hasIDs[0] != "merchant-1" {
			t.Fatalf("queried merchants = %v; want merchant-1", h.users.hasIDs)
		}
	}
	h := newHarness(t)
	h.users.hasErr = errors.New("query failed")
	if _, err := h.userSvc.HasUsers(serviceCtx(), &userv1.HasUsersRequest{MerchantId: "merchant-1"}); status.Code(err) != codes.Internal {
		t.Fatalf("repository failure code = %v; want Internal", status.Code(err))
	}
}

func TestResetAccountScopeValidation(t *testing.T) {
	for _, scopeType := range []string{"", "merchant", "region", "Brand", "STORE", "brand "} {
		t.Run("scope_type/"+scopeType, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.userSvc.ResetAccountScope(serviceCtx(), &userv1.ResetAccountScopeRequest{ScopeType: scopeType, ScopeId: "target-1"})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v (%v); want InvalidArgument", status.Code(err), err)
			}
			if len(h.users.resets) != 0 {
				t.Fatalf("invalid scope_type reset %v; want fail closed", h.users.resets)
			}
		})
	}
	for _, scopeType := range []string{"brand", "store"} {
		t.Run("empty scope_id/"+scopeType, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.userSvc.ResetAccountScope(serviceCtx(), &userv1.ResetAccountScopeRequest{ScopeType: scopeType})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v (%v); want InvalidArgument", status.Code(err), err)
			}
			if len(h.users.resets) != 0 {
				t.Fatalf("empty scope_id reset %v; want fail closed", h.users.resets)
			}
		})
	}
	for _, scopeType := range []string{"brand", "store"} {
		t.Run("valid/"+scopeType, func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.userSvc.ResetAccountScope(serviceCtx(), &userv1.ResetAccountScopeRequest{ScopeType: scopeType, ScopeId: "target-1"}); err != nil {
				t.Fatalf("ResetAccountScope: %v", err)
			}
			if len(h.users.resets) != 1 || h.users.resets[0] != [2]string{scopeType, "target-1"} {
				t.Fatalf("resets = %v; want one %s/target-1 call", h.users.resets, scopeType)
			}
		})
	}
	h := newHarness(t)
	h.users.resetErr = errors.New("reset failed")
	if _, err := h.userSvc.ResetAccountScope(serviceCtx(), &userv1.ResetAccountScopeRequest{ScopeType: "brand", ScopeId: "target-1"}); status.Code(err) != codes.Internal {
		t.Fatalf("repository failure code = %v; want Internal", status.Code(err))
	}
}

// GetProfile/UpdateProfile 目前没有调用方，必须保持未实现，而不是返回空资料
func TestProfileRPCsStayUnimplemented(t *testing.T) {
	h := newHarness(t)
	if _, err := h.userSvc.GetProfile(serviceCtx(), &userv1.GetProfileRequest{UserId: "user-1"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("GetProfile code = %v; want Unimplemented", status.Code(err))
	}
	if _, err := h.userSvc.UpdateProfile(serviceCtx(), &userv1.UpdateProfileRequest{UserId: "user-1"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("UpdateProfile code = %v; want Unimplemented", status.Code(err))
	}
}
