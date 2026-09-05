package service

import (
	"context"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

type Service struct{ repo model.Repository }

func NewService(repo model.Repository) *Service { return &Service{repo: repo} }

func (s *Service) Register(ctx context.Context, name string) (model.User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return model.User{}, model.ErrInvalidName
	}
	return s.repo.Create(ctx, name)
}
func (s *Service) GetByID(ctx context.Context, id int64) (model.User, error) {
	if id <= 0 {
		return model.User{}, model.ErrNotFound
	}
	return s.repo.GetByID(ctx, id)
}
func (s *Service) Update(ctx context.Context, id int64, update model.UserUpdate) (model.User, error) {
	if id <= 0 {
		return model.User{}, model.ErrNotFound
	}
	return s.repo.Update(ctx, id, update)
}
