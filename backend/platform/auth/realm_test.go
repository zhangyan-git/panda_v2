package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// signRawRealm mints a token carrying an arbitrary realm without going through
// SignGrant. That is the point: it stands in for a token signed by a build whose
// vocabulary Parse does not share, so the test proves Parse judges the claim
// instead of trusting it. An empty realm is omitted from the payload entirely,
// which is exactly the shape of a token minted before the claim existed.
func signRawRealm(t *testing.T, realm Realm) string {
	t.Helper()
	now := time.Now()
	claims := Claims{
		UserID: "admin-1", TokenType: AccessTokenType, Realm: realm,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: testIssuer, Subject: "admin-1", ID: "raw-1",
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A grant that does not name a realm must not be signed. Defaulting it would
// hand the most privileged realm to whichever mint site forgot to say.
func TestSignGrantRefusesToMintWithoutAKnownRealm(t *testing.T) {
	service := newTestService(t)
	for _, tt := range []struct {
		name  string
		realm Realm
		want  error
	}{
		{"unset", "", ErrInvalidRealm},
		{"undefined", Realm("admin"), ErrInvalidRealm},
		{"platform", RealmPlatform, nil},
		{"merchant", RealmMerchant, nil},
		{"consumer", RealmConsumer, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.SignAccessGrant(Grant{Subject: "s1", Realm: tt.realm})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// Tokens signed before the realm existed were platform or merchant ones, so
// resolving the absence to platform keeps them working. Rejecting them would
// log out every administrator holding a live token.
func TestParseResolvesAPreRealmTokenToPlatform(t *testing.T) {
	service := newTestService(t)
	claims, err := service.Parse(signRawRealm(t, ""))
	if err != nil {
		t.Fatalf("Parse rejected a token with no realm claim: %v", err)
	}
	if claims.Realm != RealmPlatform {
		t.Fatalf("realm = %q, want %q", claims.Realm, RealmPlatform)
	}
}

// A realm this build does not define is rejected rather than guessed at:
// defaulting an unknown vocabulary to platform is the unsafe direction.
func TestParseRejectsAnUndefinedRealm(t *testing.T) {
	service := newTestService(t)
	if _, err := service.Parse(signRawRealm(t, Realm("admin"))); !errors.Is(err, ErrInvalidRealm) {
		t.Fatalf("err = %v, want ErrInvalidRealm", err)
	}
}

// TestIsPlatformAdminIsDecidedByRealm is the property the guard sites rely on.
// The first three rows are the whole reason the field exists: they are the same
// identity shape — subject equals user id, no tenant — and the structural test
// this replaced called all three of them an administrator.
func TestIsPlatformAdminIsDecidedByRealm(t *testing.T) {
	for _, tt := range []struct {
		name     string
		identity Identity
		want     bool
	}{
		{"platform administrator", Identity{Realm: RealmPlatform, Subject: "a", UserID: "a"}, true},
		{"pre-realm token", Identity{Subject: "a", UserID: "a"}, true},
		{"consumer of the same shape", Identity{Realm: RealmConsumer, Subject: "a", UserID: "a"}, false},
		{"merchant of the same shape", Identity{Realm: RealmMerchant, Subject: "a", UserID: "a"}, false},
		{"platform realm with a tenant", Identity{Realm: RealmPlatform, Subject: "a", UserID: "a", Tenant: "m1"}, false},
		{"platform realm with a mismatched subject", Identity{Realm: RealmPlatform, Subject: "b", UserID: "a"}, false},
		{"consumer carrying a user id", Identity{Realm: RealmConsumer, Subject: "a", UserID: "a", Tenant: ""}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.identity.IsPlatformAdmin(); got != tt.want {
				t.Fatalf("IsPlatformAdmin() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The claim has to survive a real round trip, not just a struct literal: a
// consumer token is only distinguishable from an administrator token after the
// realm is actually read back off the wire.
func TestConsumerRealmSurvivesTheRoundTrip(t *testing.T) {
	service := newTestService(t)
	token, err := service.SignAccessGrant(Grant{Realm: RealmConsumer, Subject: "user-1", UserID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Realm: claims.Realm, Subject: claims.Subject, UserID: claims.UserID, Tenant: claims.Tenant}
	if identity.IsPlatformAdmin() {
		t.Fatalf("a C-end token parsed as a platform administrator: %+v", identity)
	}
}
