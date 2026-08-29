// intent-api serves the openapi.yaml financial-intent contract backed by the
// durable PostgreSQL store. Every money route is gated by Keycloak bearer
// verification, a realm-role binding and the embedded PBAC policy layer;
// actor identity (maker/checker) is derived from verified token claims only.
// The process fails closed without DATABASE_URL, a listen address, the
// Keycloak realm coordinates or a loadable policy directory.
package main

import (
	"context"
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
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	"github.com/munisp/blueeconomy-financial-controls/internal/orchestration"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("intent-api: %v", err)
	}
}

func run() error {
	databaseURL := required("DATABASE_URL")
	listenAddr := required("INTENT_API_LISTEN_ADDR")
	keycloak := cvffapi.KeycloakConfig{
		Issuer:   required("INTENT_API_KEYCLOAK_ISSUER"),
		JWKSURL:  required("INTENT_API_KEYCLOAK_JWKS_URL"),
		Audience: required("INTENT_API_JWT_AUDIENCE"),
	}
	policyDir := required("INTENT_API_POLICY_DIR")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, err := intent.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	authenticator, err := cvffapi.NewKeycloakAuthenticator(ctx, keycloak)
	if err != nil {
		return fmt.Errorf("keycloak authenticator: %w", err)
	}
	policy, err := pbac.LoadEnforcer(policyDir)
	if err != nil {
		return fmt.Errorf("authorization policy: %w", err)
	}
	// The officer-resolution route needs the TigerBeetle ledger: a VOID
	// disposition must compensate any outstanding reservation, never edit
	// state alone.
	resolver, err := newResolver(ctx, store)
	if err != nil {
		return err
	}
	handler, err := intent.NewHandler(store, authenticator, policy, resolver)
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

// newResolver builds the TigerBeetle-backed officer resolver. Every
// coordinate is required; the process refuses to serve money routes without
// the ledger the VOID disposition compensates against.
func newResolver(ctx context.Context, store *intent.Store) (*orchestration.Resolver, error) {
	clusterID, err := tigerbeetle.HexStringToUint128(required("TIGERBEETLE_CLUSTER_ID_HEX"))
	if err != nil {
		return nil, fmt.Errorf("parse TIGERBEETLE_CLUSTER_ID_HEX: %w", err)
	}
	replicas := strings.Split(required("TIGERBEETLE_REPLICA_ADDRESSES"), ",")
	for index := range replicas {
		replicas[index] = strings.TrimSpace(replicas[index])
		if replicas[index] == "" {
			return nil, errors.New("TIGERBEETLE_REPLICA_ADDRESSES contains an empty address")
		}
	}
	client, err := tigerbeetle.NewClient(clusterID, replicas)
	if err != nil {
		return nil, fmt.Errorf("create TigerBeetle client: %w", err)
	}
	go func() {
		<-ctx.Done()
		client.Close()
	}()
	ledgerNumber, err := requiredUint("TIGERBEETLE_LEDGER")
	if err != nil {
		return nil, err
	}
	code, err := requiredUint("TIGERBEETLE_CODE")
	if err != nil {
		return nil, err
	}
	if ledgerNumber > uint64(^uint32(0)) || code > uint64(^uint16(0)) {
		return nil, errors.New("TIGERBEETLE_LEDGER or TIGERBEETLE_CODE exceeds protocol width")
	}
	ledgerService, err := ledger.New(client, uint32(ledgerNumber), uint16(code))
	if err != nil {
		return nil, err
	}
	resolver, err := orchestration.NewResolver(store, ledgerService)
	if err != nil {
		return nil, err
	}
	return resolver, nil
}

func requiredUint(name string) (uint64, error) {
	value := required(name)
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an unsigned integer", name)
	}
	return parsed, nil
}
