package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
)

// merchantGrant is the shape a real merchant token has: a tenant, the merchant
// realm, and nothing else — merchant accounts carry no roles or permissions.
func merchantGrant(tenant string) auth.Grant {
	return auth.Grant{
		Subject:   "account-1",
		UserID:    "account-1",
		AccountID: "account-1",
		Tenant:    tenant,
		Realm:     auth.RealmMerchant,
	}
}

// runMerchantMiddleware drives the full chain the way a routed request does:
// auth.Middleware verifies the token, then MerchantMiddleware resolves the
// boundary. It returns the status and whatever scope the handler saw.
func runMerchantMiddleware(t *testing.T, grant auth.Grant, resolve MerchantResolver, timeout time.Duration) (int, auth.StoreScope, bool, bool) {
	t.Helper()
	svc := testService(t)
	token, err := svc.SignAccessGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	var (
		seen   auth.StoreScope
		seenOK bool
		called bool
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		seen, seenOK = auth.StoreScopeFromRequest(r)
		w.WriteHeader(http.StatusNoContent)
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	auth.Middleware(svc)(MerchantMiddleware(resolve, timeout)(handler)).ServeHTTP(w, r)
	return w.Code, seen, seenOK, called
}

func okResolver(grants MerchantGrants) MerchantResolver {
	return func(context.Context, string) (MerchantGrants, error) { return grants, nil }
}

func TestMerchantMiddlewareRejectsBeforeResolving(t *testing.T) {
	// Every case here must be refused without ever asking the identity service:
	// these are properties of the token itself, and a rejected caller should not
	// cost a round trip.
	for _, tt := range []struct {
		name   string
		grant  auth.Grant
		status int
	}{
		{
			"platform token",
			auth.Grant{Subject: "admin", UserID: "admin", Realm: auth.RealmPlatform},
			http.StatusForbidden,
		},
		{
			"consumer token",
			auth.Grant{Subject: "user", UserID: "user", Tenant: "m-1", Realm: auth.RealmConsumer},
			http.StatusForbidden,
		},
		{
			// A merchant token without a tenant has no boundary to resolve, so
			// there is nothing to default it to.
			"merchant token with empty tenant",
			auth.Grant{Subject: "account-1", UserID: "account-1", Realm: auth.RealmMerchant},
			http.StatusForbidden,
		},
		{
			"merchant token with blank tenant",
			auth.Grant{Subject: "account-1", UserID: "account-1", Tenant: "  ", Realm: auth.RealmMerchant},
			http.StatusForbidden,
		},
		{
			// A token minted for somebody else describes no usable identity.
			"subject disagrees with user id",
			auth.Grant{Subject: "other", UserID: "account-1", Tenant: "m-1", Realm: auth.RealmMerchant},
			http.StatusUnauthorized,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resolved := false
			resolve := func(context.Context, string) (MerchantGrants, error) {
				resolved = true
				return MerchantGrants{MerchantID: "m-1", ScopeType: auth.ScopeTypeMerchant}, nil
			}
			status, _, _, called := runMerchantMiddleware(t, tt.grant, resolve, time.Second)
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if resolved {
				t.Fatal("resolver was called for a request that must be refused on the token alone")
			}
			if called {
				t.Fatal("handler ran for a rejected request")
			}
		})
	}
}

func TestMerchantMiddlewareFailsClosedWhenItCannotDecide(t *testing.T) {
	// The through-line of these cases: none of them may let the request through,
	// and none may substitute an empty or a full boundary for the real answer.
	for _, tt := range []struct {
		name    string
		resolve MerchantResolver
		timeout time.Duration
		status  int
	}{
		{
			"nil resolver",
			nil,
			time.Second,
			http.StatusServiceUnavailable,
		},
		{
			"non-positive timeout",
			okResolver(MerchantGrants{MerchantID: "m-1", ScopeType: auth.ScopeTypeMerchant}),
			0,
			http.StatusServiceUnavailable,
		},
		{
			"resolver says unauthenticated",
			func(context.Context, string) (MerchantGrants, error) { return MerchantGrants{}, ErrUnauthenticated },
			time.Second,
			http.StatusUnauthorized,
		},
		{
			"resolver says forbidden",
			func(context.Context, string) (MerchantGrants, error) { return MerchantGrants{}, ErrForbidden },
			time.Second,
			http.StatusForbidden,
		},
		{
			// The identity service being unreachable must not widen access.
			"resolver transport failure",
			func(context.Context, string) (MerchantGrants, error) {
				return MerchantGrants{}, errors.New("connection refused")
			},
			time.Second,
			http.StatusServiceUnavailable,
		},
		{
			"answer is about another merchant",
			okResolver(MerchantGrants{MerchantID: "m-2", ScopeType: auth.ScopeTypeMerchant}),
			time.Second,
			http.StatusServiceUnavailable,
		},
		{
			"answer has no merchant",
			okResolver(MerchantGrants{ScopeType: auth.ScopeTypeMerchant}),
			time.Second,
			http.StatusServiceUnavailable,
		},
		{
			// An unknown level cannot be read as "unlimited": the store_ids
			// beside it were expanded from this same value.
			"unknown scope type",
			okResolver(MerchantGrants{MerchantID: "m-1", ScopeType: "region"}),
			time.Second,
			http.StatusServiceUnavailable,
		},
		{
			"missing scope type",
			okResolver(MerchantGrants{MerchantID: "m-1"}),
			time.Second,
			http.StatusServiceUnavailable,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, _, seenOK, called := runMerchantMiddleware(t, merchantGrant("m-1"), tt.resolve, tt.timeout)
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if called {
				t.Fatal("handler ran even though the boundary could not be decided")
			}
			if seenOK {
				t.Fatal("a store scope reached the handler for an undecided request")
			}
		})
	}
}

func TestMerchantMiddlewarePublishesResolvedBoundary(t *testing.T) {
	for _, tt := range []struct {
		name   string
		grants MerchantGrants
		want   auth.StoreScope
	}{
		{
			"merchant level expands to every store of the tenant",
			MerchantGrants{
				MerchantID: "m-1",
				ScopeType:  auth.ScopeTypeMerchant,
				StoreIDs:   []string{"s-1", "s-2"},
			},
			auth.StoreScope{MerchantID: "m-1", ScopeType: auth.ScopeTypeMerchant, StoreIDs: []string{"s-1", "s-2"}},
		},
		{
			"brand level carries the brand ids",
			MerchantGrants{
				MerchantID: "m-1",
				ScopeType:  auth.ScopeTypeBrand,
				ScopeIDs:   []string{"b-1", "b-2"},
				StoreIDs:   []string{"s-3", "s-4"},
			},
			auth.StoreScope{MerchantID: "m-1", ScopeType: auth.ScopeTypeBrand, ScopeIDs: []string{"b-1", "b-2"}, StoreIDs: []string{"s-3", "s-4"}},
		},
		{
			"store level carries every point it names",
			MerchantGrants{
				MerchantID: "m-1",
				ScopeType:  auth.ScopeTypeStore,
				ScopeIDs:   []string{"s-9"},
				StoreIDs:   []string{"s-9"},
			},
			auth.StoreScope{MerchantID: "m-1", ScopeType: auth.ScopeTypeStore, ScopeIDs: []string{"s-9"}, StoreIDs: []string{"s-9"}},
		},
		{
			// The load-bearing case: an account authorized for nothing must
			// reach the repository as an empty slice, never as nil. nil encodes
			// to SQL NULL, and a predicate that treats NULL as "no filter" turns
			// this account into one that sees every row.
			"nothing authorized normalizes to an empty slice",
			MerchantGrants{MerchantID: "m-1", ScopeType: auth.ScopeTypeBrand, ScopeIDs: []string{"b-empty"}, StoreIDs: nil},
			auth.StoreScope{MerchantID: "m-1", ScopeType: auth.ScopeTypeBrand, ScopeIDs: []string{"b-empty"}, StoreIDs: []string{}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, seen, seenOK, called := runMerchantMiddleware(t, merchantGrant("m-1"), okResolver(tt.grants), time.Second)
			if status != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", status, http.StatusNoContent)
			}
			if !called {
				t.Fatal("handler did not run")
			}
			if !seenOK {
				t.Fatal("handler saw no store scope")
			}
			if seen.StoreIDs == nil {
				t.Fatal("StoreIDs is nil on the request; it must be an empty slice")
			}
			if !reflect.DeepEqual(seen, tt.want) {
				t.Fatalf("scope = %+v, want %+v", seen, tt.want)
			}
		})
	}
}

// A route that forgot to mount the middleware must not be readable: nothing on
// the request means "no boundary", and the only safe reading of that is refusal.
func TestStoreScopeAbsentWithoutMiddleware(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := auth.StoreScopeFromRequest(r); ok {
		t.Fatal("StoreScopeFromRequest reported a boundary on a bare request")
	}
}
