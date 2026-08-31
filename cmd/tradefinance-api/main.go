// tradefinance-api is the WP-6 trade-finance rail service: trader/operations
// consent + application API and the bank-facing data-sharing API. Every
// coordinate is env-driven and fail-closed: no signing key, no bank
// registry, no Keycloak realm — no service.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
	"github.com/munisp/blueeconomy-financial-controls/internal/outbox"
	"github.com/munisp/blueeconomy-financial-controls/internal/tradefinance"
)

const (
	envListenAddr     = "TRADEFINANCE_API_LISTEN_ADDR"
	envDatabaseURL    = "DATABASE_URL"
	envKeycloakIssuer = "TRADEFINANCE_API_KEYCLOAK_ISSUER"
	envKeycloakJWKS   = "TRADEFINANCE_API_KEYCLOAK_JWKS_URL"
	envJWTAudience    = "TRADEFINANCE_API_JWT_AUDIENCE"
)

func requiredEnv(lookup func(string) string, name string) (string, error) {
	value := strings.TrimSpace(lookup(name))
	if value == "" {
		return "", errors.New(name + " is required")
	}
	return value, nil
}

// principalAuthenticator adapts the Keycloak authenticator to the
// tradefinance Authenticator and BearerAuthenticator interfaces.
type principalAuthenticator struct {
	inner *cvffapi.KeycloakAuthenticator
}

func (adapter principalAuthenticator) Authenticate(ctx context.Context, authorizationHeader string) (tradefinance.Principal, error) {
	principal, err := adapter.inner.Authenticate(ctx, authorizationHeader)
	if err != nil {
		return tradefinance.Principal{}, err
	}
	return tradefinance.Principal{Subject: principal.Subject, Roles: principal.Roles}, nil
}

func (adapter principalAuthenticator) AuthenticateSubject(ctx context.Context, authorizationHeader string) (string, error) {
	principal, err := adapter.inner.Authenticate(ctx, authorizationHeader)
	if err != nil {
		return "", err
	}
	return principal.Subject, nil
}

func main() {
	if err := run(context.Background(), os.Getenv); err != nil {
		log.Fatalf("tradefinance-api: %v", err)
	}
}

func run(ctx context.Context, lookup func(string) string) error {
	listenAddr, err := requiredEnv(lookup, envListenAddr)
	if err != nil {
		return err
	}
	databaseURL, err := requiredEnv(lookup, envDatabaseURL)
	if err != nil {
		return err
	}
	// Consent/dataset envelope sealing key, env-only, fail-closed.
	material, err := requiredEnv(lookup, outbox.EnvSigningPrivateKey)
	if err != nil {
		return err
	}
	epoch, err := requiredEnv(lookup, outbox.EnvSigningKeyEpoch)
	if err != nil {
		return err
	}
	privateKey, err := outbox.ParseEnvelopeSigningKey(material)
	if err != nil {
		return err
	}
	signer, err := envelope.NewSigner(outbox.ProducerName+"-"+epoch, privateKey.Seed())
	if err != nil {
		return err
	}
	registry, err := tradefinance.NewBankRegistry(lookup(tradefinance.EnvBankRegistry))
	if err != nil {
		return err
	}
	keycloak, err := cvffapi.NewKeycloakAuthenticator(ctx, cvffapi.KeycloakConfig{
		Issuer:   strings.TrimSpace(lookup(envKeycloakIssuer)),
		JWKSURL:  strings.TrimSpace(lookup(envKeycloakJWKS)),
		Audience: strings.TrimSpace(lookup(envJWTAudience)),
	})
	if err != nil {
		return err
	}
	store, err := tradefinance.Open(ctx, databaseURL, signer)
	if err != nil {
		return err
	}
	defer store.Close()
	authenticator := principalAuthenticator{inner: keycloak}
	traderAPI, err := tradefinance.NewHTTPAPI(store, authenticator)
	if err != nil {
		return err
	}
	bankAPI, err := tradefinance.NewBankAPI(store, registry, authenticator, signer)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/tradefinance/consents", traderAPI.Mux())
	mux.Handle("/v1/tradefinance/consents/", traderAPI.Mux())
	mux.Handle("/v1/tradefinance/applications", traderAPI.Mux())
	mux.Handle("/v1/tradefinance/applications/", traderAPI.Mux())
	mux.Handle("/v1/tradefinance/bank/", bankAPI.Mux())
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("tradefinance-api listening on %s (signer kid %s)", listenAddr, signer.KeyID())
	return server.ListenAndServe()
}
