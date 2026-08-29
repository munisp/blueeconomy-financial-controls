// declaration-scorer serves POST /v1/risk-scores, the risk-scoring provider
// behind DECLARATIONS_SCORER_URL consumed by blueeconomy-port-interoperability.
// Scoring is deterministic rules-based evaluation of versioned config data
// (amount bands, HS-prefix risk, country risk, new-trader flags); every
// response carries model_version and rule_based:true. The process fails
// closed without a listen address and a valid rules file.
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

	"github.com/munisp/blueeconomy-financial-controls/internal/riskscore"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("declaration-scorer: %v", err)
	}
}

func run() error {
	listenAddr := required("DECLARATION_SCORER_LISTEN_ADDR")
	rulesPath := required("DECLARATION_SCORER_RULES_PATH")
	rules, err := riskscore.LoadRules(rulesPath)
	if err != nil {
		return fmt.Errorf("rules: %w", err)
	}
	handler, err := riskscore.NewHandler(rules)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: listenAddr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("declaration-scorer: listening on %s (model %s, rule-based)", listenAddr, rules.ModelVersion)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve declaration scorer: %w", err)
	}
	return nil
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("declaration-scorer: %s is required", name)
	}
	return value
}
