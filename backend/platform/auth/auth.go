package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrInvalidToken     = errors.New("invalid token")
	ErrInvalidIssuer    = errors.New("invalid issuer")
	ErrInvalidTokenType = errors.New("invalid token type")
	ErrMissingSubject   = errors.New("missing subject")
	ErrInvalidRealm     = errors.New("invalid or missing realm")
)

type TokenType string

const (
	AccessTokenType  TokenType = "access"
	RefreshTokenType TokenType = "refresh"
)

// Realm names which population of principals a token was minted for. All three
// share one signing key and one issuer, so the claim is the only thing that
// separates them.
//
// It exists because the previous discriminator was structural — "a platform
// administrator is a token whose Subject equals its UserID and whose Tenant is
// empty" — and a C-end (miniapp) token satisfies both. Every gate that tested
// only those two conditions treated a signed-in customer as an administrator,
// and the ones that stopped them did so by accident (the id happened not to
// exist in admin_users) rather than by construction.
type Realm string

const (
	// RealmPlatform is a platform administrator.
	RealmPlatform Realm = "platform"
	// RealmMerchant is a merchant employee. Merchant tokens also carry a
	// non-empty Tenant; the realm states it outright instead of relying on that.
	RealmMerchant Realm = "merchant"
	// RealmConsumer is a miniapp (C-end) customer. Consumer tokens are the ones
	// that would otherwise be indistinguishable from administrator tokens.
	RealmConsumer Realm = "consumer"
)

// Valid reports whether r is one of the defined realms.
func (r Realm) Valid() bool {
	switch r {
	case RealmPlatform, RealmMerchant, RealmConsumer:
		return true
	default:
		return false
	}
}

// Claims is the application payload carried by an access or refresh token.
type Claims struct {
	UserID    string    `json:"user_id"`
	AccountID string    `json:"account_id"`
	Tenant    string    `json:"tenant"`
	Roles     []string  `json:"roles,omitempty"`
	TokenType TokenType `json:"token_type"`
	// Realm names the population this token was minted for. Omitted on tokens
	// signed before the field existed; Parse resolves that absence to
	// RealmPlatform, which is what those tokens in fact were.
	Realm Realm `json:"realm,omitempty"`
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
	Realm       Realm
	Roles       []string
	Permissions []string
	IsSuper     bool
	Scope       Scope
}

// IsPlatformAdmin reports whether the identity is a platform administrator's.
//
// Every gate that admits only platform administrators must ask this rather than
// restating the conditions. The test it replaces — a token whose subject equals
// its user id and whose tenant is empty — is also true of a C-end token, so a
// guard written that way admits a miniapp customer. The realm is the part that
// cannot be inferred from the shape of the other claims, which is why it is
// checked first.
//
// An empty realm counts as platform, matching how Parse resolves the absence of
// the claim: tokens issued before the realm existed were platform tokens, and
// the two spellings must not disagree about the same identity. That cannot be
// reached by omission on a C-end token — SignGrant refuses to mint one without a
// realm — so a consumer identity is only ever denied here for saying so.
func (i Identity) IsPlatformAdmin() bool {
	realm := i.Realm
	if realm == "" {
		realm = RealmPlatform
	}
	return realm == RealmPlatform && i.Subject == i.UserID && i.Tenant == ""
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
	Subject   string
	UserID    string
	AccountID string
	Tenant    string
	// Realm is required by SignGrant: a grant that does not say which population
	// it is for cannot be told apart from one minted for another, and the whole
	// point of the field is that the distinction is explicit at mint time.
	Realm       Realm
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

// placeholderSecrets are signing keys this repository publishes verbatim in its
// example configuration (deploy/config/.env.example). They are deliberately long
// enough to clear the length check below, which is exactly why the length check
// alone is not enough: `cp .env.example .env` would otherwise start every service
// on a key anyone who can read the repository can sign a token with.
//
// The comparison is exact, against the trimmed value, so surrounding whitespace
// from a copy-paste cannot sneak one past. It only catches the values shipped
// here; choosing a secret no one else knows is still the operator's job, and the
// error says how.
var placeholderSecrets = map[string]struct{}{
	"replace-with-a-long-random-secret": {},
}

func NewService(secret []byte, issuer string, accessTokenTTL, refreshTokenTTL time.Duration) (*Service, error) {
	if _, placeholder := placeholderSecrets[strings.TrimSpace(string(secret))]; placeholder {
		return nil, fmt.Errorf("JWT secret is the placeholder published in deploy/config/.env.example (%q); "+
			"it is not a secret, so anyone could sign a token this service would accept — "+
			"replace it with a value of your own, for example the output of `openssl rand -hex 32`", string(secret))
	}
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

// SignGrant is the only way to mint a token, and it is the only way on purpose:
// it takes a Grant, so every caller has to say which realm the token is for.
// The positional-argument signers it replaced took no realm, which meant a mint
// site could omit it and silently produce a platform administrator's token.
func (s *Service) SignGrant(grant Grant, tokenType TokenType) (string, error) {
	if grant.Subject == "" {
		return "", ErrMissingSubject
	}
	if tokenType != AccessTokenType && tokenType != RefreshTokenType {
		return "", ErrInvalidTokenType
	}
	// Refusing an unset realm rather than defaulting it: a caller that forgot
	// would otherwise mint a console token for a miniapp user, and this is the
	// one mistake the field exists to make impossible.
	if !grant.Realm.Valid() {
		return "", ErrInvalidRealm
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
		Realm:       grant.Realm,
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
	// A token minted before the realm existed carries none. Those tokens were
	// only ever platform or merchant ones — no C-end realm shipped yet — so
	// resolving the absence to RealmPlatform preserves exactly the behaviour
	// they had. A token carrying a realm we do not define is rejected instead of
	// guessed at: it means someone signed it with a vocabulary this build does
	// not know, and defaulting it to platform would be the unsafe direction.
	switch {
	case claims.Realm == "":
		claims.Realm = RealmPlatform
	case !claims.Realm.Valid():
		return nil, ErrInvalidRealm
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
