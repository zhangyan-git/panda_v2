package authz

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
)

// MerchantGrants is the identity service's live answer for one merchant access
// token: which tenant the account belongs to, and which data boundary it has.
//
// StoreIDs is the part consumers filter on. It is the expansion of the scope,
// not the scope itself, because the business tables that need filtering — devices
// and orders — hold only a store_id. See platform/auth.StoreScope for why the
// expansion happens upstream instead of at each consumer.
type MerchantGrants struct {
	MerchantID string
	ScopeType  string
	// ScopeIDs is the brands or stores the scope points at — several of them,
	// because the level no longer limits an account to one target. Empty at
	// merchant level.
	ScopeIDs []string
	StoreIDs []string
}

// MerchantResolver fetches the current data boundary for a merchant access
// token. Like Resolver, implementations must report a rejected caller as
// ErrUnauthenticated or ErrForbidden; any other error means "could not decide"
// and fails closed.
type MerchantResolver func(ctx context.Context, accessToken string) (MerchantGrants, error)

// MerchantMiddleware resolves a merchant request's data boundary live, on every
// request, and puts it on the context for the handler and repository to filter
// by.
//
// It is the merchant-domain counterpart of Middleware, and the same reasoning
// applies with one extra consequence: a merchant account's scope decides which
// rows exist for it at all, not merely which buttons it sees. Signing the scope
// into the token would mean a scope narrowed in the console keeps granting the
// wider set until the token expires — 24 hours with the current TTL — which is
// exactly what §5.2.4 of the plan forbids.
//
// It must run after auth.Middleware (which supplies the verified identity) and
// before anything that reads the data boundary. A resolve failure never falls
// back to the token, and never degrades to an empty or a full boundary: the
// route returns 503 instead.
func MerchantMiddleware(resolve MerchantResolver, timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			unavailable := func() {
				api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "商户鉴权暂不可用")
			}
			identity, ok := auth.IdentityFromRequest(r)
			if !ok || identity.UserID == "" {
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
				return
			}
			// Subject and UserID disagreeing means the token was minted for
			// someone else; reject locally rather than ask about it. 401 rather
			// than 403: the token does not describe a usable identity at all.
			if identity.Subject != identity.UserID {
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
				return
			}
			// The realm check is not optional, and here it is the mirror image of
			// the one in Middleware. That one admits platform administrators; this
			// one must admit only merchants, or a platform administrator's token —
			// which also has Subject == UserID — would be evaluated against a
			// merchant account row it has no business having.
			if identity.Realm != auth.RealmMerchant {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "forbidden")
				return
			}
			// The tenant is the merchant this request is about, and it is required:
			// an account with no tenant has no boundary to resolve, and defaulting
			// it to anything would be inventing one.
			if strings.TrimSpace(identity.Tenant) == "" {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "forbidden")
				return
			}
			// A nil resolver or a non-positive timeout is a wiring bug. Treat it as
			// "cannot decide" rather than as either answer.
			if resolve == nil || timeout <= 0 {
				unavailable()
				return
			}
			token, ok := auth.BearerToken(r.Header.Get("Authorization"))
			if !ok {
				unavailable()
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			grants, err := resolve(ctx, token)
			if err != nil {
				switch {
				case errors.Is(err, ErrUnauthenticated):
					api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
				case errors.Is(err, ErrForbidden):
					api.Error(w, http.StatusForbidden, api.CodeForbidden, "forbidden")
				default:
					unavailable()
				}
				return
			}
			// The identity service must answer about the tenant this token claims.
			// A mismatch means one of the two is wrong and there is no way to tell
			// which, so neither is trusted and the request fails closed. Taking
			// either side here would let a token for merchant A be served merchant
			// B's boundary.
			if grants.MerchantID == "" || grants.MerchantID != identity.Tenant {
				unavailable()
				return
			}
			// An unrecognized scope type is refused rather than widened. The
			// expansion was computed upstream from this same value, so a value this
			// build does not know is one whose store_ids cannot be interpreted —
			// and the safe reading of "I do not know how narrow this is" is not
			// "assume no limit".
			switch grants.ScopeType {
			case auth.ScopeTypeMerchant, auth.ScopeTypeBrand, auth.ScopeTypeStore:
			default:
				unavailable()
				return
			}
			// Overwrite only what authorization owns. Roles and Permissions stay as
			// the token carried them — merchant accounts have neither, and the data
			// boundary is expressed by scope alone (§009 dropped the merchant role
			// tables).
			scope := auth.StoreScope{
				MerchantID: grants.MerchantID,
				ScopeType:  grants.ScopeType,
				ScopeIDs:   grants.ScopeIDs,
				StoreIDs:   grants.StoreIDs,
			}
			// WithStoreScope normalizes nil StoreIDs to an empty slice. That
			// normalization is load-bearing: nil reaches SQL as NULL, and a
			// predicate that reads NULL as "no filter" turns "authorized for no
			// store" into "sees everything".
			ctx = auth.WithStoreScope(r.Context(), scope)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
