package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
)

func testService(t *testing.T) *auth.Service {
	t.Helper()
	svc, err := auth.NewService([]byte(strings.Repeat("test", 8)), "test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestMiddlewareReplacesGrantsWithLiveAnswer(t *testing.T) {
	svc := testService(t)
	for _, tt := range []struct {
		name        string
		grant       auth.Grant
		roles       []string
		permissions []string
		wantSuper   bool
		want        int
	}{
		{"revoked permission", auth.Grant{Realm: auth.RealmPlatform, Permissions: []string{"admin:brands:view"}}, []string{}, []string{}, false, 403},
		{"revoked super", auth.Grant{Realm: auth.RealmPlatform, IsSuper: true, Roles: []string{SuperRole}}, []string{}, []string{}, false, 403},
		{"new permission", auth.Grant{Realm: auth.RealmPlatform}, []string{"editor"}, []string{"admin:brands:view"}, false, 204},
		{"new super", auth.Grant{Realm: auth.RealmPlatform}, []string{SuperRole}, []string{}, true, 204},
		// A revoked super who kept an explicit permission is still allowed that
		// one permission, but is no longer a super.
		{"revoked super with retained permission", auth.Grant{Realm: auth.RealmPlatform, IsSuper: true, Roles: []string{SuperRole}}, []string{"editor"}, []string{"admin:brands:view"}, false, 204},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.grant.Subject, tt.grant.UserID = "admin", "admin"
			var seen auth.Identity
			handler := auth.RequirePermission("admin:brands:view")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen, _ = auth.IdentityFromRequest(r)
				w.WriteHeader(http.StatusNoContent)
			}))
			token, err := svc.SignAccessGrant(tt.grant)
			if err != nil {
				t.Fatal(err)
			}
			resolve := func(context.Context, string) (Grants, error) {
				return Grants{UserID: "admin", Roles: tt.roles, Permissions: tt.permissions}, nil
			}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			auth.Middleware(svc)(Middleware(resolve, time.Second)(handler)).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
			if tt.want != http.StatusNoContent {
				return
			}
			if !reflect.DeepEqual(seen.Roles, tt.roles) || !reflect.DeepEqual(seen.Permissions, tt.permissions) {
				t.Fatalf("stale identity: %+v", seen)
			}
			if seen.IsSuper != tt.wantSuper {
				t.Fatalf("IsSuper=%v want %v", seen.IsSuper, tt.wantSuper)
			}
		})
	}
}

// TestMiddlewareKeepsIdentityFields is the constraint that separates this from a
// rebuild: Subject, Tenant, AccountID and Scope describe who the caller is and
// must survive, because controllers attribute operations to Subject and read
// the scope to bound data access.
func TestMiddlewareKeepsIdentityFields(t *testing.T) {
	svc := testService(t)
	grant := auth.Grant{
		Realm:   auth.RealmPlatform,
		Subject: "admin", UserID: "admin",
		Roles: []string{"editor"}, Permissions: []string{"admin:brands:view"},
		Scope: auth.Scope{Type: auth.ScopeAll},
	}
	var seen auth.Identity
	resolve := func(context.Context, string) (Grants, error) {
		return Grants{UserID: "admin", Roles: []string{"viewer"}, Permissions: nil}, nil
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.IdentityFromRequest(r)
		w.WriteHeader(http.StatusNoContent)
	})
	token, err := svc.SignAccessGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	auth.Middleware(svc)(Middleware(resolve, time.Second)(handler)).ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if seen.Subject != "admin" || seen.Tenant != "" || seen.UserID != "admin" {
		t.Fatalf("identity fields were rebuilt: %+v", seen)
	}
	if !seen.AllowsAll() {
		t.Fatalf("scope was dropped: %+v", seen.Scope)
	}
	if !reflect.DeepEqual(seen.Roles, []string{"viewer"}) || len(seen.Permissions) != 0 {
		t.Fatalf("live grants not applied: %+v", seen)
	}
}

// TestMiddlewareFreshEveryRequest proves nothing is cached: two identical
// requests must ask twice, and the second answer must win.
func TestMiddlewareFreshEveryRequest(t *testing.T) {
	svc := testService(t)
	calls := 0
	resolve := func(context.Context, string) (Grants, error) {
		calls++
		if calls == 1 {
			return Grants{UserID: "admin", Roles: []string{SuperRole}}, nil
		}
		return Grants{UserID: "admin"}, nil
	}
	handler := auth.RequirePermission("admin:brands:view")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	stack := auth.Middleware(svc)(Middleware(resolve, time.Second)(handler))
	token, err := svc.SignAccessGrant(auth.Grant{Realm: auth.RealmPlatform, Subject: "admin", UserID: "admin", IsSuper: true, Roles: []string{SuperRole}})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{204, 403} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		stack.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("request %d: status=%d want %d", i, w.Code, want)
		}
	}
	if calls != 2 {
		t.Fatalf("resolver calls=%d want 2", calls)
	}
}

func TestMiddlewareFailureResponses(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply func() (Grants, error)
		want  int
	}{
		{"unauthenticated", func() (Grants, error) { return Grants{}, ErrUnauthenticated }, 401},
		{"forbidden", func() (Grants, error) { return Grants{}, ErrForbidden }, 403},
		{"internal error", func() (Grants, error) { return Grants{}, errors.New("boom") }, 503},
		{"unreachable", func() (Grants, error) { return Grants{}, context.DeadlineExceeded }, 503},
		{"user id mismatch", func() (Grants, error) { return Grants{UserID: "other", Roles: []string{SuperRole}}, nil }, 503},
		{"empty user id", func() (Grants, error) { return Grants{}, nil }, 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := testService(t)
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("failure reached business handler") })
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			token, err := svc.SignAccessGrant(auth.Grant{Realm: auth.RealmPlatform, Subject: "admin", UserID: "admin", IsSuper: true})
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			auth.Middleware(svc)(Middleware(func(context.Context, string) (Grants, error) { return tt.reply() }, time.Second)(handler)).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.want, w.Body)
			}
		})
	}
}

// TestMiddlewareFailsClosed covers the configurations that must never widen
// access. The identity is injected directly rather than through
// auth.Middleware so that the header branches are reachable — behind the real
// stack auth.Middleware rejects those requests first.
func TestMiddlewareFailsClosed(t *testing.T) {
	svc := testService(t)
	token, err := svc.SignAccessGrant(auth.Grant{Realm: auth.RealmPlatform, Subject: "admin", UserID: "admin", IsSuper: true, Permissions: []string{"admin:brands:view"}})
	if err != nil {
		t.Fatal(err)
	}
	identity := auth.Identity{Subject: "admin", UserID: "admin", IsSuper: true, Permissions: []string{"admin:brands:view"}}
	handler := auth.RequirePermission("admin:brands:view")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("unconfigured middleware reached handler")
	}))
	for _, tt := range []struct {
		name     string
		resolve  Resolver
		timeout  time.Duration
		header   string
		wantCode int
	}{
		{"nil resolver", nil, time.Second, "Bearer " + token, 503},
		{"zero timeout", func(context.Context, string) (Grants, error) { return Grants{UserID: "admin"}, nil }, 0, "Bearer " + token, 503},
		{"negative timeout", func(context.Context, string) (Grants, error) { return Grants{UserID: "admin"}, nil }, -time.Second, "Bearer " + token, 503},
		{"missing header", func(context.Context, string) (Grants, error) {
			return Grants{UserID: "admin", Roles: []string{SuperRole}}, nil
		}, time.Second, "", 503},
		{"malformed header", func(context.Context, string) (Grants, error) {
			return Grants{UserID: "admin", Roles: []string{SuperRole}}, nil
		}, time.Second, token, 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(auth.WithIdentity(context.Background(), identity))
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			w := httptest.NewRecorder()
			Middleware(tt.resolve, tt.timeout)(handler).ServeHTTP(w, r)
			if w.Code != tt.wantCode {
				t.Fatalf("status=%d want %d body=%s", w.Code, tt.wantCode, w.Body)
			}
		})
	}
	// No identity in context at all: nothing to authorize.
	t.Run("no identity", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		Middleware(func(context.Context, string) (Grants, error) {
			return Grants{UserID: "admin", Roles: []string{SuperRole}}, nil
		}, time.Second)(handler).ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401 body=%s", w.Code, w.Body)
		}
	})
}

// TestMiddlewareRejectsAConsumerToken is the regression the realm was added for:
// a C-end token has Subject == UserID and no tenant, so every structural check
// this middleware used to make called a signed-in miniapp customer an
// administrator. It is refused locally, which also means a customer's id never
// reaches the identity service.
func TestMiddlewareRejectsAConsumerToken(t *testing.T) {
	svc := testService(t)
	calls := 0
	resolve := func(context.Context, string) (Grants, error) {
		calls++
		return Grants{UserID: "user-1", Roles: []string{SuperRole}}, nil
	}
	token, err := svc.SignAccessGrant(auth.Grant{Realm: auth.RealmConsumer, Subject: "user-1", UserID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	auth.Middleware(svc)(Middleware(resolve, time.Second)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("C 端 token reached an administrator handler")
	}))).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403 body=%s", w.Code, w.Body)
	}
	if calls != 0 {
		t.Fatalf("resolver calls=%d want 0", calls)
	}
}

// TestMiddlewareRejectsMismatchedSubject proves the local pre-check: a token
// whose Subject is not its UserID is refused without ever asking the identity
// service.
func TestMiddlewareRejectsMismatchedSubject(t *testing.T) {
	svc := testService(t)
	calls := 0
	resolve := func(context.Context, string) (Grants, error) {
		calls++
		return Grants{UserID: "admin", Roles: []string{SuperRole}}, nil
	}
	token, err := svc.SignAccessGrant(auth.Grant{Realm: auth.RealmPlatform, Subject: "other", UserID: "admin", IsSuper: true})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	auth.Middleware(svc)(Middleware(resolve, time.Second)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("mismatched subject reached handler")
	}))).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%s", w.Code, w.Body)
	}
	if calls != 0 {
		t.Fatalf("resolver calls=%d want 0", calls)
	}
}

// TestMiddlewareRespectsRequestDeadline covers a client that hung up: the RPC
// context carries the caller's cancellation and the request fails closed rather
// than being served from the token.
func TestMiddlewareRespectsRequestDeadline(t *testing.T) {
	svc := testService(t)
	resolve := func(ctx context.Context, _ string) (Grants, error) {
		<-ctx.Done()
		return Grants{}, ctx.Err()
	}
	token, err := svc.SignAccessGrant(auth.Grant{Realm: auth.RealmPlatform, Subject: "admin", UserID: "admin", IsSuper: true})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	auth.Middleware(svc)(Middleware(resolve, 20*time.Millisecond)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("timed out request reached handler")
	}))).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
}
