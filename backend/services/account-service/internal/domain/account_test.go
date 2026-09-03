package domain

import (
	"context"
	"testing"
)

func TestPasswordHashAndCheck(t *testing.T) {
	hash, err := HashPassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "secret" || !CheckPassword(hash, "secret") || CheckPassword(hash, "wrong") {
		t.Fatal("password verification failed")
	}
}

func TestAuthenticateValidatesTypeAndStatus(t *testing.T) {
	hash, _ := HashPassword("secret")
	repo := fakeRepo{account: Account{ID: "a1", Username: "admin", PasswordHash: hash, Type: AdminAccount, Status: StatusActive}}
	service := NewService(repo)
	if _, err := service.Authenticate(context.Background(), "admin", "secret", AdminAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), "admin", "secret", MerchantAccount); err != ErrInvalidCredentials {
		t.Fatalf("type error = %v", err)
	}
	repo.account.Status = StatusDisabled
	if _, err := NewService(repo).Authenticate(context.Background(), "admin", "secret", AdminAccount); err != ErrAccountDisabled {
		t.Fatalf("status error = %v", err)
	}
}

type fakeRepo struct{ account Account }

func (f fakeRepo) GetByUsername(_ context.Context, _ string, _ AccountType) (Account, error) {
	return f.account, nil
}

func (f fakeRepo) GetByID(_ context.Context, _ string) (Account, error) { return f.account, nil }
