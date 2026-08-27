package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/mojaloop"
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
	telemetryConfig, err := telemetry.LoadConfig("mojaloop-adapter")
	if err != nil {
		return err
	}
	setupContext, setupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pipeline, err := telemetry.Setup(setupContext, telemetryConfig)
	setupCancel()
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = pipeline.Shutdown(shutdownContext)
	}()
	if pipeline.Enabled() {
		fmt.Printf("mojaloop-adapter: telemetry traces exporting to %s; Prometheus metrics on GET /metrics\n", telemetryConfig.Endpoint)
	} else {
		fmt.Println("mojaloop-adapter: telemetry tracing disabled (OTEL_EXPORTER_OTLP_ENDPOINT not set); explicit no-op tracer, Prometheus metrics on GET /metrics")
	}
	callbackStore := mojaloop.NewCallbackStore(store.Pool())
	handler := mojaloop.CallbackHandler{Store: callbackStore, VerificationKey: verificationKey, ExpectedSource: config.Source, ExpectedDestination: config.Destination, ExpectedVerificationKeyID: config.VerificationKeyID, ExpectedTransferPathPrefix: config.CallbackTransferPathPrefix}
	mux := http.NewServeMux()
	mux.Handle(config.CallbackTransferPathPrefix, handler)
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /readyz", func(response http.ResponseWriter, request *http.Request) {
		probeContext, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		if err := store.Ping(probeContext); err != nil {
			http.Error(response, "store is not reachable", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET /metrics", pipeline.MetricsHandler())
	server := &http.Server{Addr: address, Handler: pipeline.Middleware(mux), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: config.RequestTimeout, WriteTimeout: config.RequestTimeout, IdleTimeout: 30 * time.Second}
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
