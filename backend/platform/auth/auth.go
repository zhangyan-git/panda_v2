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
	// Permissions carries the RBAC permission codes resolved at sign time, so a
	// service can authorize a request without calling back to account-service.
	// They therefore go stale until the token is refreshed.
	Permissions []string `json:"permissions,omitempty"`
	// IsSuper marks a super administrator, who passes every permission check.
	IsSuper bool `json:"is_super,omitempty"`
	// Scope is the data boundary resolved at sign time. Absent on tokens for
	// identities that carry no scope, which grants no data rather than all.
	Scope *Scope `json:"scope,omitempty"`
	jwt.RegisteredClaims
}

// Identity is the authenticated identity used by authorization checks.
type Identity struct {
	Subject     string
	UserID      string
	Tenant      string
	Roles       []string
	Permissions []string
	IsSuper     bool
	Scope       Scope
}

// HasPermission reports whether the identity may perform code. A super
// administrator is allowed everything.
func (i Identity) HasPermission(code string) bool {
	if i.IsSuper {
		return true
	}
	for _, granted := range i.Permissions {
		if granted == code {
			return true
		}
	}
	return false
}

// Grant is the authorization payload embedded in a signed token.
type Grant struct {
	Subject     string
	UserID      string
	AccountID   string
	Tenant      string
	Roles       []string
	Permissions []string
	IsSuper     bool
	Scope       Scope
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

// SignAccessGrant signs an access token carrying permissions and the super flag.
func (s *Service) SignAccessGrant(grant Grant) (string, error) {
	return s.SignGrant(grant, AccessTokenType)
}

// SignRefreshGrant signs a refresh token carrying permissions and the super flag.
func (s *Service) SignRefreshGrant(grant Grant) (string, error) {
	return s.SignGrant(grant, RefreshTokenType)
}

func (s *Service) AccessTokenTTL() time.Duration {
	return s.accessTokenTTL
}

func (s *Service) Sign(subject, userID, accountID, tenant string, roles []string, tokenType TokenType) (string, error) {
	return s.SignGrant(Grant{
		Subject: subject, UserID: userID, AccountID: accountID, Tenant: tenant, Roles: roles,
	}, tokenType)
}

func (s *Service) SignGrant(grant Grant, tokenType TokenType) (string, error) {
	if grant.Subject == "" {
		return "", ErrMissingSubject
	}
	if tokenType != AccessTokenType && tokenType != RefreshTokenType {
		return "", ErrInvalidTokenType
	}
	if grant.Scope.size() > MaxScopeIDs {
		return "", ErrScopeTooLarge
	}
	now := time.Now()
	ttl := s.accessTokenTTL
	if tokenType == RefreshTokenType {
		ttl = s.refreshTokenTTL
	}
	var scope *Scope
	if grant.Scope.Type != "" {
		signed := grant.Scope.clone()
		scope = &signed
	}
	claims := Claims{
		UserID: grant.UserID, AccountID: grant.AccountID, Tenant: grant.Tenant,
		Roles: append([]string(nil), grant.Roles...), TokenType: tokenType,
		Permissions: append([]string(nil), grant.Permissions...), IsSuper: grant.IsSuper,
		Scope: scope,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.issuer, Subject: grant.Subject, ID: uuid.NewString(),
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
