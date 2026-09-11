package service

import "context"

// MerchantResourceAccess exposes only the brand/store facts merchant accounts
// need: which merchant owns one, and what a batch of them is called. It is
// implemented by the remote merchant-service client.
type MerchantResourceAccess interface {
	FindBrandMerchantID(ctx context.Context, brandID string) (string, error)
	FindStoreMerchantID(ctx context.Context, storeID string) (string, error)
	// ScopeNames resolves scope display names in one round trip. It replaces the
	// brand/store join this service used to run, which the split made impossible:
	// those tables belong to the merchant database.
	ScopeNames(ctx context.Context, brandIDs, storeIDs []string) (map[string]string, map[string]string, error)
}
