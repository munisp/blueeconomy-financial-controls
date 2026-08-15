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
	handler := mojaloop.CallbackHandler{Store: callbackStore, VerificationKey: &privateKey.PublicKey, ExpectedSource: config.Source, ExpectedDestination: config.Destination}
	mux := http.NewServeMux()
	mux.Handle("/transfers/", handler)
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) })
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: config.RequestTimeout, WriteTimeout: config.RequestTimeout, IdleTimeout: 30 * time.Second}
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
