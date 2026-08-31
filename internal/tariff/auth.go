package tariff

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Authenticator verifies an Authorization header and returns the verified
// token subject (maker/checker needs the authenticated identity, which the
// riskscore Authenticator contract does not expose — this is the documented
// reason this package mirrors, rather than reuses, that contract).
type Authenticator interface {
	Authenticate(ctx context.Context, authorizationHeader string) (subject string, err error)
}

type keycloakJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type keycloakJWKS struct {
	Keys []keycloakJWK `json:"keys"`
}

// KeycloakAuthenticator verifies RS256 bearer tokens against a realm's JWKS
// endpoint. Construction and behavior are fail-closed and identical to the
// repo's riskscore authenticator, plus verified-subject extraction.
type KeycloakAuthenticator struct {
	baseURL    string
	realm      string
	httpClient *http.Client

	mu      sync.Mutex
	keyOnce time.Time
	keys    map[string]*rsa.PublicKey
}

// NewKeycloakAuthenticator fails closed on missing configuration.
func NewKeycloakAuthenticator(baseURL, realm string, httpClient *http.Client) (*KeycloakAuthenticator, error) {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(realm) == "" {
		return nil, errors.New("keycloak base URL and realm are required")
	}
	if httpClient == nil {
		return nil, errors.New("keycloak http client is required")
	}
	return &KeycloakAuthenticator{
		baseURL:    strings.TrimRight(baseURL, "/"),
		realm:      realm,
		httpClient: httpClient,
		keys:       make(map[string]*rsa.PublicKey),
	}, nil
}

// Authenticate verifies the bearer token and returns its subject.
func (authenticator *KeycloakAuthenticator) Authenticate(ctx context.Context, authorizationHeader string) (string, error) {
	if !strings.HasPrefix(authorizationHeader, "Bearer ") {
		return "", errors.New("authorization header must be a bearer token")
	}
	tokenText := strings.TrimSpace(strings.TrimPrefix(authorizationHeader, "Bearer "))
	if tokenText == "" {
		return "", errors.New("bearer token is required")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenText, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok || token.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %v", token.Header["alg"])
		}
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token key id is required")
		}
		publicKey, err := authenticator.publicKey(ctx, kid)
		if err != nil {
			return nil, err
		}
		return publicKey, nil
	},
		jwt.WithIssuer(fmt.Sprintf("%s/realms/%s", authenticator.baseURL, authenticator.realm)),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !token.Valid {
		return "", errors.New("bearer token is invalid")
	}
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return "", errors.New("bearer token subject is required")
	}
	return subject, nil
}

func (authenticator *KeycloakAuthenticator) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	if key, ok := authenticator.keys[kid]; ok && time.Since(authenticator.keyOnce) < 5*time.Minute {
		return key, nil
	}
	if err := authenticator.refreshKeys(ctx); err != nil {
		return nil, err
	}
	key, ok := authenticator.keys[kid]
	if !ok {
		return nil, fmt.Errorf("key id %q is not published by the realm", kid)
	}
	return key, nil
}

func (authenticator *KeycloakAuthenticator) refreshKeys(ctx context.Context) error {
	endpoint := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/certs", authenticator.baseURL, authenticator.realm)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build jwks request: %w", err)
	}
	response, err := authenticator.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks returned %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read jwks: %w", err)
	}
	var document keycloakJWKS
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	if len(document.Keys) == 0 {
		return errors.New("realm published no signing keys")
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, jwk := range document.Keys {
		if jwk.Kty != "RSA" || jwk.Kid == "" {
			continue
		}
		modulusBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
		if err != nil {
			return fmt.Errorf("decode jwks modulus: %w", err)
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
		if err != nil {
			return fmt.Errorf("decode jwks exponent: %w", err)
		}
		var exponent int
		for _, b := range exponentBytes {
			exponent = exponent<<8 + int(b)
		}
		if exponent < 3 {
			return errors.New("jwks exponent is too small")
		}
		modulus := new(big.Int).SetBytes(modulusBytes)
		if modulus.Sign() <= 0 {
			return errors.New("jwks modulus is invalid")
		}
		keys[jwk.Kid] = &rsa.PublicKey{N: modulus, E: exponent}
	}
	if len(keys) == 0 {
		return errors.New("realm published no usable RSA signing keys")
	}
	authenticator.keys = keys
	authenticator.keyOnce = time.Now()
	return nil
}
