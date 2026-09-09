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

// MerchantAccountService manages merchant login accounts and their data scopes.
type MerchantAccountService struct {
	merchants MerchantAccessPort
	users     repository.MerchantUserRepository
	resources MerchantResourceAccess
}

func NewMerchantAccountService(merchants MerchantAccessPort, users repository.MerchantUserRepository, resources MerchantResourceAccess) *MerchantAccountService {
	return &MerchantAccountService{merchants: merchants, users: users, resources: resources}
}

func (s *MerchantAccountService) ListUsers(ctx context.Context, merchantID string) ([]*model.MerchantUser, error) {
	if _, err := s.merchants.FindStatus(ctx, merchantID); err != nil {
		return nil, err
	}
	return s.users.FindByMerchant(ctx, merchantID)
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
