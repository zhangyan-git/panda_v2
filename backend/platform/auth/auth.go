package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrInvalidToken     = errors.New("invalid token")
	ErrInvalidIssuer    = errors.New("invalid issuer")
	ErrInvalidTokenType = errors.New("invalid token type")
	ErrMissingSubject   = errors.New("missing subject")
)

type TokenType string

const (
	AccessTokenType  TokenType = "access"
	RefreshTokenType TokenType = "refresh"
)

// Claims is the application payload carried by an access or refresh token.
type Claims struct {
	UserID    string    `json:"user_id"`
	AccountID string    `json:"account_id"`
	Tenant    string    `json:"tenant"`
	Roles     []string  `json:"roles,omitempty"`
	TokenType TokenType `json:"token_type"`
	jwt.RegisteredClaims
}

// Identity is the authenticated identity used by authorization checks.
type Identity struct {
	Subject string
	UserID  string
	Tenant  string
	Roles   []string
}

type Authorizer interface {
	Authorize(Identity, string, string) error
}

// Service signs and verifies HS256 tokens. The secret is supplied by the caller.
type Service struct {
	secret          []byte
	issuer          string
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

func NewService(secret []byte, issuer string, accessTokenTTL, refreshTokenTTL time.Duration) (*Service, error) {
	if len(secret) < 32 {
		return nil, errors.New("JWT secret must be at least 32 bytes")
	}
	if issuer == "" {
		return nil, errors.New("JWT issuer must not be empty")
	}
	if accessTokenTTL <= 0 || refreshTokenTTL <= 0 {
		return nil, errors.New("JWT token TTL must be positive")
	}
	return &Service{secret: append([]byte(nil), secret...), issuer: issuer, accessTokenTTL: accessTokenTTL, refreshTokenTTL: refreshTokenTTL}, nil
}

func (s *Service) SignAccess(subject, userID, accountID, tenant string, roles []string) (string, error) {
	return s.Sign(subject, userID, accountID, tenant, roles, AccessTokenType)
}

func (s *Service) SignRefresh(subject, userID, accountID, tenant string, roles []string) (string, error) {
	return s.Sign(subject, userID, accountID, tenant, roles, RefreshTokenType)
}

func (s *Service) AccessTokenTTL() time.Duration {
	return s.accessTokenTTL
}

func (s *Service) Sign(subject, userID, accountID, tenant string, roles []string, tokenType TokenType) (string, error) {
	if subject == "" {
		return "", ErrMissingSubject
	}
	if tokenType != AccessTokenType && tokenType != RefreshTokenType {
		return "", ErrInvalidTokenType
	}
	now := time.Now()
	ttl := s.accessTokenTTL
	if tokenType == RefreshTokenType {
		ttl = s.refreshTokenTTL
	}
	claims := Claims{
		UserID: userID, AccountID: accountID, Tenant: tenant,
		Roles: append([]string(nil), roles...), TokenType: tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.issuer, Subject: subject, ID: uuid.NewString(),
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

func (s *Service) Parse(tokenString string) (*Claims, error) {
	if tokenString == "" {
		return nil, ErrInvalidToken
	}
	var claims Claims
	token, err := jwt.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing algorithm: %s", token.Method.Alg())
		}
		return s.secret, nil
	}, jwt.WithIssuer(s.issuer), jwt.WithExpirationRequired(), jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil || token == nil || !token.Valid {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, jwt.ErrTokenExpired
		}
		if errors.Is(err, jwt.ErrTokenNotValidYet) {
			return nil, jwt.ErrTokenNotValidYet
		}
		if errors.Is(err, jwt.ErrTokenInvalidIssuer) {
			return nil, ErrInvalidIssuer
		}
		return nil, ErrInvalidToken
	}
	if claims.Subject == "" {
		return nil, ErrMissingSubject
	}
	if claims.NotBefore == nil {
		return nil, ErrInvalidToken
	}
	if claims.TokenType != AccessTokenType && claims.TokenType != RefreshTokenType {
		return nil, ErrInvalidTokenType
	}
	return &claims, nil
}

func (s *Service) ParseAccess(token string) (*Claims, error) {
	return s.parseType(token, AccessTokenType)
}

func (s *Service) ParseRefresh(token string) (*Claims, error) {
	return s.parseType(token, RefreshTokenType)
}

func (s *Service) parseType(token string, expected TokenType) (*Claims, error) {
	claims, err := s.Parse(token)
	if err != nil {
		return nil, err
	}
	if claims.TokenType != expected {
		return nil, ErrInvalidTokenType
	}
	return claims, nil
}
