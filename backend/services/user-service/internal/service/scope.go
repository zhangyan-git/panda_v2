package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// MerchantResourceAccess exposes only ownership checks needed by merchant accounts.
type MerchantResourceAccess interface {
	FindBrandMerchantID(ctx context.Context, brandID string) (string, error)
	FindStoreMerchantID(ctx context.Context, storeID string) (string, error)
}

// RepositoryMerchantResourceAccess adapts the current repositories while resource
// ownership remains in this service boundary.
type RepositoryMerchantResourceAccess struct {
	brands repository.BrandRepository
	stores repository.StoreRepository
}

func NewRepositoryMerchantResourceAccess(brands repository.BrandRepository, stores repository.StoreRepository) *RepositoryMerchantResourceAccess {
	return &RepositoryMerchantResourceAccess{brands: brands, stores: stores}
}

func (a *RepositoryMerchantResourceAccess) FindBrandMerchantID(ctx context.Context, brandID string) (string, error) {
	brand, err := a.brands.FindByID(ctx, brandID)
	if err != nil {
		return "", err
	}
	return brand.MerchantID, nil
}

func (a *RepositoryMerchantResourceAccess) FindStoreMerchantID(ctx context.Context, storeID string) (string, error) {
	store, err := a.stores.FindByID(ctx, storeID)
	if err != nil {
		return "", err
	}
	return store.MerchantID, nil
}
