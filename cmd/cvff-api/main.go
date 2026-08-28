// cvff-api serves the beneficiary-facing CVFF HTTP API: own applications,
// the immutable decision timeline and supporting-document uploads. Every
// dependency is env-configured and the process fails closed when any
// required value is absent; there is no in-memory or local-disk fallback.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("cvff-api: %v", err)
	}
}

func run() error {
	config, err := cvffapi.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := cvff.Open(ctx, config.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	authenticator, err := cvffapi.NewKeycloakAuthenticator(ctx, config.Keycloak)
	if err != nil {
		return fmt.Errorf("keycloak authenticator: %w", err)
	}
	blobs, err := cvffapi.NewBlobStore(ctx, os.Getenv)
	if err != nil {
		return fmt.Errorf("object storage: %w", err)
	}
	scanner, err := cvffapi.NewHTTPScanner(config.AVScanURL)
	if err != nil {
		return err
	}
	handler, err := cvffapi.NewHandler(store, authenticator, blobs, scanner, config.Limits)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: config.ListenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("cvff-api: listening on %s (storage backend %s)", config.ListenAddr, blobs.Backend())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve cvff API: %w", err)
	}
	return nil
}
