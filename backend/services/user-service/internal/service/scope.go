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
	// ListStoreIDs turns a data scope into the set of stores it authorizes.
	//
	// The expansion lives in merchant-service because only the store table knows
	// what 品牌档 means on it. Doing it here instead would mean this service
	// reading a database it does not own (§5.2.4), and doing it at each consumer
	// would mean three implementations of one rule.
	//
	// scopeIDs is the whole set of targets, not one: several brands widen the
	// answer to the union of their stores. Empty at merchant level.
	//
	// The result is never nil for an empty scope — see the note on the repository
	// method — but callers must not read nil as "everything" either way.
	ListStoreIDs(ctx context.Context, merchantID, scopeType string, scopeIDs []string) ([]string, error)
}
