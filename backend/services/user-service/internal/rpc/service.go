// Package rpc implements user-service's internal gRPC surface. Every RPC here
// is an internal call: either service-to-service (shared service token) or a
// call made on behalf of an end user (the caller's own access token). Identity
// always comes from metadata, never from a request field.
//
// The package is named rpc rather than grpc so it does not shadow the gRPC
// transport package in import lists.
package rpc

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/service"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UserServiceServer answers the internal RPCs merchant-service performs against
// the identity database. GetProfile/UpdateProfile stay unimplemented: no caller
// exists yet, and the embedded Unimplemented server reports that honestly.
type UserServiceServer struct {
	userv1.UnimplementedUserServiceServer
	users repository.MerchantUserRepository
}

func NewUserServiceServer(users repository.MerchantUserRepository) *UserServiceServer {
	return &UserServiceServer{users: users}
}

// HasUsers tells merchant-service whether a merchant still owns accounts, which
// it must know before deleting the merchant.
func (s *UserServiceServer) HasUsers(ctx context.Context, req *userv1.HasUsersRequest) (*userv1.HasUsersResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	hasUsers, err := s.users.HasUsers(ctx, req.GetMerchantId())
	if err != nil {
		return nil, status.Error(codes.Internal, "query merchant users")
	}
	return &userv1.HasUsersResponse{HasUsers: hasUsers}, nil
}

// ResetAccountScope reclaims accounts pointing at a brand or store that is being
// deleted. It replaces the HTTP ownership transport whose reset failed closed
// and made brand and store deletion impossible.
func (s *UserServiceServer) ResetAccountScope(ctx context.Context, req *userv1.ResetAccountScopeRequest) (*userv1.ResetAccountScopeResponse, error) {
	if err := auth.RequireService(ctx); err != nil {
		return nil, err
	}
	// An unrecognised scope_type must fail rather than fall back to a default:
	// guessing wrong would rewrite the scope of unrelated accounts.
	if req.GetScopeType() != "brand" && req.GetScopeType() != "store" {
		return nil, status.Error(codes.InvalidArgument, `scope_type must be "brand" or "store"`)
	}
	if req.GetScopeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "scope_id is required")
	}
	if err := s.users.ResetScopeByTarget(ctx, req.GetScopeType(), req.GetScopeId()); err != nil {
		return nil, status.Error(codes.Internal, "reset account scope")
	}
	return &userv1.ResetAccountScopeResponse{}, nil
}

// AdminAccessServiceServer answers live authorization queries about the caller.
// It is authenticated by an end-user access token; a service token must never
// reach it, because a service has no grants to report.
type AdminAccessServiceServer struct {
	userv1.UnimplementedAdminAccessServiceServer
	authSvc *service.AdminAuthService
}

func NewAdminAccessServiceServer(authSvc *service.AdminAuthService) *AdminAccessServiceServer {
	return &AdminAccessServiceServer{authSvc: authSvc}
}

// GetAdminAccess reproduces GET /v1/admin/users/me: platform identity checks,
// live account status, then live role and permission codes from the database.
// The response carries no is_super field on purpose. Super-admin is derived by
// the caller from roles containing "super_admin" — the same rule
// service.AdminAuthService.Login uses for IsSuper — so do not "fix" that here.
func (s *AdminAccessServiceServer) GetAdminAccess(ctx context.Context, _ *userv1.GetAdminAccessRequest) (*userv1.GetAdminAccessResponse, error) {
	identity, err := auth.RequireUser(ctx)
	if err != nil {
		return nil, err
	}
	// Same guards as the HTTP handler: the token must describe one platform
	// account (subject == user id) and must not be a merchant tenant token.
	if identity.Subject != identity.UserID {
		return nil, status.Error(codes.Unauthenticated, "access token subject mismatch")
	}
	if identity.Tenant != "" {
		return nil, status.Error(codes.PermissionDenied, "platform administrator required")
	}
	user, err := s.authSvc.Profile(ctx, identity.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.Unauthenticated, "account not found")
	}
	if err != nil || user == nil || user.ID != identity.UserID {
		return nil, status.Error(codes.Unavailable, "account service unavailable")
	}
	if user.Status != "active" {
		return nil, status.Error(codes.PermissionDenied, "account disabled")
	}
	// Grants are read live, never from the token: role bindings change without
	// reissuing tokens, and a stale snapshot must not keep granting access.
	roles, perms, err := s.authSvc.LiveAccess(ctx, identity.UserID)
	if err != nil {
		return nil, status.Error(codes.Internal, "load live access")
	}
	return &userv1.GetAdminAccessResponse{
		UserId:      identity.UserID,
		Roles:       roles,
		Permissions: perms,
	}, nil
}
