package auth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func scopeService(t *testing.T) *Service {
	t.Helper()
	service, err := NewService([]byte("test-secret-value-32-bytes-long!!"), "panda-test", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// An operator scoped to a merchant and to one unrelated store sees both. The
// three id sets are ORed, so adding a store does not narrow the merchant grant.
func TestAllowsStoreUnionsEveryDimension(t *testing.T) {
	identity := Identity{Scope: Scope{
		Type:        ScopeCustom,
		MerchantIDs: []string{"merchant-1"},
		StoreIDs:    []string{"store-9"},
		Regions:     []string{"浙江省"},
	}}
	cases := []struct {
		name                                string
		storeID, merchantID, province, city string
		want                                bool
	}{
		{"store of the granted merchant", "store-1", "merchant-1", "广东省", "深圳市", true},
		{"explicitly granted store", "store-9", "merchant-8", "广东省", "深圳市", true},
		{"store in the granted province", "store-3", "merchant-8", "浙江省", "杭州市", true},
		{"unrelated store", "store-3", "merchant-8", "广东省", "深圳市", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := identity.AllowsStore(testCase.storeID, testCase.merchantID, testCase.province, testCase.city)
			if got != testCase.want {
				t.Fatalf("AllowsStore = %v, want %v", got, testCase.want)
			}
		})
	}
}

// A scope may name a city rather than a province, so both levels are matched
// against the same list.
func TestAllowsStoreMatchesCityScope(t *testing.T) {
	identity := Identity{Scope: Scope{Type: ScopeCustom, Regions: []string{"杭州市"}}}
	if !identity.AllowsStore("store-1", "merchant-1", "浙江省", "杭州市") {
		t.Fatal("city scope did not match a store in that city")
	}
	if identity.AllowsStore("store-2", "merchant-1", "浙江省", "宁波市") {
		t.Fatal("city scope matched a store in a different city")
	}
}

// Blank identifiers must never match. A store with no brand or an operator
// with an empty entry in the list would otherwise gain everything unset.
func TestBlankIdentifiersNeverMatch(t *testing.T) {
	identity := Identity{Scope: Scope{Type: ScopeCustom, StoreIDs: []string{""}, Regions: []string{""}}}
	if identity.AllowsStore("", "", "", "") {
		t.Fatal("blank values matched a blank scope entry")
	}
}

// The console denies by default: an operator whose scope was never configured
// reads nothing, rather than reading the whole platform.
func TestUnconfiguredScopeAllowsNothing(t *testing.T) {
	identity := Identity{Permissions: []string{"device:manage"}}
	if identity.AllowsAll() {
		t.Fatal("an identity with no scope must not read without a boundary")
	}
	if identity.AllowsStore("store-1", "merchant-1", "浙江省", "杭州市") {
		t.Fatal("an identity with no scope must not reach a store")
	}
	if !identity.Scope.Empty() {
		t.Fatal("a scope with no type must count as empty")
	}
}

func TestSuperAdminAndScopeAllBypassTheBoundary(t *testing.T) {
	for name, identity := range map[string]Identity{
		"super admin": {IsSuper: true},
		"scope all":   {Scope: Scope{Type: ScopeAll}},
	} {
		t.Run(name, func(t *testing.T) {
			if !identity.AllowsAll() {
				t.Fatal("AllowsAll = false")
			}
			if !identity.AllowsStore("store-1", "merchant-1", "浙江省", "杭州市") {
				t.Fatal("AllowsStore = false")
			}
			if !identity.AllowsMerchant("merchant-1") {
				t.Fatal("AllowsMerchant = false")
			}
		})
	}
}

// A store-level grant does not imply the merchant listing: the operator sees
// the stores they were given, not the merchant record they hang off.
func TestStoreScopeDoesNotGrantTheMerchant(t *testing.T) {
	identity := Identity{Scope: Scope{Type: ScopeCustom, StoreIDs: []string{"store-1"}}}
	if identity.AllowsMerchant("merchant-1") {
		t.Fatal("a store scope granted a merchant record")
	}
}

// An empty boundary must reach SQL as '{}' rather than NULL: a predicate that reads
// NULL as "no filter" would turn "authorized for no store" into "sees every store".
func TestAuthorizedStoreIDsNeverReturnsNil(t *testing.T) {
	if got := (StoreScope{}).AuthorizedStoreIDs(); got == nil || len(got) != 0 {
		t.Fatalf("empty scope = %#v; want a non-nil empty slice", got)
	}
	granted := StoreScope{StoreIDs: []string{"store-1"}}
	if got := granted.AuthorizedStoreIDs(); len(got) != 1 || got[0] != "store-1" {
		t.Fatalf("granted scope = %#v; want the authorized stores", got)
	}
}

func TestScopeSurvivesTheTokenRoundTrip(t *testing.T) {
	service := scopeService(t)
	scope := Scope{
		Type:        ScopeCustom,
		MerchantIDs: []string{"merchant-1"},
		StoreIDs:    []string{"store-1", "store-2"},
		Regions:     []string{"浙江省"},
	}
	token, err := service.SignAccessGrant(Grant{Realm: RealmPlatform, Subject: "account-1", Scope: scope})
	if err != nil {
		t.Fatal(err)
	}

	var got Identity
	handler := Middleware(service)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = IdentityFromRequest(r)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/admin/devices", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if got.Scope.Type != ScopeCustom {
		t.Fatalf("scope type = %q, want %q", got.Scope.Type, ScopeCustom)
	}
	if len(got.Scope.StoreIDs) != 2 || got.Scope.StoreIDs[0] != "store-1" {
		t.Fatalf("store ids = %v", got.Scope.StoreIDs)
	}
	if !got.AllowsStore("store-2", "merchant-9", "广东省", "深圳市") {
		t.Fatal("a store carried by the token was rejected after parsing")
	}
	if got.AllowsStore("store-3", "merchant-9", "广东省", "深圳市") {
		t.Fatal("a store outside the token's scope was allowed")
	}
}

// A token minted before data scopes existed carries no scope claim. It must
// grant no data rather than defaulting to unrestricted.
func TestTokenWithoutScopeClaimGrantsNoData(t *testing.T) {
	service := scopeService(t)
	token, err := service.SignAccessGrant(Grant{Realm: RealmPlatform, Subject: "account-1", Permissions: []string{"device:manage"}})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Scope != nil {
		t.Fatalf("scope claim = %+v, want absent", claims.Scope)
	}
	identity := Identity{Permissions: claims.Permissions}
	if identity.AllowsAll() {
		t.Fatal("a token without a scope claim read without a boundary")
	}
}

// The scope rides inside the token, so an unbounded list would push the token
// past what proxies accept. Signing rejects it instead of minting it.
func TestSigningRejectsAnOversizedScope(t *testing.T) {
	service := scopeService(t)
	ids := make([]string, MaxScopeIDs+1)
	for i := range ids {
		ids[i] = "store-" + strconv.Itoa(i)
	}
	if _, err := service.SignAccessGrant(Grant{
		Realm:   RealmPlatform,
		Subject: "account-1",
		Scope:   Scope{Type: ScopeCustom, StoreIDs: ids},
	}); err != ErrScopeTooLarge {
		t.Fatalf("err = %v, want ErrScopeTooLarge", err)
	}
}

func TestScopeFromRequest(t *testing.T) {
	t.Run("unrestricted caller skips the predicate", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/v1/admin/devices", nil)
		request = request.WithContext(WithIdentity(request.Context(), Identity{IsSuper: true}))
		if _, restricted := ScopeFromRequest(request); restricted {
			t.Fatal("a super admin was reported as restricted")
		}
	})
	t.Run("scoped caller carries its boundary", func(t *testing.T) {
		want := Scope{Type: ScopeCustom, StoreIDs: []string{"store-1"}}
		request := httptest.NewRequest(http.MethodGet, "/v1/admin/devices", nil)
		request = request.WithContext(WithIdentity(request.Context(), Identity{Scope: want}))
		scope, restricted := ScopeFromRequest(request)
		if !restricted || len(scope.StoreIDs) != 1 {
			t.Fatalf("scope = %+v restricted = %v", scope, restricted)
		}
	})
	// A scoped query reached without authentication is a routing bug; it must
	// fail closed rather than return an unrestricted scope.
	t.Run("anonymous caller is restricted to nothing", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/v1/admin/devices", nil)
		scope, restricted := ScopeFromRequest(request)
		if !restricted || !scope.Empty() {
			t.Fatalf("scope = %+v restricted = %v", scope, restricted)
		}
	})
}

// The identity holds a copy: mutating the caller's slice must not widen an
// already-parsed identity.
func TestIdentityScopeIsACopy(t *testing.T) {
	service := scopeService(t)
	ids := []string{"store-1"}
	token, err := service.SignAccessGrant(Grant{Realm: RealmPlatform, Subject: "a", Scope: Scope{Type: ScopeCustom, StoreIDs: ids}})
	if err != nil {
		t.Fatal(err)
	}
	ids[0] = "store-2"
	claims, err := service.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Scope.StoreIDs[0] != "store-1" {
		t.Fatalf("store id = %q, want store-1", claims.Scope.StoreIDs[0])
	}
}
