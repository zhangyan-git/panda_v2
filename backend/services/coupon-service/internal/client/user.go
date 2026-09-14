// Package client holds coupon-service's outbound gRPC clients. Cross-service
// calls live here rather than in the controllers so that a controller stays an
// HTTP adapter over the coupon service layer.
package client

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AdminAccessResolver answers "what may this admin do" by asking user-service,
// on every request. Coupon authorization used to read the token's claims, which
// are signed at login and go stale until it expires (24 hours with the current
// TTL) — a revoked permission or a demoted administrator kept working for the
// rest of the day.
//
// The admin access RPC authenticates the caller's own access token, so this
// client needs no service token: presenting one is for the RPCs that act on
// infrastructure rather than on a user.
type AdminAccessResolver struct {
	users userv1.AdminAccessServiceClient
}

func NewAdminAccessResolver(conn grpc.ClientConnInterface) *AdminAccessResolver {
	return &AdminAccessResolver{users: userv1.NewAdminAccessServiceClient(conn)}
}

// Resolve implements authz.Resolver. It reports a rejected caller as the
// sentinel errors the middleware maps to 401/403; every other failure stays an
// ordinary error and becomes a fail-closed 503.
func (r *AdminAccessResolver) Resolve(ctx context.Context, accessToken string) (authz.Grants, error) {
	if r == nil || r.users == nil {
		return authz.Grants{}, errors.New("admin access client is not configured")
	}
	// The token travels in metadata, never a request field: user-service
	// re-verifies it, so a caller cannot assert someone else's identity.
	resp, err := r.users.GetAdminAccess(auth.WithAccessToken(ctx, accessToken), &userv1.GetAdminAccessRequest{})
	if err != nil {
		switch status.Code(err) {
		case codes.Unauthenticated:
			return authz.Grants{}, authz.ErrUnauthenticated
		case codes.PermissionDenied:
			return authz.Grants{}, authz.ErrForbidden
		default:
			return authz.Grants{}, err
		}
	}
	return authz.Grants{
		UserID:      resp.GetUserId(),
		Roles:       resp.GetRoles(),
		Permissions: resp.GetPermissions(),
	}, nil
}
