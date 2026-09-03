package domain

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrInvalidName = errors.New("user name is required")
var ErrNotFound = errors.New("user not found")

type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Repository interface {
	Create(context.Context, string) (User, error)
	GetByID(context.Context, int64) (User, error)
}

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

func (s *Service) Register(ctx context.Context, name string) (User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return User{}, ErrInvalidName
	}
	return s.repo.Create(ctx, name)
}

func (s *Service) GetByID(ctx context.Context, id int64) (User, error) {
	if id <= 0 {
		return User{}, ErrNotFound
	}
	return s.repo.GetByID(ctx, id)
}
