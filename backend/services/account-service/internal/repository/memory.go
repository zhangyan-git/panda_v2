package repository

import (
	"context"
	"errors"
	"sync"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/domain"
)

var ErrNotFound = errors.New("account not found")

type Memory struct {
	mu       sync.RWMutex
	accounts map[string]domain.Account
}

func NewMemory(accounts ...domain.Account) *Memory {
	r := &Memory{accounts: make(map[string]domain.Account, len(accounts))}
	for _, account := range accounts {
		r.accounts[accountKey(account.Username, account.Type)] = account
	}
	return r
}

func (r *Memory) GetByUsername(_ context.Context, username string, accountType domain.AccountType) (domain.Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	account, ok := r.accounts[accountKey(username, accountType)]
	if !ok {
		return domain.Account{}, ErrNotFound
	}
	account.Roles = append([]string(nil), account.Roles...)
	return account, nil
}

func (r *Memory) GetByID(_ context.Context, id string) (domain.Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, account := range r.accounts {
		if account.ID == id {
			account.Roles = append([]string(nil), account.Roles...)
			return account, nil
		}
	}
	return domain.Account{}, ErrNotFound
}

func (r *Memory) Put(account domain.Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[accountKey(account.Username, account.Type)] = account
}

func accountKey(username string, accountType domain.AccountType) string {
	return username + "\x00" + string(accountType)
}

func (r *Memory) Upsert(_ context.Context, account domain.Account) error {
	r.Put(account)
	return nil
}
