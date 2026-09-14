// Package authz resolves a request's authorization from the identity service on
// every request, instead of trusting the permission snapshot inside the access
// token.
//
// The split it exists to enforce: a token answers "who is this" (and stays the
// source of truth for that), while "what may they do" is answered fresh by the
// identity service. A token's claims were signed at login and no longer change,
// so authorizing from them means a revoked permission or a demoted
// administrator keeps working until the token expires — 24 hours with the
// current TTL.
//
// The package deliberately knows nothing about gRPC: services adapt their own
// client into a Resolver and translate transport errors into the sentinel
// errors below. That keeps the middleware testable without a transport and
// keeps this package out of every service's dependency graph.
package authz

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
)

// SuperRole is the role code that grants every permission. It is the single
// spelling used across the platform: no service invents a synonym.
const SuperRole = "super_admin"

// Grants is the identity service's live answer for one access token.
type Grants struct {
	UserID      string
	Roles       []string
	Permissions []string
}

// Resolver fetches the current grants for an access token. Implementations must
// report a rejected caller as ErrUnauthenticated or ErrForbidden so the
// middleware can map it to a status code; any other error is treated as
// "authorization could not be decided" and fails closed.
type Resolver func(ctx context.Context, accessToken string) (Grants, error)

var (
	// ErrUnauthenticated means the token is no longer accepted (expired,
	// revoked, or belongs to a disabled account).
	ErrUnauthenticated = errors.New("authz: unauthenticated")
	// ErrForbidden means the token is valid but the account may not use it.
	ErrForbidden = errors.New("authz: forbidden")
)

// Middleware replaces the identity's authorization with live grants on every
// request. It must run after auth.Middleware (which supplies the verified
// identity) and before anything that reads permissions.
//
// A resolve failure never falls back to the token's claims: the whole point is
// that those are stale. Unreachable identity service therefore means 503 for
// the protected route, which is the safe direction — an outage must not widen
// access.
func Middleware(resolve Resolver, timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			unavailable := func() {
				api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "权限服务暂不可用")
			}
			identity, ok := auth.IdentityFromRequest(r)
			if !ok || identity.UserID == "" {
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
				return
			}
			// Subject and UserID disagreeing means the token was minted for
			// someone else; reject locally rather than ask about it. 401 rather
			// than 403: the token does not describe a usable identity at all, so
			// there is nobody to be forbidden.
			if identity.Subject != identity.UserID {
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
				return
			}
			// The realm check is not optional: this middleware guards
			// administrator routes, and a C-end token has Subject == UserID and no
			// tenant exactly as an administrator's does, so the structural
			// conditions alone would admit a signed-in miniapp customer.
			if !identity.IsPlatformAdmin() {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "forbidden")
				return
			}
			// A nil resolver or a non-positive timeout is a wiring bug. Treat it
			// as "cannot decide" rather than as either answer.
			if resolve == nil || timeout <= 0 {
				unavailable()
				return
			}
			// auth.Middleware has already verified this exact Bearer token; it
			// travels in metadata so the identity service re-verifies it and a
			// caller cannot assert someone else's identity.
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
			if grants.UserID != identity.UserID {
				// The identity service answered about somebody else. Do not use
				// that answer, and do not fall back to the token.
				unavailable()
				return
			}
			// Overwrite only the authorization fields. Subject, Tenant,
			// AccountID and Scope come from the token and stay untouched: they
			// describe who the caller is, not what they may do, and callers such
			// as the coupon controllers read Subject to attribute an operation.
			identity.Roles = grants.Roles
			identity.Permissions = grants.Permissions
			// Recomputed from live roles, so demoting an administrator takes
			// effect on their next request with no re-login.
			identity.IsSuper = slices.Contains(grants.Roles, SuperRole)
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
		})
	}
}
