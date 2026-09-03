package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
			token, ok := bearerToken(r.Header.Get("Authorization"))
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
				Subject: claims.Subject,
				UserID:  claims.UserID,
				Tenant:  claims.Tenant,
				Roles:   append([]string(nil), claims.Roles...),
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
		})
	}
}

// Bearer is an alias for Middleware for callers that prefer the auth scheme name.
func Bearer(service *Service, tokenTypes ...TokenType) func(http.Handler) http.Handler {
	return Middleware(service, tokenTypes...)
}

// BearerMiddleware authenticates requests using a Bearer token.
func BearerMiddleware(service *Service, tokenTypes ...TokenType) func(http.Handler) http.Handler {
	return Middleware(service, tokenTypes...)
}

func bearerToken(header string) (string, bool) {
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
