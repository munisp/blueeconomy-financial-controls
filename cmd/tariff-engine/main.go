package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/blueeconomy-financial-controls/internal/tariff"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// run loads configuration, applies migrations and serves until interrupted.
func run(logger *slog.Logger) error {
	listenAddr := strings.TrimSpace(os.Getenv("TARIFF_ENGINE_LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	migrationPath := strings.TrimSpace(os.Getenv("TARIFF_MIGRATION_PATH"))
	if migrationPath == "" {
		return errors.New("TARIFF_MIGRATION_PATH is required")
	}
	ctx := context.Background()
	pipeline, err := setupTelemetry(ctx, logger, "tariff-engine")
	if err != nil {
		return err
	}
	defer func() {
		if err := pipeline.Shutdown(context.Background()); err != nil {
			logger.Error("telemetry shutdown failed", "error", err)
		}
	}()
	pool, err := pipeline.NewPGXPool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open postgres pool: %w", err)
	}
	defer pool.Close()
	if err := applyMigration(ctx, pool, migrationPath); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}
	store, err := tariff.NewStore(pool)
	if err != nil {
		return err
	}
	authenticator, err := authenticatorFromEnv()
	if err != nil {
		return err
	}
	handler, err := tariff.NewHandler(authenticator, store, logger)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           pipeline.Middleware("tariff-engine", handler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	logger.Info("tariff engine listening", "addr", listenAddr)
	return server.ListenAndServe()
}

// setupTelemetry builds the OpenTelemetry pipeline from the environment. An
// absent OTEL_EXPORTER_OTLP_ENDPOINT means telemetry is disabled and the
// service boots and serves exactly as before (the one sanctioned fail-open).
func setupTelemetry(ctx context.Context, logger *slog.Logger, serviceName string) (*telemetry.Telemetry, error) {
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
		logger.Info("telemetry enabled", "otlp_endpoint", config.Endpoint)
	} else {
		logger.Info("telemetry disabled (OTEL_EXPORTER_OTLP_ENDPOINT not set)")
	}
	return pipeline, nil
}

// applyMigration executes the migration file, following the repo's
// boot-time migration pattern (idempotent DDL).
func applyMigration(ctx context.Context, pool *pgxpool.Pool, migrationPath string) error {
	statement, err := os.ReadFile(migrationPath)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", migrationPath, err)
	}
	if _, err := pool.Exec(ctx, string(statement)); err != nil {
		return fmt.Errorf("execute migration %s: %w", migrationPath, err)
	}
	return nil
}

// authenticatorFromEnv builds the RS256 Keycloak authenticator; an explicit
// non-production profile may opt out with a dev authenticator, mirroring the
// declaration-scorer contract. Production never serves unauthenticated.
func authenticatorFromEnv() (tariff.Authenticator, error) {
	profile := strings.ToLower(strings.TrimSpace(os.Getenv("TARIFF_ENGINE_ENV")))
	allowInsecure := profile != "" && profile != "production" && profile != "prod"
	if allowInsecure {
		if strings.TrimSpace(os.Getenv("KEYCLOAK_BASE_URL")) == "" {
			return devAuthenticator{}, nil
		}
	}
	baseURL := strings.TrimSpace(os.Getenv("KEYCLOAK_BASE_URL"))
	realm := strings.TrimSpace(os.Getenv("KEYCLOAK_REALM"))
	if baseURL == "" || realm == "" {
		return nil, errors.New("KEYCLOAK_BASE_URL and KEYCLOAK_REALM are required (fail-closed)")
	}
	return tariff.NewKeycloakAuthenticator(baseURL, realm, &http.Client{Timeout: 10 * time.Second})
}

// devAuthenticator accepts any bearer token and uses it as the subject; it
// exists only for explicit non-production profiles (never in prod).
type devAuthenticator struct{}

// Authenticate returns the token text as the subject in dev profiles.
func (devAuthenticator) Authenticate(_ context.Context, authorizationHeader string) (string, error) {
	token := strings.TrimSpace(strings.TrimPrefix(authorizationHeader, "Bearer "))
	if token == "" {
		return "", errors.New("bearer token is required")
	}
	return "dev:" + token, nil
}
