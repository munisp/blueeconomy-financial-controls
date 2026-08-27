// intent-api serves the openapi.yaml financial-intent contract backed by the
// durable PostgreSQL store. It fails closed without DATABASE_URL and a listen
// address; the platform ingress terminates mTLS/OAuth (external manifest
// inputs per the openapi description).
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

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("intent-api: %v", err)
	}
}

func run() error {
	databaseURL := required("DATABASE_URL")
	listenAddr := required("INTENT_API_LISTEN_ADDR")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := intent.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	handler, err := intent.NewHandler(store)
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
