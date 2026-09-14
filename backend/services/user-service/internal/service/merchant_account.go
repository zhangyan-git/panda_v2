package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrMerchantUsernameTaken = errors.New("用户名已存在")
	ErrScopeTypeInvalid      = errors.New("无效的数据范围类型")
	ErrScopeIDRequired       = errors.New("数据范围目标不能为空")
	ErrScopeOutOfMerchant    = errors.New("数据范围目标不属于该商户")
)

// MerchantAccountService manages merchant login accounts and their data scopes.
type MerchantAccountService struct {
	merchants MerchantAccessPort
	users     repository.MerchantUserRepository
	resources MerchantResourceAccess
}

func NewMerchantAccountService(merchants MerchantAccessPort, users repository.MerchantUserRepository, resources MerchantResourceAccess) *MerchantAccountService {
	return &MerchantAccountService{merchants: merchants, users: users, resources: resources}
}

// ListUsers 返回某商户下的一页账号及其总数。
//
// scope 名称只对当前页解析：那是逐行展示用的补充信息，给整批账号预先解析
// 等于把分页省下的开销又还回去。
func (s *MerchantAccountService) ListUsers(ctx context.Context, merchantID string, page, pageSize int) ([]*model.MerchantUser, int64, error) {
	if _, err := s.merchants.FindStatus(ctx, merchantID); err != nil {
		return nil, 0, err
	}
	users, err := s.users.FindPage(ctx, merchantID, pageSize, api.PageOffset(page, pageSize))
	if err != nil {
		return nil, 0, err
	}
	if err := s.decorateScopeNames(ctx, users...); err != nil {
		return nil, 0, err
	}
	total, err := s.users.Count(ctx, merchantID)
	if err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// decorateScopeNames fills in each account's scope display name. The names come
// from the merchant database, so one RPC answers the whole batch instead of a
// join the identity database can no longer make. A scope that no longer exists
// is absent from the answer and simply leaves that account's name empty:
// display data must not turn a listing into an error.
func (s *MerchantAccountService) decorateScopeNames(ctx context.Context, users ...*model.MerchantUser) error {
	var brandIDs, storeIDs []string
	for _, u := range users {
		switch {
		case u == nil || u.ScopeID == "":
		case u.ScopeType == "brand":
			brandIDs = append(brandIDs, u.ScopeID)
		case u.ScopeType == "store":
			storeIDs = append(storeIDs, u.ScopeID)
		}
	}
	if len(brandIDs) == 0 && len(storeIDs) == 0 {
		return nil
	}
	brandNames, storeNames, err := s.resources.ScopeNames(ctx, brandIDs, storeIDs)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u == nil {
			continue
		}
		switch u.ScopeType {
		case "brand":
			u.ScopeName = brandNames[u.ScopeID]
		case "store":
			u.ScopeName = storeNames[u.ScopeID]
		}
	}
	return nil
}

func (s *MerchantAccountService) validateScope(ctx context.Context, merchantID, scopeType, scopeID string) error {
	switch scopeType {
	case "", "merchant":
		return nil
	case "brand":
		if scopeID == "" {
			return ErrScopeIDRequired
		}
		merchantResourceID, err := s.resources.FindBrandMerchantID(ctx, scopeID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrScopeOutOfMerchant
			}
			return err
		}
		if merchantResourceID != merchantID {
			return ErrScopeOutOfMerchant
		}
		return nil
	case "store":
		if scopeID == "" {
			return ErrScopeIDRequired
		}
		merchantResourceID, err := s.resources.FindStoreMerchantID(ctx, scopeID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrScopeOutOfMerchant
			}
			return err
		}
		if merchantResourceID != merchantID {
			return ErrScopeOutOfMerchant
		}
		return nil
	default:
		return ErrScopeTypeInvalid
	}
}

func normalizeScope(scopeType, scopeID string) (string, string) {
	if scopeType == "" || scopeType == "merchant" {
		return "merchant", ""
	}
	return scopeType, scopeID
}

func (s *MerchantAccountService) CreateUser(ctx context.Context, merchantID, username, password, name, email, phone string, isAdmin bool, scopeType, scopeID string) (*model.MerchantUser, error) {
	if _, err := s.merchants.FindStatus(ctx, merchantID); err != nil {
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
	u := &model.MerchantUser{ID: uuid.NewString(), MerchantID: merchantID, Username: username, PasswordHash: string(hash), Name: name, Email: email, Phone: phone, Status: "active", IsAdmin: isAdmin, ScopeType: scopeType, ScopeID: scopeID, CreatedAt: now, UpdatedAt: now}
	if err := s.users.Create(ctx, u); err != nil {
		return nil, err
	}
	// 账号已经落库，这里只是把范围名称补进响应；解析失败就留空，
	// 不能让一次「已成功」的创建看起来像失败。列表接口会再查一次。
	_ = s.decorateScopeNames(ctx, u)
	return u, nil
}

func (s *MerchantAccountService) UpdateUserScope(ctx context.Context, id, scopeType, scopeID string, isAdmin bool) error {
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

func (s *MerchantAccountService) UpdateUserStatus(ctx context.Context, id, status string) error {
	if _, err := s.users.FindByID(ctx, id); err != nil {
		return err
	}
	return s.users.UpdateStatus(ctx, id, status)
}

func (s *MerchantAccountService) DeleteUser(ctx context.Context, id string) error {
	if _, err := s.users.FindByID(ctx, id); err != nil {
		return err
	}
	return s.users.Delete(ctx, id)
}
