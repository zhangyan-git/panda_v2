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

// merchantAccessClient is the single RPC the merchant authorizer needs.
// user-service re-verifies the forwarded access token and answers for that
// token's identity only; nothing the caller sends can name someone else.
type merchantAccessClient interface {
	GetMerchantAccess(ctx context.Context, in *userv1.GetMerchantAccessRequest, opts ...grpc.CallOption) (*userv1.GetMerchantAccessResponse, error)
}

// MerchantAuthorizer resolves a merchant account's live data boundary over the
// internal gRPC API. The middleware itself lives in platform/authz so every
// merchant-domain service enforces the same rules; this type is the gRPC half —
// the MerchantAccessService call and the translation of its status codes into
// authz's sentinel errors.
//
// It is the counterpart of AdminAuthorizer, and it is deliberately not merged
// with it: an admin query answers with roles and permissions, a merchant query
// with a store set, and a route must be one or the other. Merging them would
// make "which kind of caller is this" a runtime property of the answer instead
// of a property of the route.
type MerchantAuthorizer struct {
	users   merchantAccessClient
	timeout time.Duration
}

func NewMerchantAuthorizer(users merchantAccessClient, timeout time.Duration) (*MerchantAuthorizer, error) {
	if users == nil || timeout <= 0 {
		return nil, errors.New("merchant access client and a positive timeout are required")
	}
	return &MerchantAuthorizer{users: users, timeout: timeout}, nil
}

func (a *MerchantAuthorizer) middleware(next http.Handler) http.Handler {
	if a == nil {
		// A nil authorizer (a wiring bug, or a test passing nil) must fail
		// closed instead of panicking on the field access below.
		return authz.MerchantMiddleware(nil, 0)(next)
	}
	return authz.MerchantMiddleware(a.resolve, a.timeout)(next)
}

// resolve implements authz.MerchantResolver.
func (a *MerchantAuthorizer) resolve(ctx context.Context, accessToken string) (authz.MerchantGrants, error) {
	if a.users == nil {
		return authz.MerchantGrants{}, errors.New("authorizer has no merchant access client")
	}
	resp, err := a.users.GetMerchantAccess(auth.WithAccessToken(ctx, accessToken), &userv1.GetMerchantAccessRequest{})
	if err != nil {
		switch status.Code(err) {
		case codes.Unauthenticated:
			return authz.MerchantGrants{}, authz.ErrUnauthenticated
		case codes.PermissionDenied:
			return authz.MerchantGrants{}, authz.ErrForbidden
		default:
			// Unavailable included: a boundary that could not be fetched is never
			// replaced by one that was guessed.
			return authz.MerchantGrants{}, err
		}
	}
	return authz.MerchantGrants{
		MerchantID: resp.GetMerchantId(),
		ScopeType:  resp.GetScopeType(),
		ScopeID:    resp.GetScopeId(),
		StoreIDs:   resp.GetStoreIds(),
	}, nil
}
