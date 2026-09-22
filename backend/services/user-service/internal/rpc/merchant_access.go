package rpc

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MerchantAccessServiceServer answers the live data boundary of a merchant
// account. It is the merchant counterpart of AdminAccessServiceServer: identity
// comes only from metadata, and grants are read fresh on every call.
//
// It is authenticated by an end-user access token; a service token must never
// reach it, because a service has no merchant account and therefore no boundary.
type MerchantAccessServiceServer struct {
	userv1.UnimplementedMerchantAccessServiceServer
	access *service.MerchantAccessService
}

func NewMerchantAccessServiceServer(access *service.MerchantAccessService) *MerchantAccessServiceServer {
	return &MerchantAccessServiceServer{access: access}
}

// GetMerchantAccess answers the boundary the merchant-domain services filter on.
//
// Every failure below is fail-closed, and the two directions of failure are kept
// apart on purpose: a rejected caller is 401/403, while a boundary that could
// not be worked out is 503. Collapsing them into one code would make "you may
// see nothing" and "we could not tell what you may see" indistinguishable — and
// the second one must never be answered with an empty set, because the caller
// would then serve an empty console instead of reporting a broken dependency.
func (s *MerchantAccessServiceServer) GetMerchantAccess(ctx context.Context, _ *userv1.GetMerchantAccessRequest) (*userv1.GetMerchantAccessResponse, error) {
	identity, err := auth.RequireUser(ctx)
	if err != nil {
		return nil, err
	}
	// Same guards as GetAdminAccess. A C-end token satisfies the first two
	// conditions as well, so the realm check is what keeps a miniapp customer out.
	if identity.Subject != identity.UserID {
		return nil, status.Error(codes.Unauthenticated, "access token subject mismatch")
	}
	if identity.Realm != auth.RealmMerchant {
		return nil, status.Error(codes.PermissionDenied, "merchant account required")
	}
	tenant := strings.TrimSpace(identity.Tenant)
	if tenant == "" {
		// A merchant token without a tenant names no merchant, so there is no
		// account to look up and nothing to compare the answer against.
		return nil, status.Error(codes.PermissionDenied, "merchant account required")
	}
	access, err := s.access.Resolve(ctx, identity.UserID, tenant)
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// The account was deleted after the token was issued.
			return nil, status.Error(codes.Unauthenticated, "account not found")
		case errors.Is(err, service.ErrMerchantAccountMismatch),
			errors.Is(err, service.ErrMerchantUserDisabled),
			errors.Is(err, service.ErrMerchantPending),
			errors.Is(err, service.ErrMerchantSuspended):
			return nil, status.Error(codes.PermissionDenied, "merchant account not permitted")
		case errors.Is(err, service.ErrScopeTypeInvalid):
			// The stored scope is a value this build cannot interpret. Reporting
			// it as a dependency failure is deliberate: the alternative readings
			// are "merchant-wide" (too wide) and "no stores" (silently empty),
			// and neither is what the row says.
			return nil, status.Error(codes.Unavailable, "merchant scope cannot be interpreted")
		default:
			// Downstream unreachable, or the expansion failed. Never degraded to
			// an empty boundary and never widened to a full one.
			return nil, status.Error(codes.Unavailable, "merchant scope unavailable")
		}
	}
	return &userv1.GetMerchantAccessResponse{
		MerchantId: access.MerchantID,
		ScopeType:  access.ScopeType,
		ScopeId:    access.ScopeID,
		StoreIds:   access.StoreIDs,
	}, nil
}
