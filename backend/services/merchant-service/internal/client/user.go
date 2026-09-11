// Package client holds merchant-service's outbound gRPC clients. Cross-service
// calls live here rather than in the repositories so that a repository package
// stays a local-storage adapter.
package client

import (
	"context"
	"fmt"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UserServiceClient reaches the internal user-service RPCs with the shared
// service token. merchant_users and the account scope live in the identity
// database, so every answer here is a remote call that must not be cached.
type UserServiceClient struct {
	users        userv1.UserServiceClient
	serviceToken string
}

func NewUserServiceClient(conn grpc.ClientConnInterface, serviceToken string) *UserServiceClient {
	return &UserServiceClient{users: userv1.NewUserServiceClient(conn), serviceToken: serviceToken}
}

// ResetScopeByTarget reclaims the accounts pointing at a brand or store that is
// being deleted. It satisfies repository.MerchantUserRepository.
//
// Fail-closed: an error of any kind aborts the caller's delete. There is no
// retry and no local fallback, because a silently skipped scope reset would
// strand accounts on a target that no longer exists.
//
// A target that no longer has any referencing account is success, not a missing
// resource: the invariant the reset protects (no account points at the deleted
// target) already holds, so the delete is allowed to proceed.
func (c *UserServiceClient) ResetScopeByTarget(ctx context.Context, scopeType, scopeID string) error {
	if c == nil || c.users == nil {
		return repository.ErrUnavailable
	}
	_, err := c.users.ResetAccountScope(
		auth.WithServiceToken(ctx, c.serviceToken),
		&userv1.ResetAccountScopeRequest{ScopeType: scopeType, ScopeId: scopeID},
	)
	if err != nil {
		return scopeResetError(err)
	}
	return nil
}

// HasUsers reports whether the merchant still owns accounts. It replaces the
// direct merchant_users read that the database split makes impossible, and
// satisfies service.MerchantAccountPresence.
func (c *UserServiceClient) HasUsers(ctx context.Context, merchantID string) (bool, error) {
	if c == nil || c.users == nil {
		return false, repository.ErrUnavailable
	}
	resp, err := c.users.HasUsers(
		auth.WithServiceToken(ctx, c.serviceToken),
		&userv1.HasUsersRequest{MerchantId: merchantID},
	)
	if err != nil {
		return false, err
	}
	return resp.GetHasUsers(), nil
}

// scopeResetError distinguishes "we never reached user-service" from "user-service
// refused the reset". Only the former keeps the historical 503 signal; a refusal
// is a real failure and must surface as such.
func scopeResetError(err error) error {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %v", repository.ErrUnavailable, err)
	default:
		return err
	}
}
