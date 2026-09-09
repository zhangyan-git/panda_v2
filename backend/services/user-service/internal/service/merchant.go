package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrMerchantNameRequired     = errors.New("商户名称不能为空")
	ErrMerchantStatusTransition = errors.New("不允许的商户状态流转")
	ErrMerchantHasUsers         = errors.New("商户下存在账号，无法删除")
	ErrMerchantUsernameTaken    = errors.New("用户名已存在")
	ErrScopeTypeInvalid         = errors.New("无效的数据范围类型")
	ErrScopeIDRequired          = errors.New("数据范围目标不能为空")
	ErrScopeOutOfMerchant       = errors.New("数据范围目标不属于该商户")
)

// AdminMerchantService 平台侧商户管理：商户 CRUD + 状态流转 + 商户账号维护
type AdminMerchantService struct {
	merchants repository.MerchantRepository
	users     repository.MerchantUserRepository
	brands    repository.BrandRepository
	stores    repository.StoreRepository
}

func NewAdminMerchantService(merchants repository.MerchantRepository, users repository.MerchantUserRepository, brands repository.BrandRepository, stores repository.StoreRepository) *AdminMerchantService {
	return &AdminMerchantService{merchants: merchants, users: users, brands: brands, stores: stores}
}

func (s *AdminMerchantService) List(ctx context.Context, name, status string) ([]*model.Merchant, error) {
	return s.merchants.FindAll(ctx, name, status)
}

func (s *AdminMerchantService) GetByID(ctx context.Context, id string) (*model.Merchant, error) {
	return s.merchants.FindByID(ctx, id)
}

// Create 新建商户，状态固定为 pending（待审核），只能走状态流转接口审核
func (s *AdminMerchantService) Create(ctx context.Context, name, contactName, contactPhone, contactEmail string) (*model.Merchant, error) {
	if name == "" {
		return nil, ErrMerchantNameRequired
	}
	now := time.Now()
	m := &model.Merchant{
		ID:           uuid.NewString(),
		Name:         name,
		Status:       "pending",
		ContactName:  contactName,
		ContactPhone: contactPhone,
		ContactEmail: contactEmail,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.merchants.Create(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Update 只更新名称与联系人信息，状态流转走 UpdateStatus
func (s *AdminMerchantService) Update(ctx context.Context, id, name, contactName, contactPhone, contactEmail string) (*model.Merchant, error) {
	if name == "" {
		return nil, ErrMerchantNameRequired
	}
	m, err := s.merchants.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	m.Name = name
	m.ContactName = contactName
	m.ContactPhone = contactPhone
	m.ContactEmail = contactEmail
	m.UpdatedAt = time.Now()
	if err := s.merchants.Update(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

// validMerchantTransition 商户状态机：pending→active（审核通过）、
// active→suspended（暂停）、suspended→active（恢复），其余一律拒绝
func validMerchantTransition(from, to string) bool {
	switch {
	case from == "pending" && to == "active":
		return true
	case from == "active" && to == "suspended":
		return true
	case from == "suspended" && to == "active":
		return true
	default:
		return false
	}
}

func (s *AdminMerchantService) UpdateStatus(ctx context.Context, id, status string) error {
	m, err := s.merchants.FindByID(ctx, id)
	if err != nil {
		return err
	}
	if !validMerchantTransition(m.Status, status) {
		return ErrMerchantStatusTransition
	}
	return s.merchants.UpdateStatus(ctx, id, status)
}

// Delete 删除商户；名下存在账号时拒绝，避免触发外键约束报 500
func (s *AdminMerchantService) Delete(ctx context.Context, id string) error {
	if _, err := s.merchants.FindByID(ctx, id); err != nil {
		return err
	}
	hasUsers, err := s.merchants.HasUsers(ctx, id)
	if err != nil {
		return err
	}
	if hasUsers {
		return ErrMerchantHasUsers
	}
	return s.merchants.Delete(ctx, id)
}

// ListUsers 商户下的登录账号列表
func (s *AdminMerchantService) ListUsers(ctx context.Context, merchantID string) ([]*model.MerchantUser, error) {
	if _, err := s.merchants.FindByID(ctx, merchantID); err != nil {
		return nil, err
	}
	return s.users.FindByMerchant(ctx, merchantID)
}

// validateScope 数据范围单点校验：
// merchant 级不允许带 scopeID；brand/store 级必须带且目标必须属于该商户。
// scope_id 无外键（多态指向），归属校验只能在服务层做。
func (s *AdminMerchantService) validateScope(ctx context.Context, merchantID, scopeType, scopeID string) error {
	switch scopeType {
	case "", "merchant":
		return nil
	case "brand":
		if scopeID == "" {
			return ErrScopeIDRequired
		}
		brand, err := s.brands.FindByID(ctx, scopeID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrScopeOutOfMerchant
			}
			return err
		}
		if brand.MerchantID != merchantID {
			return ErrScopeOutOfMerchant
		}
		return nil
	case "store":
		if scopeID == "" {
			return ErrScopeIDRequired
		}
		store, err := s.stores.FindByID(ctx, scopeID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrScopeOutOfMerchant
			}
			return err
		}
		if store.MerchantID != merchantID {
			return ErrScopeOutOfMerchant
		}
		return nil
	default:
		return ErrScopeTypeInvalid
	}
}

// normalizeScope 空 scope_type 归一化为 merchant，并清掉多余的 scopeID
func normalizeScope(scopeType, scopeID string) (string, string) {
	if scopeType == "" || scopeType == "merchant" {
		return "merchant", ""
	}
	return scopeType, scopeID
}

// CreateUser 为商户创建登录账号；username 全局唯一（003 迁移加约束）。
// 数据范围三选一（merchant/brand/store），单点关联，账号可看到该层级下全部数据。
func (s *AdminMerchantService) CreateUser(ctx context.Context, merchantID, username, password, name, email, phone string, isAdmin bool, scopeType, scopeID string) (*model.MerchantUser, error) {
	if _, err := s.merchants.FindByID(ctx, merchantID); err != nil {
		return nil, err
	}
	scopeType, scopeID = normalizeScope(scopeType, scopeID)
	if err := s.validateScope(ctx, merchantID, scopeType, scopeID); err != nil {
		return nil, err
	}
	if existing, err := s.users.FindByUsername(ctx, username); err == nil && existing != nil {
		return nil, ErrMerchantUsernameTaken
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u := &model.MerchantUser{
		ID:           uuid.NewString(),
		MerchantID:   merchantID,
		Username:     username,
		PasswordHash: string(hash),
		Name:         name,
		Email:        email,
		Phone:        phone,
		Status:       "active",
		IsAdmin:      isAdmin,
		ScopeType:    scopeType,
		ScopeID:      scopeID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// UpdateUserScope 调整账号数据范围（单点：merchant/brand/store 三选一）
func (s *AdminMerchantService) UpdateUserScope(ctx context.Context, id, scopeType, scopeID string, isAdmin bool) error {
	u, err := s.users.FindByID(ctx, id)
	if err != nil {
		return err
	}
	scopeType, scopeID = normalizeScope(scopeType, scopeID)
	if err := s.validateScope(ctx, u.MerchantID, scopeType, scopeID); err != nil {
		return err
	}
	return s.users.UpdateScope(ctx, id, scopeType, scopeID, isAdmin)
}

func (s *AdminMerchantService) UpdateUserStatus(ctx context.Context, id, status string) error {
	if _, err := s.users.FindByID(ctx, id); err != nil {
		return err
	}
	return s.users.UpdateStatus(ctx, id, status)
}

func (s *AdminMerchantService) DeleteUser(ctx context.Context, id string) error {
	if _, err := s.users.FindByID(ctx, id); err != nil {
		return err
	}
	return s.users.Delete(ctx, id)
}
