package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
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
// The middleware itself lives in platform/authz so every service enforces the
// same rules; this type is the gRPC half — the AdminAccessService call and the
// translation of its status codes into authz's sentinel errors.
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
	if a == nil {
		// A nil authorizer (a wiring bug, or a test passing nil) must fail
		// closed instead of panicking on the field access below.
		return authz.Middleware(nil, 0)(next)
	}
	return authz.Middleware(a.resolve, a.timeout)(next)
}

// resolve implements authz.Resolver.
func (a *AdminAuthorizer) resolve(ctx context.Context, accessToken string) (authz.Grants, error) {
	if a.users == nil {
		return authz.Grants{}, errors.New("authorizer has no admin access client")
	}
	// The token travels in metadata, never a request field: user-service
	// re-verifies it, so a caller cannot assert someone else's identity.
	resp, err := a.users.GetAdminAccess(auth.WithAccessToken(ctx, accessToken), &userv1.GetAdminAccessRequest{})
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
