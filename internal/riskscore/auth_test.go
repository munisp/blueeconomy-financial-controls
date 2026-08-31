package riskscore

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://keycloak.example/realms/blueeconomy"
	testAudience = "declaration-scorer"
	testSubject  = "service-account-port-interoperability"
)

type jwksFixture struct {
	key    *rsa.PrivateKey
	server *httptest.Server
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
		{Issuer: testIssuer, JWKSURL: "http://keys.example", Audience: testAudience},
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

func TestAuthenticateValidToken(t *testing.T) {
	fixture := newJWKSFixture(t)
	if err := fixture.authenticator(t).Authenticate(context.Background(), "Bearer "+fixture.token(t, nil)); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func TestAuthenticateFailClosed(t *testing.T) {
	fixture := newJWKSFixture(t)
	authenticator := fixture.authenticator(t)
	cases := map[string]string{
		"no header":       "",
		"not bearer":      "Basic abc",
		"garbage":         "Bearer not-a-jwt",
		"expired":         "Bearer " + fixture.token(t, func(c *keycloakClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) }),
		"no expiry":       "Bearer " + fixture.token(t, func(c *keycloakClaims) { c.ExpiresAt = nil }),
		"wrong audience":  "Bearer " + fixture.token(t, func(c *keycloakClaims) { c.Audience = jwt.ClaimStrings{"someone-else"} }),
		"wrong issuer":    "Bearer " + fixture.token(t, func(c *keycloakClaims) { c.Issuer = "https://evil.example/realms/blueeconomy" }),
		"missing subject": "Bearer " + fixture.token(t, func(c *keycloakClaims) { c.Subject = "" }),
	}
	for name, header := range cases {
		if err := authenticator.Authenticate(context.Background(), header); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestAuthenticateWrongKeyRejected(t *testing.T) {
	fixture := newJWKSFixture(t)
	authenticator := fixture.authenticator(t)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claims := &keycloakClaims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   testSubject,
		Audience:  jwt.ClaimStrings{testAudience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
	}}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "realm-key-1"
	signed, err := token.SignedString(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticator.Authenticate(context.Background(), "Bearer "+signed); err == nil {
		t.Fatal("token signed by an unknown key accepted")
	}
}

func TestHandlerRequiresAuthentication(t *testing.T) {
	fixture := newJWKSFixture(t)
	handler, err := NewHandler(testRules(), fixture.authenticator(t))
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	body, err := json.Marshal(validPayload())
	if err != nil {
		t.Fatal(err)
	}
	post := func(header string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/risk-scores", strings.NewReader(string(body)))
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := post(""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", recorder.Code)
	}
	if recorder := post("Bearer garbage"); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("garbage token = %d, want 401", recorder.Code)
	}
	if recorder := post("Bearer " + fixture.token(t, nil)); recorder.Code != http.StatusOK {
		t.Fatalf("valid token = %d: %s", recorder.Code, recorder.Body)
	}
}

func TestHandlerFailClosedWithoutAuthenticator(t *testing.T) {
	if _, err := NewHandler(testRules(), nil); err == nil {
		t.Fatal("handler accepted a nil authenticator")
	}
}

func TestHealthzStaysPublic(t *testing.T) {
	fixture := newJWKSFixture(t)
	handler, err := NewHandler(testRules(), fixture.authenticator(t))
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", recorder.Code)
	}
}
