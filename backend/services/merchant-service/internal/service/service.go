package service

import (
	"context"
	"errors"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

type Service struct{ repo repository.Repository }

func New(repo repository.Repository) *Service { return &Service{repo: repo} }
func (s *Service) ListMerchants(ctx context.Context) ([]model.Merchant, error) {
	return s.repo.ListMerchants(ctx)
}
func ValidateMerchant(m model.Merchant) error {
	if strings.TrimSpace(m.Name) == "" {
		return ErrInvalidMerchant
	}
	return nil
}

var ErrInvalidMerchant = errors.New("invalid merchant")
