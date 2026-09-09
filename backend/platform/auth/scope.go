package auth

import (
	"errors"
	"net/http"
)

// Scope types. An identity without a scope sees nothing: the console denies by
// default so a newly created operator cannot read the whole platform by
// accident. ScopeAll is the explicit opt-out.
const (
	ScopeAll    = "all"
	ScopeCustom = "custom"
	ScopeSelf   = "self"
)

// MaxScopeIDs caps how many identifiers a single scope may carry. The scope
// travels inside the access token, so an unbounded list would push the token
// past what proxies accept. An operator who needs more than this should be
// scoped by merchant instead of by enumerating stores.
const MaxScopeIDs = 512

// ErrScopeTooLarge is returned when a grant's scope exceeds MaxScopeIDs.
var ErrScopeTooLarge = errors.New("data scope carries too many identifiers")

// Scope is the data boundary carried by a token. The three id sets are ORed:
// an operator scoped to merchant M and to store S sees both, even when S
// belongs to another merchant.
type Scope struct {
	Type        string   `json:"type,omitempty"`
	MerchantIDs []string `json:"merchant_ids,omitempty"`
	StoreIDs    []string `json:"store_ids,omitempty"`
	// Regions holds province and city names as stored on the store record.
	Regions []string `json:"regions,omitempty"`
}

// Unrestricted reports whether the scope waives every data boundary.
func (s Scope) Unrestricted() bool { return s.Type == ScopeAll }

// Empty reports whether the scope admits nothing. A scope with no type at all
// counts as empty, so a token minted before data scopes existed grants no data
// rather than silently granting everything.
func (s Scope) Empty() bool {
	if s.Unrestricted() {
		return false
	}
	return len(s.MerchantIDs) == 0 && len(s.StoreIDs) == 0 && len(s.Regions) == 0
}

func (s Scope) size() int {
	return len(s.MerchantIDs) + len(s.StoreIDs) + len(s.Regions)
}

func (s Scope) clone() Scope {
	return Scope{
		Type:        s.Type,
		MerchantIDs: append([]string(nil), s.MerchantIDs...),
		StoreIDs:    append([]string(nil), s.StoreIDs...),
		Regions:     append([]string(nil), s.Regions...),
	}
}

// AllowsStore reports whether the scope permits access to a store. Devices,
// orders and coupons all resolve to a store, so this single check covers them.
func (s Scope) AllowsStore(storeID, merchantID, province, city string) bool {
	if s.Unrestricted() {
		return true
	}
	if contains(s.StoreIDs, storeID) || contains(s.MerchantIDs, merchantID) {
		return true
	}
	return contains(s.Regions, province) || contains(s.Regions, city)
}

// AllowsMerchant reports whether the scope permits access to a merchant record.
// A store-level scope grants no merchant listing on its own.
func (s Scope) AllowsMerchant(merchantID string) bool {
	if s.Unrestricted() {
		return true
	}
	return contains(s.MerchantIDs, merchantID)
}

// AllowsAll reports whether the identity reads without a data boundary.
func (i Identity) AllowsAll() bool { return i.IsSuper || i.Scope.Unrestricted() }

// AllowsStore reports whether the identity may see data attached to a store.
// Devices, orders and coupons all resolve to a store, so this single check
// covers them; pass the store's own ids and the province/city it sits in.
func (i Identity) AllowsStore(storeID, merchantID, province, city string) bool {
	if i.AllowsAll() {
		return true
	}
	return i.Scope.AllowsStore(storeID, merchantID, province, city)
}

// AllowsMerchant reports whether the identity may see a merchant record. A
// store-level scope grants no merchant listing on its own — the operator sees
// the stores, not the merchant they hang off.
func (i Identity) AllowsMerchant(merchantID string) bool {
	if i.AllowsAll() {
		return true
	}
	return i.Scope.AllowsMerchant(merchantID)
}

// ScopeFromRequest returns the data boundary for a request. The second result
// is false when the caller reads without a boundary, which tells a repository
// to skip the scope predicate entirely rather than filter on an empty set.
func ScopeFromRequest(r *http.Request) (Scope, bool) {
	identity, ok := IdentityFromRequest(r)
	if !ok {
		// An unauthenticated request reaching a scoped query is a routing bug.
		// Return a restricted empty scope so the query yields nothing.
		return Scope{Type: ScopeCustom}, true
	}
	if identity.AllowsAll() {
		return Scope{}, false
	}
	return identity.Scope, true
}

func contains(values []string, want string) bool {
	if want == "" {
		return false
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
