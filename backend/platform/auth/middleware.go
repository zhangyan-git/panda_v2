package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/api"
)

type identityContextKey struct{}

// WithIdentity returns a context carrying an authenticated identity.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the authenticated identity stored in ctx.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	identity, ok := ctx.Value(identityContextKey{}).(Identity)
	return identity, ok
}

// IdentityFromRequest returns the authenticated identity stored on r.
func IdentityFromRequest(r *http.Request) (Identity, bool) {
	if r == nil {
		return Identity{}, false
	}
	return IdentityFromContext(r.Context())
}

// Middleware authenticates an HTTP Bearer access token and adds its identity
// to the request context. If tokenTypes is provided, the token must have one
// of the specified types; otherwise an access token is required.
func Middleware(service *Service, tokenTypes ...TokenType) func(http.Handler) http.Handler {
	requiredTypes := tokenTypes
	if len(requiredTypes) == 0 {
		requiredTypes = []TokenType{AccessTokenType}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := BearerToken(r.Header.Get("Authorization"))
			if !ok || service == nil {
				writeUnauthorized(w)
				return
			}

			claims, err := service.Parse(token)
			if err != nil || !hasTokenType(claims.TokenType, requiredTypes) {
				writeUnauthorized(w)
				return
			}

			identity := Identity{
				Subject:     claims.Subject,
				UserID:      claims.UserID,
				Tenant:      claims.Tenant,
				Realm:       claims.Realm,
				Roles:       append([]string(nil), claims.Roles...),
				Permissions: append([]string(nil), claims.Permissions...),
				IsSuper:     claims.IsSuper,
			}
			if claims.Scope != nil {
				identity.Scope = claims.Scope.clone()
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
		})
	}
}

// RequireRoles allows only authenticated identities carrying one of the roles.
func RequireRoles(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := IdentityFromRequest(r)
			if !ok || !hasRole(identity.Roles, roles) {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePermission rejects requests whose identity holds none of the given
// permission codes. A super administrator bypasses the check entirely.
func RequirePermission(codes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, ok := IdentityFromRequest(r)
			if !ok || !hasPermission(identity, codes) {
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "没有权限执行此操作")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasPermission(identity Identity, required []string) bool {
	for _, code := range required {
		if identity.HasPermission(code) {
			return true
		}
	}
	return false
}

func hasRole(current, required []string) bool {
	for _, role := range current {
		for _, wanted := range required {
			if role == wanted {
				return true
			}
		}
	}
	return false
}

// Bearer is an alias for Middleware for callers that prefer the auth scheme name.
func Bearer(service *Service, tokenTypes ...TokenType) func(http.Handler) http.Handler {
	return Middleware(service, tokenTypes...)
}

// BearerMiddleware authenticates requests using a Bearer token.
func BearerMiddleware(service *Service, tokenTypes ...TokenType) func(http.Handler) http.Handler {
	return Middleware(service, tokenTypes...)
}

// BearerToken extracts the raw token from an Authorization header value,
// accepting any casing of the scheme. It is exported for services that forward
// the caller's own token onward — the platform's parser living in one place is
// what keeps the forwarded value byte-identical to the one already verified.
func BearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func hasTokenType(tokenType TokenType, required []TokenType) bool {
	for _, expected := range required {
		if tokenType == expected {
			return true
		}
	}
	return false
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")

}
