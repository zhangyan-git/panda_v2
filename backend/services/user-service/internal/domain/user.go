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
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Nickname   string    `json:"nickname"`
	AvatarURL  string    `json:"avatar_url"`
	Email      string    `json:"email"`
	Gender     string    `json:"gender"`
	Birthday   string    `json:"birthday"`
	Occupation string    `json:"occupation"`
	Hobbies    []string  `json:"hobbies"`
	RegionCode string    `json:"region_code"`
	RegionName string    `json:"region_name"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type UserUpdate struct {
	Nickname   *string
	AvatarURL  *string
	Email      *string
	Gender     *string
	Birthday   *string
	Occupation *string
	Hobbies    *[]string
	RegionCode *string
	RegionName *string
}

type Repository interface {
	Create(context.Context, string) (User, error)
	GetByID(context.Context, int64) (User, error)
	Update(context.Context, int64, UserUpdate) (User, error)
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

func (s *Service) Update(ctx context.Context, id int64, update UserUpdate) (User, error) {
	if id <= 0 {
		return User{}, ErrNotFound
	}
	return s.repo.Update(ctx, id, update)
}
