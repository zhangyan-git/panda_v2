package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

type AdminUserService struct {
	users repository.AdminUserRepository
}

func NewAdminUserService(users repository.AdminUserRepository) *AdminUserService {
	return &AdminUserService{users: users}
}

// List 返回一页管理员及其总数。页码到 OFFSET 的换算放在这里而不是 handler：
// 那是分页这个概念的一部分，散到各处就会有人算错。
func (s *AdminUserService) List(ctx context.Context, page, pageSize int) ([]*model.AdminUser, int64, error) {
	users, err := s.users.FindPage(ctx, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.users.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

func (s *AdminUserService) GetByID(ctx context.Context, id string) (*model.AdminUser, error) {
	return s.users.FindByID(ctx, id)
}

func (s *AdminUserService) CreateUser(ctx context.Context, username, password, name, email string) (*model.AdminUser, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u := &model.AdminUser{
		ID:           uuid.NewString(),
		Username:     username,
		PasswordHash: string(hash),
		Name:         name,
		Email:        email,
		Status:       "active",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *AdminUserService) UpdateStatus(ctx context.Context, id, status string) error {
	return s.users.UpdateStatus(ctx, id, status)
}

// MerchantAuthService 商户账号登录/token 签发。
// 账号由平台在「商户管理」中创建，username 全局唯一，登录只需 username+password。
