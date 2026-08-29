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
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("cvff-api: %v", err)
	}
}

// setupTelemetry builds the OpenTelemetry pipeline from the environment.
// An absent OTEL_EXPORTER_OTLP_ENDPOINT means telemetry is disabled and the
// service boots and serves exactly as before (the one sanctioned fail-open).
func setupTelemetry(ctx context.Context, serviceName string) (*telemetry.Telemetry, error) {
	config, err := telemetry.LoadConfig(serviceName)
	if err != nil {
		return nil, fmt.Errorf("load telemetry config: %w", err)
	}
	pipeline, err := telemetry.Setup(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("setup telemetry: %w", err)
	}
	telemetry.InstallDefault(pipeline)
	if pipeline.Enabled() {
		log.Printf("%s: telemetry enabled (otlp endpoint %s)", serviceName, config.Endpoint)
	} else {
		log.Printf("%s: telemetry disabled (OTEL_EXPORTER_OTLP_ENDPOINT not set)", serviceName)
	}
	return pipeline, nil
}

func run() error {
	config, err := cvffapi.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pipeline, err := setupTelemetry(ctx, "cvff-api")
	if err != nil {
		return err
	}
	defer func() {
		if err := pipeline.Shutdown(context.Background()); err != nil {
			log.Printf("cvff-api: telemetry shutdown failed: %v", err)
		}
	}()
	temporalInterceptor, err := pipeline.TemporalInterceptor()
	if err != nil {
		return err
	}

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
	temporalClient, err := temporalclient.Dial(temporalclient.Options{
		HostPort:     config.Temporal.HostPort,
		Namespace:    config.Temporal.Namespace,
		Interceptors: []interceptor.ClientInterceptor{temporalInterceptor},
	})
	if err != nil {
		return fmt.Errorf("dial Temporal: %w", err)
	}
	defer temporalClient.Close()
	starter, err := cvffapi.NewTemporalStarter(temporalClient, config.Temporal.TaskQueue)
	if err != nil {
		return err
	}
	signaler, err := cvffapi.NewTemporalSignaler(temporalClient, config.Temporal.TaskQueue)
	if err != nil {
		return err
	}
	policy, err := pbac.LoadEnforcer(config.PolicyDir)
	if err != nil {
		return fmt.Errorf("authorization policy: %w", err)
	}
	handler, err := cvffapi.NewHandler(store, authenticator, blobs, scanner, config.Limits, starter, signaler, policy)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: config.ListenAddr, Handler: pipeline.Middleware("cvff-api", handler), ReadHeaderTimeout: 10 * time.Second}
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
