package repository

import (
	"context"
	"errors"
)

// ErrUnavailable reports that the remote ownership/identity dependency could not
// be reached. Handlers map it to 503 so the caller retries instead of assuming
// the operation succeeded.
var ErrUnavailable = errors.New("merchant repository is unavailable")

// MerchantUserRepository reclaims account scope on a deleted target. It is
// implemented by the user-service gRPC client, because merchant_users and the
// accounts it points at belong to the identity database.
type MerchantUserRepository interface {
	ResetScopeByTarget(context.Context, string, string) error
}
