package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
	"github.com/munisp/blueeconomy-financial-controls/internal/revenue"
	"github.com/munisp/blueeconomy-financial-controls/internal/tariff"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// revenue-assurance serves the W-FEAT-7 chain: debit-note issuance, TSA
// remittance split, three-way reconciliation and the reporting primitives.
// Configuration is env-only; the service fails closed on missing secrets
// (signing key) or missing auth configuration in production profiles.
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	listenAddr := strings.TrimSpace(os.Getenv("REVENUE_ASSURANCE_LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = ":8082"
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	migrationPath := strings.TrimSpace(os.Getenv("MIGRATION_PATH"))
	if migrationPath == "" {
		return errors.New("MIGRATION_PATH is required")
	}
	ctx := context.Background()
	pipeline, err := setupTelemetry(ctx, logger, "revenue-assurance")
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
	signer, err := signerFromEnv()
	if err != nil {
		return err
	}
	store, err := revenue.NewStore(pool, signer)
	if err != nil {
		return err
	}
	verifier, err := verifierFromEnv()
	if err != nil {
		return err
	}
	authenticator, err := authenticatorFromEnv()
	if err != nil {
		return err
	}
	handler, err := revenue.NewHandler(authenticator, store, verifier, logger)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           pipeline.Middleware("revenue-assurance", handler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	logger.Info("revenue assurance listening", "addr", listenAddr)
	return server.ListenAndServe()
}

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

// signerFromEnv loads the envelope signing key (env-only secret):
// REVENUE_ENVELOPE_SIGNING_KEY is the base64url 32-byte Ed25519 seed and
// REVENUE_ENVELOPE_KEY_ID its key id. Missing or malformed is fatal.
func signerFromEnv() (*envelope.Signer, error) {
	encoded := strings.TrimSpace(os.Getenv("REVENUE_ENVELOPE_SIGNING_KEY"))
	kid := strings.TrimSpace(os.Getenv("REVENUE_ENVELOPE_KEY_ID"))
	if encoded == "" || kid == "" {
		return nil, errors.New("REVENUE_ENVELOPE_SIGNING_KEY and REVENUE_ENVELOPE_KEY_ID are required (fail-closed)")
	}
	seed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("REVENUE_ENVELOPE_SIGNING_KEY must be a base64url 32-byte Ed25519 seed")
	}
	return envelope.NewSigner(kid, seed)
}

// verifierFromEnv loads the trusted statement-ingest keys:
// REVENUE_INGEST_TRUSTED_KEYS = "kid1=base64url-pub,kid2=base64url-pub".
// Unset means statement ingest is disabled (the endpoint refuses) — never a
// default trust set.
func verifierFromEnv() (*envelope.Verifier, error) {
	configured := strings.TrimSpace(os.Getenv("REVENUE_INGEST_TRUSTED_KEYS"))
	if configured == "" {
		return nil, nil // ingest disabled, fail-closed at the endpoint
	}
	trusted := map[string]string{}
	for _, pair := range strings.Split(configured, ",") {
		kid, encoded, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || kid == "" || encoded == "" {
			return nil, errors.New("REVENUE_INGEST_TRUSTED_KEYS entries must be kid=base64url-pub")
		}
		trusted[kid] = encoded
	}
	return envelope.NewVerifier(trusted)
}

// authenticatorFromEnv mirrors the tariff-engine contract: RS256 Keycloak in
// every profile, with an explicit non-production opt-out for local dev.
func authenticatorFromEnv() (tariff.Authenticator, error) {
	profile := strings.ToLower(strings.TrimSpace(os.Getenv("REVENUE_ASSURANCE_ENV")))
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

type devAuthenticator struct{}

func (devAuthenticator) Authenticate(_ context.Context, authorizationHeader string) (string, error) {
	token := strings.TrimSpace(strings.TrimPrefix(authorizationHeader, "Bearer "))
	if token == "" {
		return "", errors.New("bearer token is required")
	}
	return "dev:" + token, nil
}
