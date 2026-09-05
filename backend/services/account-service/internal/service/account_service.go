package service

import (
	"context"
	"errors"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAccountDisabled    = errors.New("account is disabled")
	ErrInvalidAccountType = errors.New("invalid account type")
	ErrInvalidAccount     = errors.New("invalid account")
)

type Service struct{ repo model.Repository }

func NewService(repo model.Repository) *Service { return &Service{repo: repo} }

func HashPassword(password string) (string, error) {
	if password == "" {
		return "", ErrInvalidCredentials
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func CheckPassword(hash, password string) bool {
	return password != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (s *Service) GetByID(ctx context.Context, id string) (model.Account, error) {
	if strings.TrimSpace(id) == "" {
		return model.Account{}, ErrInvalidAccount
	}
	return s.repo.GetByID(ctx, id)
}

func (s *Service) Authenticate(ctx context.Context, username, password string, accountType model.AccountType) (model.Account, error) {
	username = strings.TrimSpace(username)
	if username == "" || !validType(accountType) {
		if !validType(accountType) {
			return model.Account{}, ErrInvalidAccountType
		}
		return model.Account{}, ErrInvalidCredentials
	}
	account, err := s.repo.GetByUsername(ctx, username, accountType)
	if err != nil || account.Type != accountType {
		return model.Account{}, ErrInvalidCredentials
	}
	if account.Status != model.StatusActive {
		return model.Account{}, ErrAccountDisabled
	}
	if !CheckPassword(account.PasswordHash, password) {
		return model.Account{}, ErrInvalidCredentials
	}
	return account, nil
}

func validType(t model.AccountType) bool {
	return t == model.AdminAccount || t == model.MerchantAccount
}
