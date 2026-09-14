package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	couponclient "github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/client"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// 这个文件把「coupon-service 的实时授权」当作一个整体来验：真实 JWT 中间件 +
// 商户闸门 + platform/authz + 权限码，前面接一枚真实签发的 token，后面接一个
// 真实的 user-service gRPC 服务端（bufconn，不走网络）。所以这里既不是单测
// 纯函数，也不需要 Postgres——但中间件的接线顺序、token 的传递、错误码的映射
// 都是真的。

const (
	testJWTSecret = "coupon-authorization-test-secret-32b"
	testIssuer    = "panda-test"
	// 所有用例共用一个权限码，等价于 /v1/admin/coupons/* 上挂的那个。
	testCode = "admin:coupons:view"
)

// fakeAdminAccess 扮演 user-service 的 AdminAccessService。它每次调用都现读自己
// 此刻的字段，所以「改了库里的东西」在测试里就是改这里——token 一个字节不动。
type fakeAdminAccess struct {
	userv1.UnimplementedAdminAccessServiceServer
	userID      string
	roles       []string
	permissions []string
	err         error

	calls  int
	tokens []string
	// 收到的 service token（若有）。coupon 不该带：它用的是调用者自己的 token。
	internalTokens []string
}

func (f *fakeAdminAccess) GetAdminAccess(ctx context.Context, _ *userv1.GetAdminAccessRequest) (*userv1.GetAdminAccessResponse, error) {
	f.calls++
	if token, ok := auth.AccessTokenFromMetadata(ctx); ok {
		f.tokens = append(f.tokens, token)
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		f.internalTokens = append(f.internalTokens, md.Get(auth.MetadataServiceToken)...)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &userv1.GetAdminAccessResponse{
		UserId:      f.userID,
		Roles:       f.roles,
		Permissions: f.permissions,
	}, nil
}

// couponAuthHarness 是 /v1/admin/coupons/* 的鉴权栈，只把最内层的业务处理换成
// 204——被验的是「能不能走到业务处理」。
type couponAuthHarness struct {
	users   *fakeAdminAccess
	handler http.Handler
}

func newCouponAuthHarness(t *testing.T) *couponAuthHarness {
	t.Helper()
	users := &fakeAdminAccess{userID: "admin"}
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	userv1.RegisterAdminAccessServiceServer(server, users)
	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("fake user-service stopped: %v", err)
		}
	}()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial fake user-service: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	jwtService, err := auth.NewService([]byte(testJWTSecret), testIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("init jwt: %v", err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return &couponAuthHarness{
		users:   users,
		handler: adminAuthorizer(jwtService, couponclient.NewAdminAccessResolver(conn).Resolve, 2*time.Second)(testCode)(next),
	}
}

func (h *couponAuthHarness) request(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/admin/coupons/types", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// adminToken 签一枚平台账号 token。claims 里可以塞任意权限与超管标记——测试正是
// 要证明这些**不被采信**：授权只看 user-service 的实时应答。
func adminToken(t *testing.T, grants auth.Grant) string {
	t.Helper()
	svc, err := auth.NewService([]byte(testJWTSecret), testIssuer, 24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("init jwt: %v", err)
	}
	// An absent realm means platform, matching how Parse resolves it; fixtures
	// describing a merchant token set RealmMerchant themselves.
	if grants.Realm == "" {
		grants.Realm = auth.RealmPlatform
	}
	token, err := svc.SignAccessGrant(grants)
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	return token
}

func platformGrant() auth.Grant {
	return auth.Grant{Realm: auth.RealmPlatform, Subject: "admin", UserID: "admin"}
}

// 核心：token 里写着有权限、user-service 说没有 → 403；把 user-service 的说法
// 改过来，同一枚 token 立刻变 204。改权限不需要重新登录，这正是本次改动的目的。
func TestCouponAdminAuthorizationIsLiveEveryRequest(t *testing.T) {
	h := newCouponAuthHarness(t)
	// token 的快照里带着这个权限，且自称超管——都不算数。
	token := adminToken(t, auth.Grant{Subject: "admin", UserID: "admin",
		Roles: []string{"super_admin"}, Permissions: []string{testCode}, IsSuper: true})

	h.users.roles = []string{"operator"}
	h.users.permissions = nil
	if w := h.request(t, token); w.Code != http.StatusForbidden {
		t.Fatalf("撤权后 status=%d want 403 body=%s", w.Code, w.Body)
	}

	// 库里授予同一权限：token 未换。
	h.users.permissions = []string{testCode}
	if w := h.request(t, token); w.Code != http.StatusNoContent {
		t.Fatalf("授权后 status=%d want 204 body=%s", w.Code, w.Body)
	}
	if h.users.calls != 2 {
		t.Fatalf("GetAdminAccess calls = %d, want 2: 每次请求都要问一次", h.users.calls)
	}
}

// 超管由实时角色决定：权限码列表是空的也全通，因为判定不查权限表。
func TestCouponSuperAdminPassesWithoutPermissionRows(t *testing.T) {
	h := newCouponAuthHarness(t)
	token := adminToken(t, platformGrant())

	h.users.roles = []string{authz.SuperRole}
	h.users.permissions = nil
	if w := h.request(t, token); w.Code != http.StatusNoContent {
		t.Fatalf("超管 status=%d want 204 body=%s", w.Code, w.Body)
	}
}

// 降级：解绑超管角色后，同一枚 token、同一次会话立即失去全权限。
func TestCouponSuperAdminDowngradeTakesEffectImmediately(t *testing.T) {
	h := newCouponAuthHarness(t)
	token := adminToken(t, auth.Grant{Subject: "admin", UserID: "admin",
		Roles: []string{"super_admin"}, IsSuper: true})

	h.users.roles = []string{"super_admin"}
	if w := h.request(t, token); w.Code != http.StatusNoContent {
		t.Fatalf("降级前 status=%d want 204 body=%s", w.Code, w.Body)
	}

	h.users.roles = []string{"operator"}
	h.users.permissions = nil
	if w := h.request(t, token); w.Code != http.StatusForbidden {
		t.Fatalf("降级后 status=%d want 403 body=%s", w.Code, w.Body)
	}
}

// user-service 的应答必须关于同一个用户；答非所问时既不用它也不回落 token。
func TestCouponRejectsAnswerAboutAnotherUser(t *testing.T) {
	h := newCouponAuthHarness(t)
	token := adminToken(t, auth.Grant{Subject: "admin", UserID: "admin",
		Permissions: []string{testCode}})

	h.users.userID = "someone-else"
	if w := h.request(t, token); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", w.Code, w.Body)
	}
}

// token 仍然有效，但 user-service 已经不认它了（账号停用/删除）→ 401。
func TestCouponMapsRejectedToken(t *testing.T) {
	h := newCouponAuthHarness(t)
	token := adminToken(t, auth.Grant{Subject: "admin", UserID: "admin",
		Permissions: []string{testCode}})

	h.users.err = status.Error(codes.Unauthenticated, "account not found")
	if w := h.request(t, token); w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", w.Code, w.Body)
	}
}

// 失败关闭：身份服务不可用 → 503，而不是拿 token 里的旧权限放行。
func TestCouponFailsClosedWhenIdentityServiceIsDown(t *testing.T) {
	h := newCouponAuthHarness(t)
	token := adminToken(t, auth.Grant{Subject: "admin", UserID: "admin",
		Roles: []string{"super_admin"}, Permissions: []string{testCode}, IsSuper: true})

	h.users.err = status.Error(codes.Unavailable, "connection refused")
	w := h.request(t, token)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503 body=%s", w.Code, w.Body)
	}
}

// 商户 token 在本地就被拒：不打 user-service（省一次跨服务调用），
// 也不因为没有平台账号而变成 503。
func TestCouponRejectsTenantTokenLocally(t *testing.T) {
	h := newCouponAuthHarness(t)
	token := adminToken(t, auth.Grant{Realm: auth.RealmMerchant, Subject: "merchant-user", UserID: "merchant-user", Tenant: "t1",
		Permissions: []string{testCode}})

	if w := h.request(t, token); w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", w.Code, w.Body)
	}
	if h.users.calls != 0 {
		t.Fatalf("GetAdminAccess calls = %d, want 0: 商户 token 不该走到实时查询", h.users.calls)
	}
}

// 实时查询用的是调用者自己的 token（不是服务 token）：user-service 会重新校验它，
// 所以调用方无法伪造成别人的身份。
func TestCouponForwardsCallerTokenToIdentityService(t *testing.T) {
	h := newCouponAuthHarness(t)
	token := adminToken(t, platformGrant())
	h.users.roles = []string{"super_admin"}

	if w := h.request(t, token); w.Code != http.StatusNoContent {
		t.Fatalf("status=%d want 204 body=%s", w.Code, w.Body)
	}
	if len(h.users.tokens) != 1 || h.users.tokens[0] != token {
		t.Fatalf("forwarded tokens = %v, want the caller's own token", h.users.tokens)
	}
	if len(h.users.internalTokens) != 0 {
		t.Fatalf("internal tokens = %v, want none: 这条 RPC 不需要服务 token", h.users.internalTokens)
	}
}
