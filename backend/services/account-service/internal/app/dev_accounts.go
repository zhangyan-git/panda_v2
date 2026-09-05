package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

// InitializeDevelopmentAccounts creates or updates explicitly configured local accounts.
// It never runs outside the dev environment and never logs credentials.
func InitializeDevelopmentAccounts(ctx context.Context, cfg config.Config, writer model.AccountWriter) error {
	if !cfg.DevAccountInitEnabled || !strings.EqualFold(cfg.Environment, "dev") {
		return nil
	}
	if writer == nil {
		return errors.New("development account writer is required")
	}
	accounts := []struct {
		username    string
		password    string
		accountType model.AccountType
	}{
		{cfg.DevAdminUsername, cfg.DevAdminPassword, model.AdminAccount},
		{cfg.DevMerchantUsername, cfg.DevMerchantPassword, model.MerchantAccount},
	}
	for _, candidate := range accounts {
		if candidate.username == "" && candidate.password == "" {
			continue
		}
		if strings.TrimSpace(candidate.username) == "" || candidate.password == "" {
			return fmt.Errorf("development %s account requires username and password", candidate.accountType)
		}
		hash, err := model.HashPassword(candidate.password)
		if err != nil {
			return fmt.Errorf("hash development %s account password: %w", candidate.accountType, err)
		}
		if err := writer.Upsert(ctx, model.Account{Username: strings.TrimSpace(candidate.username), PasswordHash: hash, Type: candidate.accountType, Status: model.StatusActive}); err != nil {
			return fmt.Errorf("initialize development %s account: %w", candidate.accountType, err)
		}
		slog.Info("initialized development account", "type", candidate.accountType, "username", strings.TrimSpace(candidate.username))
	}
	return nil
}
