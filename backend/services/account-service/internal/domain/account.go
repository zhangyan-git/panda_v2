package domain

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrAccountDisabled    = errors.New("account is disabled")
	ErrInvalidAccountType = errors.New("invalid account type")
	ErrInvalidAccount     = errors.New("invalid account")
)

type AccountType string

const (
	AdminAccount    AccountType = "admin"
	MerchantAccount AccountType = "merchant"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

type Account struct {
	ID           string      `json:"id"`
	Username     string      `json:"username"`
	PasswordHash string      `json:"-"`
	Type         AccountType `json:"type"`
	Tenant       string      `json:"tenant,omitempty"`
	UserID       string      `json:"user_id,omitempty"`
	Roles        []string    `json:"roles,omitempty"`
	Status       Status      `json:"status"`
}

type Repository interface {
	GetByUsername(context.Context, string, AccountType) (Account, error)
	GetByID(context.Context, string) (Account, error)
}

type AccountWriter interface {
	Upsert(context.Context, Account) error
}

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }

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

func (s *Service) GetByID(ctx context.Context, id string) (Account, error) {
	if strings.TrimSpace(id) == "" {
		return Account{}, ErrInvalidAccount
	}
	return s.repo.GetByID(ctx, id)
}

func (s *Service) Authenticate(ctx context.Context, username, password string, accountType AccountType) (Account, error) {
	username = strings.TrimSpace(username)
	if username == "" || !validType(accountType) {
		if !validType(accountType) {
			return Account{}, ErrInvalidAccountType
		}
		return Account{}, ErrInvalidCredentials
	}
	account, err := s.repo.GetByUsername(ctx, username, accountType)
	if err != nil {
		return Account{}, ErrInvalidCredentials
	}
	if account.Type != accountType {
		return Account{}, ErrInvalidCredentials
	}
	if account.Status != StatusActive {
		return Account{}, ErrAccountDisabled
	}
	if !CheckPassword(account.PasswordHash, password) {
		return Account{}, ErrInvalidCredentials
	}
	return account, nil
}

func validType(t AccountType) bool { return t == AdminAccount || t == MerchantAccount }
