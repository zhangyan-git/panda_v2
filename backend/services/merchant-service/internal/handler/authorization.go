package handler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// adminAccessClient is the single RPC the authorizer needs. user-service
// re-verifies the forwarded access token and answers for that token's identity
// only.
type adminAccessClient interface {
	GetAdminAccess(ctx context.Context, in *userv1.GetAdminAccessRequest, opts ...grpc.CallOption) (*userv1.GetAdminAccessResponse, error)
}

// AdminAuthorizer resolves current admin access over the internal gRPC API.
// It never caches grants or falls back to the token's authorization snapshot.
type AdminAuthorizer struct {
	users   adminAccessClient
	timeout time.Duration
}

func NewAdminAuthorizer(users adminAccessClient, timeout time.Duration) (*AdminAuthorizer, error) {
	if users == nil || timeout <= 0 {
		return nil, errors.New("authorization client and a positive timeout are required")
	}
	return &AdminAuthorizer{users: users, timeout: timeout}, nil
}

func (a *AdminAuthorizer) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unavailable := func() {
			api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, "权限服务暂不可用")
		}
		identity, ok := auth.IdentityFromRequest(r)
		if !ok || identity.UserID == "" || identity.Subject != identity.UserID {
			api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
			return
		}
		// auth.Middleware has already verified this exact Bearer access token.
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || a == nil || a.users == nil {
			unavailable()
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), a.timeout)
		defer cancel()
		// The token travels in metadata, never a request field: user-service
		// re-verifies it, so a caller cannot assert someone else's identity.
		resp, err := a.users.GetAdminAccess(auth.WithAccessToken(ctx, token), &userv1.GetAdminAccessRequest{})
		if err != nil {
			switch status.Code(err) {
			case codes.Unauthenticated:
				api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, "unauthorized")
			case codes.PermissionDenied:
				api.Error(w, http.StatusForbidden, api.CodeForbidden, "forbidden")
			default:
				unavailable()
			}
			return
		}
		if resp.GetUserId() != identity.UserID {
			unavailable()
			return
		}
		identity.Roles = resp.GetRoles()
		identity.Permissions = resp.GetPermissions()
		// super-admin is the "super_admin" role throughout this codebase.
		identity.IsSuper = slices.Contains(identity.Roles, "super_admin")
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
	})
}

// bearerToken reads the already-verified credential off the request header. It
// mirrors the platform parser so the forwarded value is the raw token.
func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}
