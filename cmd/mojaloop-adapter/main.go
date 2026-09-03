package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/mojaloop"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mojaloop-adapter: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	config, err := mojaloop.LoadConfig()
	if err != nil {
		return err
	}
	// The operating mode is explicit and mandatory (config fails closed on
	// anything else). `receive-only` serves only the inbound signed callback
	// surface; `full` additionally serves the outbound leg — POST /payouts
	// (bearer/PBAC-gated) -> POST /quotes -> signed PUT /quotes/{id} callback
	// -> POST /transfers -> signed PUT /transfers/{id} callback — and
	// requires the payout authorization and outbound-migration coordinates.
	log.Printf("mojaloop-adapter: MOJALOOP_MODE=%s — inbound signed callbacks at %s{id} and %s{id}", config.Mode, config.CallbackTransferPathPrefix, config.CallbackQuotePathPrefix)
	privateKeyBytes, err := os.ReadFile(config.SigningKeyFile)
	if err != nil {
		return fmt.Errorf("read signing key: %w", err)
	}
	privateKey, err := parseRSAPrivateKey(privateKeyBytes)
	if err != nil {
		return err
	}
	caBundle, err := os.ReadFile(config.CABundleFile)
	if err != nil {
		return fmt.Errorf("read CA bundle: %w", err)
	}
	if err := mojaloop.ValidateCABundle(caBundle); err != nil {
		return err
	}
	verificationKey := &privateKey.PublicKey
	if config.VerificationKeyFile != "" {
		verificationKeyBytes, readErr := os.ReadFile(config.VerificationKeyFile)
		if readErr != nil {
			return fmt.Errorf("read callback verification key: %w", readErr)
		}
		verificationKey, readErr = parseRSAPublicKey(verificationKeyBytes)
		if readErr != nil {
			return readErr
		}
	}
	pipeline, err := setupTelemetry(context.Background(), "mojaloop-adapter")
	if err != nil {
		return err
	}
	defer func() {
		if err := pipeline.Shutdown(context.Background()); err != nil {
			log.Printf("mojaloop-adapter: telemetry shutdown failed: %v", err)
		}
	}()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	store, err := intent.Open(context.Background(), databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	migrationPath := os.Getenv("MOJALOOP_MIGRATION_PATH")
	if migrationPath == "" {
		return errors.New("MOJALOOP_MIGRATION_PATH is required")
	}
	migration, err := os.ReadFile(migrationPath)
	if err != nil {
		return fmt.Errorf("read Mojaloop migration: %w", err)
	}
	if err := store.Exec(context.Background(), string(migration)); err != nil {
		return fmt.Errorf("apply Mojaloop migration: %w", err)
	}
	address := os.Getenv("MOJALOOP_LISTEN_ADDR")
	if address == "" {
		return errors.New("MOJALOOP_LISTEN_ADDR is required")
	}
	certificateFile := os.Getenv("MOJALOOP_TLS_CERT_FILE")
	privateCertificateKeyFile := os.Getenv("MOJALOOP_TLS_KEY_FILE")
	if certificateFile == "" || privateCertificateKeyFile == "" {
		return errors.New("MOJALOOP_TLS_CERT_FILE and MOJALOOP_TLS_KEY_FILE are required")
	}
	callbackStore := mojaloop.NewCallbackStore(store.Pool())
	outboundStore := mojaloop.NewOutboundStore(store.Pool())
	var payoutAuthenticator *cvffapi.KeycloakAuthenticator
	var payoutPolicy *pbac.Enforcer
	if config.Mode == mojaloop.ModeFull {
		// Full mode coordinates (fail-closed): the outbound migration, the
		// Keycloak realm that authorizes payout callers and the PBAC policy
		// pack. Any gap refuses the boot rather than running ungated.
		outboundMigrationPath := os.Getenv("MOJALOOP_OUTBOUND_MIGRATION_PATH")
		if outboundMigrationPath == "" {
			return errors.New("MOJALOOP_OUTBOUND_MIGRATION_PATH is required in full mode")
		}
		outboundMigration, err := os.ReadFile(outboundMigrationPath)
		if err != nil {
			return fmt.Errorf("read outbound Mojaloop migration: %w", err)
		}
		if err := store.Exec(context.Background(), string(outboundMigration)); err != nil {
			return fmt.Errorf("apply outbound Mojaloop migration: %w", err)
		}
		payoutAuthenticator, err = cvffapi.NewKeycloakAuthenticator(context.Background(), cvffapi.KeycloakConfig{
			Issuer:   os.Getenv("MOJALOOP_PAYOUT_ISSUER"),
			JWKSURL:  os.Getenv("MOJALOOP_PAYOUT_JWKS_URL"),
			Audience: os.Getenv("MOJALOOP_PAYOUT_AUDIENCE"),
		})
		if err != nil {
			return fmt.Errorf("payout authenticator: %w", err)
		}
		payoutPolicy, err = pbac.LoadEnforcer(os.Getenv("MOJALOOP_PBAC_POLICY_DIR"))
		if err != nil {
			return fmt.Errorf("payout policy: %w", err)
		}
	}
	// FC-3: a RESERVED callback locks funds; without a timeout sweep it
	// strands forever. The TTL is required (fail-closed) and the sweep marks
	// expired reservations timed out with an audit event (local operational
	// evidence; the Hub-signed terminal callback stays the state truth).
	reservedTimeoutSeconds := os.Getenv("MOJALOOP_RESERVED_TIMEOUT_SECONDS")
	reservedTimeout, err := strconv.Atoi(strings.TrimSpace(reservedTimeoutSeconds))
	if err != nil || reservedTimeout <= 0 {
		return errors.New("MOJALOOP_RESERVED_TIMEOUT_SECONDS must be a positive integer")
	}
	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go runReservedTimeoutSweep(sweepCtx, callbackStore, time.Duration(reservedTimeout)*time.Second)
	outboundClient, err := mojaloop.NewClient(config, privateKey)
	if err != nil {
		return err
	}
	handler := mojaloop.CallbackHandler{Store: callbackStore, VerificationKey: verificationKey, ExpectedSource: config.Source, ExpectedDestination: config.Destination, ExpectedVerificationKeyID: config.VerificationKeyID, ExpectedTransferPathPrefix: config.CallbackTransferPathPrefix}
	quoteHandler := mojaloop.QuoteCallbackHandler{Store: outboundStore, PayerFSP: config.Source, PayeeFSP: config.Destination, VerificationKey: verificationKey, ExpectedSource: config.Source, ExpectedDestination: config.Destination, ExpectedVerificationKeyID: config.VerificationKeyID, ExpectedQuotePathPrefix: config.CallbackQuotePathPrefix}
	if config.Mode == mojaloop.ModeFull {
		handler.Outbound = outboundStore
		quoteHandler.Client = &outboundClient
	}
	mux := http.NewServeMux()
	mux.Handle(config.CallbackTransferPathPrefix, handler)
	mux.Handle(config.CallbackQuotePathPrefix, quoteHandler)
	if config.Mode == mojaloop.ModeFull {
		mux.Handle("/payouts", mojaloop.PayoutHandler{Store: outboundStore, Client: &outboundClient, PayerFSP: config.Source, PayeeFSP: config.Destination, Authenticator: payoutAuthenticator, Policy: payoutPolicy})
	}
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, _ *http.Request) {
		// The health payload reports the configured rail posture so operators
		// can observe the receive-only mode, not just process liveness.
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(response).Encode(struct {
			Status string `json:"status"`
			Mode   string `json:"mode"`
		}{Status: "ok", Mode: config.Mode})
	})
	server := &http.Server{Addr: address, Handler: pipeline.Middleware("mojaloop-adapter", mux), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: config.RequestTimeout, WriteTimeout: config.RequestTimeout, IdleTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()
	if err := server.ListenAndServeTLS(certificateFile, privateCertificateKeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// reservedSweepInterval bounds the sweep cadence to a quarter of the TTL,
// clamped to [10s, 5m].
func reservedSweepInterval(ttl time.Duration) time.Duration {
	interval := ttl / 4
	if interval < 10*time.Second {
		return 10 * time.Second
	}
	if interval > 5*time.Minute {
		return 5 * time.Minute
	}
	return interval
}

func runReservedTimeoutSweep(ctx context.Context, store *mojaloop.CallbackStore, ttl time.Duration) {
	sweep := func() {
		expired, err := store.SweepReservedTimeouts(ctx, ttl, time.Now())
		if err != nil {
			log.Printf("mojaloop-adapter: reserved-timeout sweep failed: %v", err)
			return
		}
		if expired > 0 {
			log.Printf("mojaloop-adapter: marked %d RESERVED transfers timed out (ttl %s)", expired, ttl)
		}
	}
	sweep()
	ticker := time.NewTicker(reservedSweepInterval(ttl))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func parseRSAPublicKey(data []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("callback verification key is not PEM encoded")
	}
	if certificate, err := x509.ParseCertificate(block.Bytes); err == nil {
		key, ok := certificate.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("callback verification certificate is not RSA")
		}
		return key, nil
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse callback RSA verification key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("callback verification key is not RSA")
	}
	return key, nil
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

func parseRSAPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("signing key is not PEM encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA signing key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("signing key is not RSA")
	}
	return key, nil
}
