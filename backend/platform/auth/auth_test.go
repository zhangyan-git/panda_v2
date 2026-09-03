package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer = "panda-test"
	testSecret = "01234567890123456789012345678901"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	service, err := NewService([]byte(testSecret), testIssuer, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestNewServiceSecretLengthBoundary(t *testing.T) {
	if _, err := NewService([]byte("0123456789012345678901234567890"), testIssuer, time.Hour, 24*time.Hour); err == nil {
		t.Fatal("NewService accepted a secret shorter than 32 bytes")
	}
	if _, err := NewService([]byte(testSecret), testIssuer, time.Hour, 24*time.Hour); err != nil {
		t.Fatalf("NewService rejected a 32-byte secret: %v", err)
	}
}

func signTestClaims(t *testing.T, secret []byte, method jwt.SigningMethod, claims Claims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func validTestClaims(tokenType TokenType) Claims {
	now := time.Now()
	return Claims{
		UserID: "user-1", AccountID: "account-1", Tenant: "tenant-1", Roles: []string{"user"}, TokenType: tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: testIssuer, Subject: "subject-1", ID: "jti-1",
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
}

func TestServiceSignAndParse(t *testing.T) {
	service := newTestService(t)
	token, err := service.SignAccess("subject-1", "user-1", "account-1", "tenant-1", []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.ParseAccess(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "subject-1" || claims.UserID != "user-1" || claims.AccountID != "account-1" || claims.Tenant != "tenant-1" || claims.TokenType != AccessTokenType || claims.ID == "" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestParseRejectsWrongSignature(t *testing.T) {
	service := newTestService(t)
	token := signTestClaims(t, []byte("wrong-secret"), jwt.SigningMethodHS256, validTestClaims(AccessTokenType))
	if _, err := service.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Parse error = %v, want ErrInvalidToken", err)
	}
}

func TestParseRejectsWrongAlgorithm(t *testing.T) {
	service := newTestService(t)
	token := signTestClaims(t, []byte(testSecret), jwt.SigningMethodHS384, validTestClaims(AccessTokenType))
	if _, err := service.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Parse error = %v, want ErrInvalidToken", err)
	}
}

func TestParseRejectsWrongIssuer(t *testing.T) {
	service := newTestService(t)
	claims := validTestClaims(AccessTokenType)
	claims.Issuer = "other-issuer"
	token := signTestClaims(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
	if _, err := service.Parse(token); !errors.Is(err, ErrInvalidIssuer) {
		t.Fatalf("Parse error = %v, want ErrInvalidIssuer", err)
	}
}

func TestParseRejectsExpiredToken(t *testing.T) {
	service := newTestService(t)
	claims := validTestClaims(AccessTokenType)
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	token := signTestClaims(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
	if _, err := service.Parse(token); !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("Parse error = %v, want jwt.ErrTokenExpired", err)
	}
}

func TestParseRejectsNotBefore(t *testing.T) {
	service := newTestService(t)
	claims := validTestClaims(AccessTokenType)
	claims.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Minute))
	token := signTestClaims(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
	if _, err := service.Parse(token); !errors.Is(err, jwt.ErrTokenNotValidYet) {
		t.Fatalf("Parse error = %v, want jwt.ErrTokenNotValidYet", err)
	}
}

func TestParseRejectsMissingSubject(t *testing.T) {
	service := newTestService(t)
	claims := validTestClaims(AccessTokenType)
	claims.Subject = ""
	token := signTestClaims(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
	if _, err := service.Parse(token); !errors.Is(err, ErrMissingSubject) {
		t.Fatalf("Parse error = %v, want ErrMissingSubject", err)
	}
}

func TestParseRejectsInvalidTokenType(t *testing.T) {
	service := newTestService(t)
	claims := validTestClaims(TokenType("unknown"))
	token := signTestClaims(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
	if _, err := service.Parse(token); !errors.Is(err, ErrInvalidTokenType) {
		t.Fatalf("Parse error = %v, want ErrInvalidTokenType", err)
	}
}

func TestParseTypeRejectsOtherTokenType(t *testing.T) {
	service := newTestService(t)
	token, err := service.SignRefresh("subject-1", "user-1", "account-1", "tenant-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ParseAccess(token); !errors.Is(err, ErrInvalidTokenType) {
		t.Fatalf("ParseAccess error = %v, want ErrInvalidTokenType", err)
	}
}
