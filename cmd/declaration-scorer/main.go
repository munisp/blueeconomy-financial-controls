// declaration-scorer serves POST /v1/risk-scores, the risk-scoring provider
// behind DECLARATIONS_SCORER_URL consumed by blueeconomy-port-interoperability,
// and — when DECLARATION_SCORER_GRPC_LISTEN_ADDR is set — the Phase-7 gRPC
// contract blueeconomy.riskscore.v1.RiskScoreService over the same scoring
// core and the same Keycloak RS256 authentication.
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
	"net"
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	authenticator, err := authenticatorFromEnv(ctx)
	if err != nil {
		return err
	}
	handler, err := riskscore.NewHandler(rules, authenticator)
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
	// Phase-7 (PRA-066..068): the gRPC RiskScoreService is served alongside
	// HTTP when DECLARATION_SCORER_GRPC_LISTEN_ADDR is set, over the same
	// scoring core and the same Keycloak RS256 authenticator (every RPC
	// except the public health/reflection endpoints requires a verified
	// token; the production profile still refuses to boot without the
	// Keycloak triple above).
	grpcListenAddr := strings.TrimSpace(os.Getenv("DECLARATION_SCORER_GRPC_LISTEN_ADDR"))
	if grpcListenAddr != "" {
		grpcServer, err := riskscore.NewGRPCServer(rules, authenticator)
		if err != nil {
			return fmt.Errorf("configure gRPC risk-scoring server: %w", err)
		}
		grpcListener, err := net.Listen("tcp", grpcListenAddr)
		if err != nil {
			return fmt.Errorf("bind gRPC listen address: %w", err)
		}
		go func() {
			<-ctx.Done()
			grpcServer.GracefulStop()
		}()
		go func() {
			log.Printf("declaration-scorer: gRPC listening on %s (model %s, rule-based)", grpcListenAddr, rules.ModelVersion)
			if err := grpcServer.Serve(grpcListener); err != nil {
				log.Printf("declaration-scorer: gRPC server stopped: %v", err)
			}
		}()
	}
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

// authenticatorFromEnv wires the Keycloak RS256 verifier. The environment
// contract is fail-closed: in the production profile (DECLARATION_SCORER_ENV
// unset or "production") KEYCLOAK_JWKS_URL, KEYCLOAK_ISSUER and
// KEYCLOAK_EXPECTED_AUDIENCE are all mandatory and the realm JWKS must be
// reachable at boot. A non-production profile may run without authentication
// only by explicitly opting out, and any partially configured Keycloak
// coordinate set is a boot error everywhere.
func authenticatorFromEnv(ctx context.Context) (riskscore.Authenticator, error) {
	profile := strings.ToLower(strings.TrimSpace(os.Getenv("DECLARATION_SCORER_ENV")))
	production := profile == "" || profile == "production"
	jwksURL := strings.TrimSpace(os.Getenv("KEYCLOAK_JWKS_URL"))
	issuer := strings.TrimSpace(os.Getenv("KEYCLOAK_ISSUER"))
	audience := strings.TrimSpace(os.Getenv("KEYCLOAK_EXPECTED_AUDIENCE"))
	configured := 0
	for _, value := range []string{jwksURL, issuer, audience} {
		if value != "" {
			configured++
		}
	}
	if configured > 0 && configured < 3 {
		return nil, fmt.Errorf("KEYCLOAK_JWKS_URL, KEYCLOAK_ISSUER and KEYCLOAK_EXPECTED_AUDIENCE must be configured together (got %d of 3)", configured)
	}
	if configured == 0 {
		if production {
			return nil, fmt.Errorf("production profile requires KEYCLOAK_JWKS_URL, KEYCLOAK_ISSUER and KEYCLOAK_EXPECTED_AUDIENCE")
		}
		log.Printf("declaration-scorer: WARNING — non-production profile %q running WITHOUT request authentication; never expose this posture", profile)
		return allowAllAuthenticator{}, nil
	}
	authenticator, err := riskscore.NewKeycloakAuthenticator(ctx, riskscore.KeycloakConfig{Issuer: issuer, JWKSURL: jwksURL, Audience: audience})
	if err != nil {
		return nil, fmt.Errorf("keycloak authenticator: %w", err)
	}
	return authenticator, nil
}

// allowAllAuthenticator is reachable only on an explicitly non-production
// profile with no Keycloak coordinates configured (see authenticatorFromEnv).
type allowAllAuthenticator struct{}

func (allowAllAuthenticator) Authenticate(context.Context, string) error { return nil }
