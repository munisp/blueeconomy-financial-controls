package cvffapi

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// BeneficiaryRole is the approved realm role a caller must hold in the CVFF
// realm to use the beneficiary API.
const BeneficiaryRole = "beneficiary"

var (
	// ErrUnauthenticated marks a missing, malformed or unverifiable bearer
	// token. It maps to HTTP 401.
	ErrUnauthenticated = errors.New("bearer token is absent or unverifiable")
	// ErrForbidden marks a verified token without the required beneficiary
	// realm role. It maps to HTTP 403.
	ErrForbidden = errors.New("bearer token does not hold the beneficiary role")
)

// Principal is the verified identity behind one request. Subject is the
// Keycloak `sub` claim and is the durable beneficiary ownership key.
type Principal struct {
	Subject string
	Roles   []string
}

// Authenticator verifies the Authorization header of one request.
type Authenticator interface {
	Authenticate(ctx context.Context, authorizationHeader string) (Principal, error)
}

// KeycloakConfig carries the fail-closed verification coordinates for the
// CVFF realm. Issuer is the exact realm issuer URL, JWKSURL the realm JWK
// endpoint and Audience the approved client (matched against `aud` or `azp`).
type KeycloakConfig struct {
	Issuer   string
	JWKSURL  string
	Audience string
}

func (config KeycloakConfig) validate() error {
	for name, value := range map[string]string{"issuer": config.Issuer, "jwks_url": config.JWKSURL, "audience": config.Audience} {
		if strings.TrimSpace(value) != value || value == "" {
			return fmt.Errorf("keycloak %s is required and must be canonical", name)
		}
	}
	if !strings.HasPrefix(config.Issuer, "https://") || !strings.HasPrefix(config.JWKSURL, "https://") {
		return errors.New("keycloak issuer and JWKS URL must use https")
	}
	return nil
}

// keycloakClaims mirrors the CVFF realm access-token shape.
type keycloakClaims struct {
	jwt.RegisteredClaims
	AuthorizedParty string `json:"azp"`
	RealmAccess     struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// jwksDocument is the realm JWK set (RSA keys only).
type jwksDocument struct {
	Keys []struct {
		KeyID     string `json:"kid"`
		KeyType   string `json:"kty"`
		Algorithm string `json:"alg"`
		Use       string `json:"use"`
		Modulus   string `json:"n"`
		Exponent  string `json:"e"`
	} `json:"keys"`
}

// KeycloakAuthenticator verifies Keycloak RS256 access tokens against the
// realm JWKS. Keys are cached and refreshed on unknown key IDs and on a
// bounded interval; every verification failure is fail-closed.
type KeycloakAuthenticator struct {
	config     KeycloakConfig
	httpClient *http.Client
	now        func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// NewKeycloakAuthenticator fails closed when the configuration is incomplete
// and eagerly fetches the realm keys so a misconfigured endpoint stops the
// service at startup, not at first request.
func NewKeycloakAuthenticator(ctx context.Context, config KeycloakConfig) (*KeycloakAuthenticator, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	authenticator := &KeycloakAuthenticator{
		config: config,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		now:  time.Now,
		keys: map[string]*rsa.PublicKey{},
	}
	if err := authenticator.refreshKeys(ctx); err != nil {
		return nil, fmt.Errorf("fetch realm JWKS: %w", err)
	}
	return authenticator, nil
}

// jwksMinRefreshInterval bounds how often an unknown key ID may trigger a
// JWKS reload so a flood of forged tokens cannot stampede the realm endpoint.
const jwksMinRefreshInterval = 30 * time.Second

func (authenticator *KeycloakAuthenticator) refreshKeys(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, authenticator.config.JWKSURL, nil)
	if err != nil {
		return fmt.Errorf("build JWKS request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := authenticator.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request JWKS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned HTTP %d", response.StatusCode)
	}
	var document jwksDocument
	if err := json.NewDecoder(http.MaxBytesReader(nil, response.Body, 1<<20)).Decode(&document); err != nil {
		return fmt.Errorf("decode JWKS document: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, jwk := range document.Keys {
		if jwk.KeyType != "RSA" || jwk.KeyID == "" {
			continue
		}
		if jwk.Use != "" && jwk.Use != "sig" {
			continue
		}
		modulusBytes, err := base64.RawURLEncoding.DecodeString(jwk.Modulus)
		if err != nil {
			return fmt.Errorf("decode JWKS modulus for kid %q: %w", jwk.KeyID, err)
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(jwk.Exponent)
		if err != nil {
			return fmt.Errorf("decode JWKS exponent for kid %q: %w", jwk.KeyID, err)
		}
		exponent := 0
		for _, octet := range exponentBytes {
			exponent = exponent<<8 | int(octet)
		}
		if exponent < 3 {
			return fmt.Errorf("JWKS exponent for kid %q is not a valid RSA exponent", jwk.KeyID)
		}
		modulus := new(big.Int).SetBytes(modulusBytes)
		if modulus.BitLen() < 2048 {
			return fmt.Errorf("JWKS modulus for kid %q is below 2048 bits", jwk.KeyID)
		}
		keys[jwk.KeyID] = &rsa.PublicKey{N: modulus, E: exponent}
	}
	if len(keys) == 0 {
		return errors.New("JWKS document contains no RSA signing keys")
	}
	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	authenticator.keys = keys
	authenticator.fetchedAt = authenticator.now()
	return nil
}

func (authenticator *KeycloakAuthenticator) keyFor(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	authenticator.mu.Lock()
	key, ok := authenticator.keys[keyID]
	stale := authenticator.now().Sub(authenticator.fetchedAt) >= jwksMinRefreshInterval
	authenticator.mu.Unlock()
	if ok {
		return key, nil
	}
	if !stale {
		return nil, fmt.Errorf("%w: unknown signing key", ErrUnauthenticated)
	}
	if err := authenticator.refreshKeys(ctx); err != nil {
		return nil, fmt.Errorf("%w: refresh signing keys", ErrUnauthenticated)
	}
	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	key, ok = authenticator.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: unknown signing key", ErrUnauthenticated)
	}
	return key, nil
}

// Authenticate verifies the bearer token: RS256 signature against the realm
// JWKS, exact issuer, expiry, approved audience and the beneficiary realm
// role. Any gap fails closed.
func (authenticator *KeycloakAuthenticator) Authenticate(ctx context.Context, authorizationHeader string) (Principal, error) {
	if !strings.HasPrefix(authorizationHeader, "Bearer ") {
		return Principal{}, ErrUnauthenticated
	}
	tokenText := strings.TrimSpace(strings.TrimPrefix(authorizationHeader, "Bearer "))
	if tokenText == "" {
		return Principal{}, ErrUnauthenticated
	}
	claims := &keycloakClaims{}
	_, err := jwt.ParseWithClaims(tokenText, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %q", token.Method.Alg())
		}
		keyID, _ := token.Header["kid"].(string)
		if keyID == "" {
			return nil, errors.New("token has no key ID")
		}
		return authenticator.keyFor(ctx, keyID)
	},
		jwt.WithIssuer(authenticator.config.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(authenticator.now),
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return Principal{}, fmt.Errorf("%w: token has no subject", ErrUnauthenticated)
	}
	if len(claims.Subject) > 256 {
		return Principal{}, fmt.Errorf("%w: subject is not canonical identifier text", ErrUnauthenticated)
	}
	audienceOK := claims.AuthorizedParty == authenticator.config.Audience
	for _, audience := range claims.Audience {
		if audience == authenticator.config.Audience {
			audienceOK = true
		}
	}
	if !audienceOK {
		return Principal{}, fmt.Errorf("%w: token is not issued for this API audience", ErrUnauthenticated)
	}
	principal := Principal{Subject: claims.Subject, Roles: append([]string(nil), claims.RealmAccess.Roles...)}
	if !principal.hasRole(BeneficiaryRole) {
		return Principal{}, ErrForbidden
	}
	return principal, nil
}

func (principal Principal) hasRole(role string) bool {
	for _, held := range principal.Roles {
		if held == role {
			return true
		}
	}
	return false
}

// principalContextKey carries the verified principal through the request
// context.
type principalContextKey struct{}

// RequireAuth wraps one handler with bearer verification. 401 marks an
// unverifiable token, 403 a verified token without the beneficiary role.
func RequireAuth(authenticator Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, err := authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
		if err != nil {
			if errors.Is(err, ErrForbidden) {
				writeProblem(writer, http.StatusForbidden, "The authenticated identity does not hold the beneficiary role.", nil)
				return
			}
			writeProblem(writer, http.StatusUnauthorized, "A valid bearer token is required.", nil)
			return
		}
		ctx := context.WithValue(request.Context(), principalContextKey{}, principal)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func principalFrom(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalContextKey{}).(Principal)
	return principal
}
