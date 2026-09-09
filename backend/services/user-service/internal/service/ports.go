package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// MerchantAccessPort exposes the small set of merchant facts needed by
// merchant accounts. It is a compatibility seam until merchant-service owns
// the backing implementation.
type MerchantAccessPort interface {
	FindStatus(ctx context.Context, merchantID string) (string, error)
	FindName(ctx context.Context, merchantID string) (string, error)
}

// RepositoryMerchantAccess adapts the current repository during the staged
// service split. It keeps account authentication independent from the full
// merchant CRUD repository contract.
type RepositoryMerchantAccess struct {
	merchants repository.MerchantRepository
}

func NewRepositoryMerchantAccess(merchants repository.MerchantRepository) *RepositoryMerchantAccess {
	return &RepositoryMerchantAccess{merchants: merchants}
}

func (a *RepositoryMerchantAccess) FindStatus(ctx context.Context, merchantID string) (string, error) {
	merchant, err := a.merchants.FindByID(ctx, merchantID)
	if err != nil {
		return "", err
	}
	return merchant.Status, nil
}

func (a *RepositoryMerchantAccess) FindName(ctx context.Context, merchantID string) (string, error) {
	merchant, err := a.merchants.FindByID(ctx, merchantID)
	if err != nil {
		return "", err
	}
	return merchant.Name, nil
}
