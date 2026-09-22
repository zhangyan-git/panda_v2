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
	// 商户端鉴权走的是同一张表的按 id 正查，与 /v1/merchant/users/me 同一条路径。
	user    *model.MerchantUser
	userErr error
	lookup  []string
}

func (f *fakeMerchantUsers) HasUsers(_ context.Context, merchantID string) (bool, error) {
	f.hasIDs = append(f.hasIDs, merchantID)
	return f.hasUsers, f.hasErr
}

func (f *fakeMerchantUsers) ResetScopeByTarget(_ context.Context, scopeType, scopeID string) error {
	f.resets = append(f.resets, [2]string{scopeType, scopeID})
	return f.resetErr
}

func (f *fakeMerchantUsers) FindByID(_ context.Context, id string) (*model.MerchantUser, error) {
	f.lookup = append(f.lookup, id)
	return f.user, f.userErr
}

// fakeMerchantStatus 是商户主体状态/名称的桩。
type fakeMerchantStatus struct {
	status, name       string
	statusErr, nameErr error
	statusIDs          []string
}

func (f *fakeMerchantStatus) FindStatus(_ context.Context, merchantID string) (string, error) {
	f.statusIDs = append(f.statusIDs, merchantID)
	return f.status, f.statusErr
}

func (f *fakeMerchantStatus) FindName(_ context.Context, merchantID string) (string, error) {
	return f.name, f.nameErr
}

// fakeMerchantResources 是范围展开的桩，记录调用次数：失败路径上不该发生的那些调用
// 只能靠计数器看见。
type fakeMerchantResources struct {
	service.MerchantResourceAccess
	storeIDs  []string
	listErr   error
	listCalls int
}

func (f *fakeMerchantResources) ListStoreIDs(_ context.Context, _, _, _ string) ([]string, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.storeIDs, nil
}

// fakeConsumerUsers 是 C 端用户仓库的桩，只实现微信身份那条正查。
type fakeConsumerUsers struct {
	repository.UserRepository
	identity *model.UserWechatIdentity
	err      error
	// lookups 记下每次正查的 (user_id, app_type)，用来断言被拒绝的调用没有落到仓库上。
	lookups [][2]string
}

func (f *fakeConsumerUsers) FindWechatIdentityByUser(_ context.Context, userID, appType string) (*model.UserWechatIdentity, error) {
	f.lookups = append(f.lookups, [2]string{userID, appType})
	if f.err != nil {
		return nil, f.err
	}
	return f.identity, nil
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
	users         *fakeMerchantUsers
	consumerUsers *fakeConsumerUsers
	adminUsers    *fakeAdminUsers
	bindings      *fakeAdminBindings
	merchantStat  *fakeMerchantStatus
	resources     *fakeMerchantResources
	jwtSvc        *auth.Service
	userSvc       userv1.UserServiceClient
	access        userv1.AdminAccessServiceClient
	merchantAcc   userv1.MerchantAccessServiceClient
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	jwtSvc, err := auth.NewService([]byte(testJWTSecret), testIssuer, time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		users:         &fakeMerchantUsers{},
		consumerUsers: &fakeConsumerUsers{},
		adminUsers:    &fakeAdminUsers{user: &model.AdminUser{ID: "admin-1", Username: "admin", Status: "active"}},
		bindings:      &fakeAdminBindings{perms: []string{}},
		merchantStat:  &fakeMerchantStatus{status: "active", name: "Coffee"},
		resources:     &fakeMerchantResources{storeIDs: []string{}},
		jwtSvc:        jwtSvc,
	}

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(jwtSvc, testServiceToken)))
	userv1.RegisterUserServiceServer(server, NewUserServiceServer(h.users, h.consumerUsers))
	// 会话仓库传 nil：这里的 AdminAccessService 只做 Profile / LiveAccess，
	// 既不发令牌也不撤会话，走不到需要会话表的那两条路径。
	userv1.RegisterAdminAccessServiceServer(server, NewAdminAccessServiceServer(service.NewAdminAuthService(h.adminUsers, h.bindings, nil, jwtSvc)))
	// 与 main.go 一样，数据边界的判定复用登录链路那个 MerchantAuthService。
	merchantAuthSvc := service.NewMerchantAuthService(h.users, h.merchantStat, nil)
	userv1.RegisterMerchantAccessServiceServer(server, NewMerchantAccessServiceServer(service.NewMerchantAccessService(h.users, merchantAuthSvc, h.resources)))
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
	h.merchantAcc = userv1.NewMerchantAccessServiceClient(conn)
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
	return auth.Grant{Realm: auth.RealmPlatform, Subject: "admin-1", UserID: "admin-1", AccountID: "admin-1"}
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
		// openid 是能替别人发起支付的东西，终端用户 token 更不能调它
		{"GetWechatIdentity", func(ctx context.Context) error {
			_, err := h.userSvc.GetWechatIdentity(ctx, &userv1.GetWechatIdentityRequest{
				UserId: "user-1", AppType: model.WechatAppMiniapp,
			})
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
	if len(h.users.hasIDs) != 0 || len(h.users.resets) != 0 || len(h.consumerUsers.lookups) != 0 {
		t.Fatalf("rejected calls reached the repository: %v %v %v",
			h.users.hasIDs, h.users.resets, h.consumerUsers.lookups)
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
			name: "subject mismatch", grant: auth.Grant{Realm: auth.RealmPlatform, Subject: "other", UserID: "admin-1"},
			user: &model.AdminUser{ID: "admin-1", Status: "active"}, wantCode: codes.Unauthenticated,
		},
		{
			name: "merchant tenant", grant: auth.Grant{Realm: auth.RealmPlatform, Subject: "admin-1", UserID: "admin-1", Tenant: "merchant-1"},
			user: &model.AdminUser{ID: "admin-1", Status: "active"}, wantCode: codes.PermissionDenied,
		},
		{
			// 形状与管理员一致，只有 realm 不同：这条守住的就是那个区别。
			name: "consumer token", grant: auth.Grant{Realm: auth.RealmConsumer, Subject: "user-1", UserID: "user-1"},
			user: &model.AdminUser{ID: "user-1", Status: "active"}, wantCode: codes.PermissionDenied,
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

func merchantGrant() auth.Grant {
	return auth.Grant{Realm: auth.RealmMerchant, Subject: "user-1", UserID: "user-1", AccountID: "user-1", Tenant: "merchant-1"}
}

// 数据边界的答案里带着一个商户的完整授权点位，只有这个账号本人能问。服务令牌没有账号，
// 必须被拒在查询之前。
func TestGetMerchantAccessRejectsNonMerchantCallers(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
		code codes.Code
	}{
		{name: "no credentials", ctx: context.Background, code: codes.Unauthenticated},
		{name: "service token", ctx: serviceCtx, code: codes.Unauthenticated},
		{name: "platform token", ctx: func() context.Context { return h.userCtx(t, platformGrant()) }, code: codes.PermissionDenied},
		{name: "consumer token", ctx: func() context.Context {
			return h.userCtx(t, auth.Grant{Realm: auth.RealmConsumer, Subject: "user-1", UserID: "user-1"})
		}, code: codes.PermissionDenied},
		{
			// 商户令牌没有 tenant 就指不出是哪个商户，也就没有账号可比对。
			name: "merchant token without tenant",
			ctx: func() context.Context {
				return h.userCtx(t, auth.Grant{Realm: auth.RealmMerchant, Subject: "user-1", UserID: "user-1"})
			},
			code: codes.PermissionDenied,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.merchantAcc.GetMerchantAccess(tc.ctx(), &userv1.GetMerchantAccessRequest{})
			if status.Code(err) != tc.code {
				t.Fatalf("code = %v (%v); want %v", status.Code(err), err, tc.code)
			}
			if resp != nil || len(h.users.lookup) != 0 || h.resources.listCalls != 0 {
				t.Fatalf("被拒的调用摸到了账号或范围: resp=%v lookups=%v expansions=%d", resp, h.users.lookup, h.resources.listCalls)
			}
		})
	}
}

func TestGetMerchantAccessReturnsTheExpandedBoundary(t *testing.T) {
	h := newHarness(t)
	h.users.user = &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active", ScopeType: "brand", ScopeID: "brand-1"}
	h.resources.storeIDs = []string{"store-1", "store-2"}

	resp, err := h.merchantAcc.GetMerchantAccess(h.userCtx(t, merchantGrant()), &userv1.GetMerchantAccessRequest{})
	if err != nil {
		t.Fatalf("GetMerchantAccess: %v", err)
	}
	if resp.GetMerchantId() != "merchant-1" || resp.GetScopeType() != "brand" || resp.GetScopeId() != "brand-1" {
		t.Fatalf("boundary = %+v", resp)
	}
	if len(resp.GetStoreIds()) != 2 || resp.GetStoreIds()[0] != "store-1" {
		t.Fatalf("store_ids = %v; want the expanded set", resp.GetStoreIds())
	}
	if len(h.users.lookup) != 1 || h.users.lookup[0] != "user-1" {
		t.Fatalf("account lookups = %v; want the token's own user id", h.users.lookup)
	}
}

// 这一条是本刀的核心：范围改了不用重新登录，下一枚同样的令牌就必须给出新的答案。
func TestGetMerchantAccessFollowsScopeChangesWithoutReissuingTheToken(t *testing.T) {
	h := newHarness(t)
	h.users.user = &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active", ScopeType: "merchant"}
	h.resources.storeIDs = []string{"store-1", "store-2", "store-3"}
	ctx := h.userCtx(t, merchantGrant())

	resp, err := h.merchantAcc.GetMerchantAccess(ctx, &userv1.GetMerchantAccessRequest{})
	if err != nil || len(resp.GetStoreIds()) != 3 {
		t.Fatalf("首次应给出全部门店: %v %v", resp.GetStoreIds(), err)
	}

	h.users.user.ScopeType, h.users.user.ScopeID = "store", "store-3"
	h.resources.storeIDs = []string{"store-3"}
	resp, err = h.merchantAcc.GetMerchantAccess(ctx, &userv1.GetMerchantAccessRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetScopeType() != "store" || len(resp.GetStoreIds()) != 1 || resp.GetStoreIds()[0] != "store-3" {
		t.Fatalf("收窄后应立即只回一个点位: %+v", resp)
	}
}

// 每一条失败都必须是拒绝，而不是一份更小的边界——尤其是展开失败那条：回空集会让下游
// 把「答不了」显示成「你没有点位」。
func TestGetMerchantAccessFailClosed(t *testing.T) {
	missing := &model.MerchantUser{}
	for _, tc := range []struct {
		name       string
		grant      auth.Grant
		user       *model.MerchantUser
		userErr    error
		status     string
		statusErr  error
		scopeType  string
		listErr    error
		wantCode   codes.Code
		wantExpand int
	}{
		{
			name:     "subject mismatch",
			grant:    auth.Grant{Realm: auth.RealmMerchant, Subject: "other", UserID: "user-1", Tenant: "merchant-1"},
			user:     &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active"},
			wantCode: codes.Unauthenticated,
		},
		{name: "account deleted", grant: merchantGrant(), userErr: pgx.ErrNoRows, status: "active", wantCode: codes.Unauthenticated},
		// 行读出来了却没有商户可锚：不认识的形状一律按「这个账号不成立」拒掉。
		{name: "row without a merchant", grant: merchantGrant(), user: missing, status: "active", wantCode: codes.PermissionDenied},
		{
			name: "tenant mismatch", grant: merchantGrant(), status: "active",
			user:     &model.MerchantUser{ID: "user-1", MerchantID: "merchant-2", Status: "active"},
			wantCode: codes.PermissionDenied,
		},
		{
			name: "disabled account", grant: merchantGrant(), status: "active",
			user:     &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "disabled"},
			wantCode: codes.PermissionDenied,
		},
		{
			name: "pending merchant", grant: merchantGrant(), status: "pending",
			user:     &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active"},
			wantCode: codes.PermissionDenied,
		},
		{
			name: "suspended merchant", grant: merchantGrant(), status: "suspended",
			user:     &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active"},
			wantCode: codes.PermissionDenied,
		},
		{
			name: "merchant status unavailable", grant: merchantGrant(), statusErr: context.DeadlineExceeded,
			user:     &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active"},
			wantCode: codes.Unavailable,
		},
		{
			// 认不出的档位既不能读成全量也不能读成空集，而且不该白跑一次展开。
			name: "unreadable scope type", grant: merchantGrant(), status: "active",
			user:     &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active", ScopeType: "region", ScopeID: "r1"},
			wantCode: codes.Unavailable,
		},
		{
			name: "expansion failed", grant: merchantGrant(), status: "active", scopeType: "brand",
			user:       &model.MerchantUser{ID: "user-1", MerchantID: "merchant-1", Status: "active", ScopeType: "brand", ScopeID: "brand-1"},
			listErr:    errors.New("merchant service unavailable"),
			wantCode:   codes.Unavailable,
			wantExpand: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.users.user, h.users.userErr = tc.user, tc.userErr
			if tc.status != "" {
				h.merchantStat.status = tc.status
			}
			h.merchantStat.statusErr = tc.statusErr
			h.resources.listErr = tc.listErr
			h.resources.storeIDs = []string{"store-1"}

			resp, err := h.merchantAcc.GetMerchantAccess(h.userCtx(t, tc.grant), &userv1.GetMerchantAccessRequest{})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v (%v); want %v", status.Code(err), err, tc.wantCode)
			}
			if resp != nil {
				t.Fatalf("拒绝时不能回边界: %+v", resp)
			}
			if h.resources.listCalls != tc.wantExpand {
				t.Fatalf("范围展开调用 = %d; want %d", h.resources.listCalls, tc.wantExpand)
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

// GetWechatIdentity 是 order-service 发起微信支付时取 openid 的那一次调用。
func TestGetWechatIdentityReturnsTheBinding(t *testing.T) {
	h := newHarness(t)
	h.consumerUsers.identity = &model.UserWechatIdentity{
		ID: "ident-1", UserID: "user-1", AppType: model.WechatAppMiniapp,
		OpenID: "openid-1", UnionID: "unionid-1",
	}
	resp, err := h.userSvc.GetWechatIdentity(serviceCtx(), &userv1.GetWechatIdentityRequest{
		UserId: "user-1", AppType: model.WechatAppMiniapp,
	})
	if err != nil {
		t.Fatalf("GetWechatIdentity: %v", err)
	}
	if resp.GetOpenid() != "openid-1" || resp.GetUnionid() != "unionid-1" {
		t.Fatalf("identity = %q/%q; want openid-1/unionid-1", resp.GetOpenid(), resp.GetUnionid())
	}
	// 正查的 key 必须原样带上 app_type：漏了它就成了「这个人随便哪个应用的 openid」，
	// 而 openid 是按应用独立的，发错了渠道只会回一句看不懂的错。
	if len(h.consumerUsers.lookups) != 1 || h.consumerUsers.lookups[0] != [2]string{"user-1", model.WechatAppMiniapp} {
		t.Fatalf("lookups = %v; want user-1/miniapp", h.consumerUsers.lookups)
	}
}

// 「没有绑定」与「问不到」是两种结局：前者调用方该换一种支付方式（NotFound），
// 后者重发一次可能就好（Internal）。混成一个会让一次库故障看起来像「这个用户没绑微信」。
func TestGetWechatIdentitySeparatesMissingFromUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{name: "no binding", err: pgx.ErrNoRows, wantCode: codes.NotFound},
		{name: "repository failure", err: errors.New("query failed"), wantCode: codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.consumerUsers.err = tc.err
			resp, err := h.userSvc.GetWechatIdentity(serviceCtx(), &userv1.GetWechatIdentityRequest{
				UserId: "user-1", AppType: model.WechatAppMiniapp,
			})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("code = %v (%v); want %v", status.Code(err), err, tc.wantCode)
			}
			if resp != nil || err == nil {
				t.Fatalf("response = %v, %v; want no response and an error", resp, err)
			}
		})
	}
}

// 形状不合法的请求必须在碰库之前被拒：app_type 写错时返回空 openid 会被调用方读成
// 「这个人没绑定微信」，进而让用户去换支付方式——而真正的问题是调用方传错了参数。
func TestGetWechatIdentityValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *userv1.GetWechatIdentityRequest
	}{
		{name: "empty user_id", req: &userv1.GetWechatIdentityRequest{AppType: model.WechatAppMiniapp}},
		{name: "empty app_type", req: &userv1.GetWechatIdentityRequest{UserId: "user-1"}},
		{name: "unknown app_type", req: &userv1.GetWechatIdentityRequest{UserId: "user-1", AppType: "wechat"}},
		{name: "uppercase app_type", req: &userv1.GetWechatIdentityRequest{UserId: "user-1", AppType: "Miniapp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.userSvc.GetWechatIdentity(serviceCtx(), tc.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v (%v); want InvalidArgument", status.Code(err), err)
			}
			if len(h.consumerUsers.lookups) != 0 {
				t.Fatalf("invalid request reached the repository: %v", h.consumerUsers.lookups)
			}
		})
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
