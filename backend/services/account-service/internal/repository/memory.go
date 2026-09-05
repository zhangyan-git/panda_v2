package repository

import (
	"context"
	"errors"
	"sync"

	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

var ErrNotFound = errors.New("account not found")

type Memory struct {
	mu       sync.RWMutex
	accounts map[string]model.Account
}

func NewMemory(accounts ...model.Account) *Memory {
	r := &Memory{accounts: make(map[string]model.Account, len(accounts))}
	for _, account := range accounts {
		r.accounts[accountKey(account.Username, account.Type)] = account
	}
	return r
}

func (r *Memory) GetByUsername(_ context.Context, username string, accountType model.AccountType) (model.Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	account, ok := r.accounts[accountKey(username, accountType)]
	if !ok {
		return model.Account{}, ErrNotFound
	}
	account.Roles = append([]string(nil), account.Roles...)
	return account, nil
}

func (r *Memory) GetByID(_ context.Context, id string) (model.Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, account := range r.accounts {
		if account.ID == id {
			account.Roles = append([]string(nil), account.Roles...)
			return account, nil
		}
	}
	return model.Account{}, ErrNotFound
}

func (r *Memory) Put(account model.Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[accountKey(account.Username, account.Type)] = account
}

func accountKey(username string, accountType model.AccountType) string {
	return username + "\x00" + string(accountType)
}

func (r *Memory) Upsert(_ context.Context, account model.Account) error {
	r.Put(account)
	return nil
}
