package app

import (
	"context"
	"testing"

	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"
)

type recordingWriter struct{ accounts []model.Account }

func (w *recordingWriter) Upsert(_ context.Context, account model.Account) error {
	w.accounts = append(w.accounts, account)
	return nil
}

func TestInitializeDevelopmentAccountsRequiresDevAndFlag(t *testing.T) {
	writer := &recordingWriter{}
	cfg := config.Config{Environment: "prod", DevAccountInitEnabled: true, DevAdminUsername: "admin", DevAdminPassword: "secret"}
	if err := InitializeDevelopmentAccounts(context.Background(), cfg, writer); err != nil {
		t.Fatal(err)
	}
	if len(writer.accounts) != 0 {
		t.Fatal("initialized account outside dev")
	}
}

func TestInitializeDevelopmentAccountsHashesConfiguredAccounts(t *testing.T) {
	writer := &recordingWriter{}
	cfg := config.Config{Environment: "dev", DevAccountInitEnabled: true, DevAdminUsername: "admin", DevAdminPassword: "admin-secret", DevMerchantUsername: "merchant", DevMerchantPassword: "merchant-secret"}
	if err := InitializeDevelopmentAccounts(context.Background(), cfg, writer); err != nil {
		t.Fatal(err)
	}
	if len(writer.accounts) != 2 {
		t.Fatalf("initialized %d accounts, want 2", len(writer.accounts))
	}
	for _, account := range writer.accounts {
		if account.PasswordHash == "admin-secret" || account.PasswordHash == "merchant-secret" || !model.CheckPassword(account.PasswordHash, map[string]string{"admin": "admin-secret", "merchant": "merchant-secret"}[account.Username]) {
			t.Fatalf("password was not safely hashed: %+v", account)
		}
	}
}

func TestInitializeDevelopmentAccountsRejectsPartialCredentials(t *testing.T) {
	cfg := config.Config{Environment: "dev", DevAccountInitEnabled: true, DevAdminUsername: "admin"}
	if err := InitializeDevelopmentAccounts(context.Background(), cfg, &recordingWriter{}); err == nil {
		t.Fatal("expected partial credentials error")
	}
}
