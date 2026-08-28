package cvffapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://keycloak.example/realms/blueeconomy-cvff"
	testAudience = "beneficiary-portal"
	testSubject  = "kc-beneficiary-001"
)

type jwksFixture struct {
	key       *rsa.PrivateKey
	server    *httptest.Server
	algorithm string
}

func newJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	fixture := &jwksFixture{key: key}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		modulus := base64.RawURLEncoding.EncodeToString(fixture.key.PublicKey.N.Bytes())
		exponent := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(fixture.key.PublicKey.E)).Bytes())
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"keys": []map[string]string{{
				"kid": "realm-key-1",
				"kty": "RSA",
				"alg": "RS256",
				"use": "sig",
				"n":   modulus,
				"e":   exponent,
			}},
		})
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *jwksFixture) token(t *testing.T, mutate func(*keycloakClaims)) string {
	t.Helper()
	claims := &keycloakClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   testSubject,
			Audience:  jwt.ClaimStrings{testAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	claims.RealmAccess.Roles = []string{BeneficiaryRole, "default-roles-blueeconomy-cvff"}
	if mutate != nil {
		mutate(claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "realm-key-1"
	signed, err := token.SignedString(fixture.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func (fixture *jwksFixture) authenticator(t *testing.T) *KeycloakAuthenticator {
	t.Helper()
	// The fixture constructs the authenticator directly so the TLS test
	// server's client is used; the production constructor path (https-only
	// config validation plus eager JWKS fetch) is covered by its own tests.
	authenticator := &KeycloakAuthenticator{
		config: KeycloakConfig{
			Issuer:   testIssuer,
			JWKSURL:  fixture.server.URL,
			Audience: testAudience,
		},
		httpClient: fixture.server.Client(),
		now:        time.Now,
		keys:       map[string]*rsa.PublicKey{},
	}
	if err := authenticator.refreshKeys(context.Background()); err != nil {
		t.Fatalf("refresh keys: %v", err)
	}
	return authenticator
}

func TestKeycloakConfigFailClosed(t *testing.T) {
	for _, config := range []KeycloakConfig{
		{},
		{Issuer: testIssuer},
		{Issuer: testIssuer, JWKSURL: "https://keys.example", Audience: " "},
		{Issuer: "http://insecure.example", JWKSURL: "https://keys.example", Audience: testAudience},
	} {
		if err := config.validate(); err == nil {
			t.Fatalf("config %+v accepted", config)
		}
	}
}

func TestAuthenticatorStartupRequiresReachableJWKS(t *testing.T) {
	if _, err := NewKeycloakAuthenticator(context.Background(), KeycloakConfig{
		Issuer:   testIssuer,
		JWKSURL:  "https://keys.invalid/jwks",
		Audience: testAudience,
	}); err == nil {
		t.Fatal("authenticator started against an unreachable JWKS endpoint")
	}
}

func TestAuthenticateValidBeneficiaryToken(t *testing.T) {
	fixture := newJWKSFixture(t)
	principal, err := fixture.authenticator(t).Authenticate(context.Background(), "Bearer "+fixture.token(t, nil))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if principal.Subject != testSubject || !principal.hasRole(BeneficiaryRole) {
		t.Fatalf("unexpected principal: %+v", principal)
	}
}

func TestAuthenticateFailClosed(t *testing.T) {
	fixture := newJWKSFixture(t)
	authenticator := fixture.authenticator(t)
	cases := map[string]string{
		"no header":    "",
		"not bearer":   "Basic abc",
		"empty token":  "Bearer ",
		"garbage":      "Bearer not-a-jwt",
		"expired":      "Bearer " + fixture.token(t, func(claims *keycloakClaims) { claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) }),
		"wrong issuer": "Bearer " + fixture.token(t, func(claims *keycloakClaims) { claims.Issuer = "https://keycloak.example/realms/other" }),
		"wrong audience": "Bearer " + fixture.token(t, func(claims *keycloakClaims) {
			claims.Audience = jwt.ClaimStrings{"another-client"}
		}),
		"no subject": "Bearer " + fixture.token(t, func(claims *keycloakClaims) { claims.Subject = "" }),
	}
	for name, header := range cases {
		if _, err := authenticator.Authenticate(context.Background(), header); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("case %s: error = %v, want ErrUnauthenticated", name, err)
		}
	}
}

func TestAuthenticateAudienceViaAuthorizedParty(t *testing.T) {
	fixture := newJWKSFixture(t)
	token := fixture.token(t, func(claims *keycloakClaims) {
		claims.Audience = jwt.ClaimStrings{"account"}
		claims.AuthorizedParty = testAudience
	})
	if _, err := fixture.authenticator(t).Authenticate(context.Background(), "Bearer "+token); err != nil {
		t.Fatalf("azp-bound token rejected: %v", err)
	}
}

func TestAuthenticateMissingBeneficiaryRoleForbidden(t *testing.T) {
	fixture := newJWKSFixture(t)
	token := fixture.token(t, func(claims *keycloakClaims) {
		claims.RealmAccess.Roles = []string{"cbn-observer"}
	})
	if _, err := fixture.authenticator(t).Authenticate(context.Background(), "Bearer "+token); !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}

func TestAuthenticateTamperedSignature(t *testing.T) {
	fixture := newJWKSFixture(t)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	claims := &keycloakClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   testSubject,
			Audience:  jwt.ClaimStrings{testAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
		},
	}
	claims.RealmAccess.Roles = []string{BeneficiaryRole}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "realm-key-1"
	signed, err := token.SignedString(otherKey)
	if err != nil {
		t.Fatalf("sign with other key: %v", err)
	}
	if _, err := fixture.authenticator(t).Authenticate(context.Background(), "Bearer "+signed); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("tampered token accepted: %v", err)
	}
}

func TestAuthenticateRejectsNonRS256(t *testing.T) {
	fixture := newJWKSFixture(t)
	authenticator := fixture.authenticator(t)
	// alg=none tokens must never verify.
	token := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"iss": testIssuer,
		"sub": testSubject,
		"aud": testAudience,
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	})
	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none token: %v", err)
	}
	if _, err := authenticator.Authenticate(context.Background(), "Bearer "+signed); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("alg=none token accepted: %v", err)
	}
}

type stubAuthenticator struct {
	principal Principal
	err       error
}

func (authenticator stubAuthenticator) Authenticate(_ context.Context, _ string) (Principal, error) {
	return authenticator.principal, authenticator.err
}

func TestRequireAuthMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal := principalFrom(request.Context())
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(principal.Subject))
	})
	wrapped := RequireAuth(stubAuthenticator{principal: Principal{Subject: testSubject, Roles: []string{BeneficiaryRole}}}, ok)
	recorder := httptest.NewRecorder()
	wrapped.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/cvff/applications", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != testSubject {
		t.Fatalf("authenticated request: %d %q", recorder.Code, recorder.Body.String())
	}

	for status, err := range map[int]error{http.StatusUnauthorized: ErrUnauthenticated, http.StatusForbidden: ErrForbidden} {
		recorder := httptest.NewRecorder()
		RequireAuth(stubAuthenticator{err: err}, ok).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/cvff/applications", nil))
		if recorder.Code != status {
			t.Fatalf("error %v mapped to %d, want %d", err, recorder.Code, status)
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
			t.Fatalf("auth failure content type = %q", contentType)
		}
	}
}
