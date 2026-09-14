package handler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func testJWT(t *testing.T) *auth.Service {
	t.Helper()
	svc, err := auth.NewService([]byte(strings.Repeat("test", 8)), "test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func testToken(t *testing.T, svc *auth.Service, grant auth.Grant) string {
	t.Helper()
	// SignGrant refuses to mint a token that does not say which realm it is for.
	// An absent realm means platform here for the same reason Parse resolves it
	// that way: these fixtures describe administrators, and the ones that do not
	// (a tenant, a mismatched subject) say so through fields this does not touch.
	if grant.Realm == "" {
		grant.Realm = auth.RealmPlatform
	}
	token, err := svc.SignAccessGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}

// fakeAdminAccess stands in for user-service's AdminAccessService. It records
// how the call was made so a test can assert the caller's token travelled in
// metadata and that the result was fetched rather than reused.
type fakeAdminAccess struct {
	resp *userv1.GetAdminAccessResponse
	err  error

	calls         int
	authorization string
	block         bool
}

func (f *fakeAdminAccess) GetAdminAccess(ctx context.Context, _ *userv1.GetAdminAccessRequest, _ ...grpc.CallOption) (*userv1.GetAdminAccessResponse, error) {
	f.calls++
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if values := md.Get(auth.MetadataAuthorization); len(values) > 0 {
			f.authorization = values[0]
		}
	}
	// A real client fails the RPC as soon as the call context is done.
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if f.block {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return f.resp, f.err
}

func testAdminAuthorizer(t *testing.T, client adminAccessClient, timeout time.Duration) *AdminAuthorizer {
	t.Helper()
	a, err := NewAdminAuthorizer(client, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// liveAccess answers with the given live grants for the admin user.
func liveAccess(t *testing.T, roles, permissions []string) (*AdminAuthorizer, *fakeAdminAccess) {
	t.Helper()
	fake := &fakeAdminAccess{resp: &userv1.GetAdminAccessResponse{UserId: "admin", Roles: roles, Permissions: permissions}}
	return testAdminAuthorizer(t, fake, time.Second), fake
}

func TestLiveAuthorization(t *testing.T) {
	svc := testJWT(t)
	for _, tt := range []struct {
		name               string
		grant              auth.Grant
		roles, permissions []string
		want               int
	}{
		{"revoked permission", auth.Grant{Permissions: []string{"admin:brands:view"}}, []string{}, []string{}, 403},
		{"revoked super", auth.Grant{IsSuper: true, Roles: []string{"super_admin"}}, []string{}, []string{}, 403},
		{"new permission", auth.Grant{}, []string{"editor"}, []string{"admin:brands:view"}, 204},
		{"new super", auth.Grant{}, []string{"super_admin"}, []string{}, 204},
		{"revoked super with retained permission", auth.Grant{IsSuper: true, Roles: []string{"super_admin"}}, []string{"editor"}, []string{"admin:brands:view"}, 204},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.grant.Subject, tt.grant.UserID = "admin", "admin"
			token := testToken(t, svc, tt.grant)
			wantSuper := slices.Contains(tt.roles, "super_admin")
			a, live := liveAccess(t, tt.roles, tt.permissions)
			calls := 0
			h := auth.Middleware(svc)(adminIdentity(a.middleware(auth.RequirePermission("admin:brands:view")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				id, _ := auth.IdentityFromRequest(r)
				if !reflect.DeepEqual(id.Roles, tt.roles) || !reflect.DeepEqual(id.Permissions, tt.permissions) {
					t.Errorf("stale context: %+v", id)
				}
				if id.IsSuper != wantSuper {
					t.Errorf("IsSuper=%v want %v", id.IsSuper, wantSuper)
				}
				if !wantSuper && id.AllowsMerchant("outside-scope") {
					t.Error("revoked super bypassed scope")
				}
				w.WriteHeader(204)
			})))))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if tt.want == 403 && calls != 0 {
				t.Fatal("denied request reached handler")
			}
			if live.authorization != token {
				t.Fatalf("forwarded authorization=%q want %q", live.authorization, token)
			}
		})
	}
}

// TestLiveAuthorizationFreshEveryRequest proves the lookup is not cached: a
// second identical request must ask user-service again, and the new answer must
// replace the previous grants.
func TestLiveAuthorizationFreshEveryRequest(t *testing.T) {
	svc := testJWT(t)
	token := testToken(t, svc, auth.Grant{Subject: "admin", UserID: "admin", IsSuper: true})
	fake := &fakeAdminAccess{resp: &userv1.GetAdminAccessResponse{UserId: "admin", Roles: []string{"super_admin"}}}
	a := testAdminAuthorizer(t, fake, time.Second)
	h := auth.Middleware(svc)(a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("request %d: status=%d", i, w.Code)
		}
	}
	if fake.calls != 2 {
		t.Fatalf("live calls=%d want 2", fake.calls)
	}
}

func TestLiveAuthorizationFailureResponses(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply func() (*userv1.GetAdminAccessResponse, error)
		want  int
	}{
		{"unauthenticated", func() (*userv1.GetAdminAccessResponse, error) { return nil, status.Error(codes.Unauthenticated, "no") }, 401},
		{"disabled admin", func() (*userv1.GetAdminAccessResponse, error) { return nil, status.Error(codes.PermissionDenied, "no") }, 403},
		{"internal error", func() (*userv1.GetAdminAccessResponse, error) { return nil, status.Error(codes.Internal, "boom") }, 503},
		{"unavailable", func() (*userv1.GetAdminAccessResponse, error) { return nil, status.Error(codes.Unavailable, "down") }, 503},
		{"deadline exceeded", func() (*userv1.GetAdminAccessResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "slow")
		}, 503},
		{"user id mismatch", func() (*userv1.GetAdminAccessResponse, error) {
			return &userv1.GetAdminAccessResponse{UserId: "other", Roles: []string{"super_admin"}}, nil
		}, 503},
		{"empty user id", func() (*userv1.GetAdminAccessResponse, error) { return &userv1.GetAdminAccessResponse{}, nil }, 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := tt.reply()
			assertAuthorizationStatus(t, testAdminAuthorizer(t, &fakeAdminAccess{resp: resp, err: err}, time.Second), tt.want)
		})
	}
}

func assertAuthorizationStatus(t *testing.T, a *AdminAuthorizer, want int) {
	t.Helper()
	svc := testJWT(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", testToken(t, svc, auth.Grant{Subject: "admin", UserID: "admin", IsSuper: true}))
	w := httptest.NewRecorder()
	auth.Middleware(svc)(adminIdentity(a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("failure reached business handler") })))).ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("status=%d want %d body=%s", w.Code, want, w.Body)
	}
}

func TestLiveAuthorizationUnavailable(t *testing.T) {
	t.Run("nil dependency", func(t *testing.T) { assertAuthorizationStatus(t, nil, 503) })
	t.Run("transport failure", func(t *testing.T) {
		// A closed listener yields the same transport failure as a dead peer.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		listener.Close()
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		assertAuthorizationStatus(t, testAdminAuthorizer(t, userv1.NewAdminAccessServiceClient(conn), time.Second), 503)
	})
	t.Run("timeout", func(t *testing.T) {
		a := testAdminAuthorizer(t, &fakeAdminAccess{resp: &userv1.GetAdminAccessResponse{UserId: "admin"}, block: true}, 20*time.Millisecond)
		assertAuthorizationStatus(t, a, 503)
	})
	t.Run("cancelled request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		a, _ := liveAccess(t, []string{"super_admin"}, []string{})
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		svc := testJWT(t)
		r.Header.Set("Authorization", testToken(t, svc, auth.Grant{Subject: "admin", UserID: "admin"}))
		w := httptest.NewRecorder()
		auth.Middleware(svc)(a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("cancelled request reached handler") }))).ServeHTTP(w, r)
		if w.Code != 503 {
			t.Fatalf("status=%d", w.Code)
		}
	})
}

func TestNewAdminAuthorizerValidation(t *testing.T) {
	if _, err := NewAdminAuthorizer(nil, time.Second); err == nil {
		t.Fatal("accepted a nil authorization client")
	}
	for _, timeout := range []time.Duration{0, -1} {
		if _, err := NewAdminAuthorizer(&fakeAdminAccess{}, timeout); err == nil {
			t.Fatal("accepted unbounded timeout")
		}
	}
}

func TestAdminRouteDenialMatrix(t *testing.T) {
	svc := testJWT(t)
	var liveCalls atomic.Int64
	fake := &fakeAdminAccess{resp: &userv1.GetAdminAccessResponse{UserId: "admin", Roles: []string{}, Permissions: []string{"unrelated:permission"}}}
	a := testAdminAuthorizer(t, countingAccess{inner: fake, calls: &liveCalls}, time.Second)
	s := runtime.NewHTTPRouter(khttp.NewServer())
	Register(s, &AdminMerchantHandler{}, &AdminBrandHandler{}, &AdminStoreHandler{}, NewAdminUploadHandler(nil, nil), svc, a)
	grants := []struct {
		name, token string
		want        int
		live        bool
	}{
		{"anonymous", "", 401, false},
		{"invalid jwt", "Bearer invalid", 401, false},
		{"revoked grants", testToken(t, svc, auth.Grant{Subject: "admin", UserID: "admin", IsSuper: true, Roles: []string{"super_admin"}, Permissions: []string{"admin:merchants:view", "admin:brands:manage", "admin:stores:delete"}}), 403, true},
		{"tenant", testToken(t, svc, auth.Grant{Subject: "admin", UserID: "admin", Tenant: "merchant", IsSuper: true}), 403, false},
		{"whitespace tenant", testToken(t, svc, auth.Grant{Subject: "admin", UserID: "admin", Tenant: " ", IsSuper: true}), 403, false},
		{"subject user mismatch", testToken(t, svc, auth.Grant{Subject: "other", UserID: "admin", IsSuper: true}), 401, false},
	}
	for _, resource := range []string{"merchants", "brands", "stores"} {
		routes := []struct{ method, suffix string }{{"GET", ""}, {"POST", ""}, {"GET", "/id"}, {"PUT", "/id"}, {"DELETE", "/id"}, {"PATCH", "/id/status"}}
		if resource != "merchants" {
			routes = append(routes, struct{ method, suffix string }{"PATCH", "/id/audit"})
		}
		for _, route := range routes {
			for _, grant := range grants {
				t.Run(fmt.Sprintf("%s %s%s/%s", route.method, resource, route.suffix, grant.name), func(t *testing.T) {
					before := liveCalls.Load()
					r := httptest.NewRequest(route.method, "/v1/admin/"+resource+route.suffix, nil)
					r.Header.Set("Authorization", grant.token)
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					if w.Code != grant.want {
						t.Fatalf("status=%d want %d body=%s", w.Code, grant.want, w.Body)
					}
					wantCalls := int64(0)
					if grant.live {
						wantCalls = 1
					}
					if liveCalls.Load()-before != wantCalls {
						t.Fatal("unexpected live authorization call count")
					}
				})
			}
		}
	}
}

// countingAccess counts live lookups while delegating the answer.
type countingAccess struct {
	inner adminAccessClient
	calls *atomic.Int64
}

func (c countingAccess) GetAdminAccess(ctx context.Context, in *userv1.GetAdminAccessRequest, opts ...grpc.CallOption) (*userv1.GetAdminAccessResponse, error) {
	c.calls.Add(1)
	return c.inner.GetAdminAccess(ctx, in, opts...)
}
