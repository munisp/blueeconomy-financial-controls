// intent-api serves the openapi.yaml financial-intent contract backed by the
// durable PostgreSQL store. Every money route is gated by Keycloak bearer
// verification, a realm-role binding and the embedded PBAC policy layer;
// actor identity (maker/checker) is derived from verified token claims only.
// The process fails closed without DATABASE_URL, a listen address, the
// Keycloak realm coordinates or a loadable policy directory.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("intent-api: %v", err)
	}
}

func run() error {
	databaseURL := required("DATABASE_URL")
	listenAddr := required("INTENT_API_LISTEN_ADDR")
	keycloak := cvffapi.KeycloakConfig{
		Issuer:   required("INTENT_API_KEYCLOAK_ISSUER"),
		JWKSURL:  required("INTENT_API_KEYCLOAK_JWKS_URL"),
		Audience: required("INTENT_API_JWT_AUDIENCE"),
	}
	policyDir := required("INTENT_API_POLICY_DIR")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := intent.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	authenticator, err := cvffapi.NewKeycloakAuthenticator(ctx, keycloak)
	if err != nil {
		return fmt.Errorf("keycloak authenticator: %w", err)
	}
	policy, err := pbac.LoadEnforcer(policyDir)
	if err != nil {
		return fmt.Errorf("authorization policy: %w", err)
	}
	handler, err := intent.NewHandler(store, authenticator, policy)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: listenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("intent-api: listening on %s", listenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve intent API: %w", err)
	}
	return nil
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("intent-api: %s is required", name)
	}
	return value
}
