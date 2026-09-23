package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

var (
	ErrMerchantNameRequired      = errors.New("商户名称不能为空")
	ErrMerchantStatusTransition  = errors.New("不允许的商户状态流转")
	ErrMerchantHasUsers          = errors.New("商户下存在账号，无法删除")
	ErrMerchantHasBrandsOrStores = errors.New("商户下存在品牌或门店，无法删除")
	ErrMerchantUsernameTaken     = errors.New("用户名已存在")
	ErrScopeTypeInvalid          = errors.New("无效的数据范围类型")
	ErrScopeIDRequired           = errors.New("数据范围目标不能为空")
	ErrScopeOutOfMerchant        = errors.New("数据范围目标不属于该商户")
)

// MerchantAccountPresence checks whether a merchant has accounts.
type MerchantAccountPresence interface {
	HasUsers(ctx context.Context, merchantID string) (bool, error)
}

// AdminMerchantService 平台侧商户管理：商户 CRUD + 状态流转
type AdminMerchantService struct {
	merchants       repository.MerchantRepository
	accountPresence MerchantAccountPresence
}

func NewAdminMerchantService(merchants repository.MerchantRepository, accountPresence MerchantAccountPresence) *AdminMerchantService {
	return &AdminMerchantService{merchants: merchants, accountPresence: accountPresence}
}

// List 返回一页商户及其总数。页码到 OFFSET 的换算只在这里做一次，
// 散到 handler 里各写一遍迟早会有人漏掉那个 -1。
func (s *AdminMerchantService) List(ctx context.Context, name, status string, page, pageSize int) ([]*model.Merchant, int64, error) {
	list, err := s.merchants.FindPage(ctx, name, status, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	total, err := s.merchants.Count(ctx, name, status)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
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

// Delete 删除商户；名下存在账号、品牌或门店时拒绝。
//
// 账号那条是跨库检查（identity 库），品牌/门店这条是本库的：brands 与 stores 对
// merchants 都是 ON DELETE CASCADE（migrations/merchant），不拦的话删除会静默连带品牌、
// 门店与两张审核表的历史一起消失，接口只回一个 200。
func (s *AdminMerchantService) Delete(ctx context.Context, id string) error {
	if _, err := s.merchants.FindByID(ctx, id); err != nil {
		return err
	}
	hasUsers, err := s.accountPresence.HasUsers(ctx, id)
	if err != nil {
		return err
	}
	if hasUsers {
		return ErrMerchantHasUsers
	}
	hasChildren, err := s.merchants.HasBrandsOrStores(ctx, id)
	if err != nil {
		return err
	}
	if hasChildren {
		return ErrMerchantHasBrandsOrStores
	}
	return s.merchants.Delete(ctx, id)
}
