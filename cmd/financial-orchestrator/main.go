package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	"github.com/munisp/blueeconomy-financial-controls/internal/orchestration"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

func main() {
	intentID := flag.String("intent-id", "", "financial intent identifier")
	operation := flag.String("operation", "reserve", "one of: reserve, post, void")
	flag.Parse()
	if strings.TrimSpace(*intentID) == "" {
		fail(errors.New("--intent-id is required"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	databaseURL := required("DATABASE_URL")
	store, err := intent.Open(ctx, databaseURL)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	clusterID, err := tigerbeetle.HexStringToUint128(required("TIGERBEETLE_CLUSTER_ID_HEX"))
	if err != nil {
		fail(fmt.Errorf("parse TigerBeetle cluster ID: %w", err))
	}
	replicas := splitRequired("TIGERBEETLE_REPLICA_ADDRESSES")
	if requiredBool("TIGERBEETLE_REQUIRE_SIX_REPLICAS", false) {
		if err := validateProductionQuorum(clusterID, replicas); err != nil {
			fail(err)
		}
	}
	client, err := tigerbeetle.NewClient(clusterID, replicas)
	if err != nil {
		fail(fmt.Errorf("create TigerBeetle client: %w", err))
	}
	defer client.Close()
	ledgerNumber := uint64Env("TIGERBEETLE_LEDGER")
	code := uint64Env("TIGERBEETLE_CODE")
	if ledgerNumber > uint64(^uint32(0)) || code > uint64(^uint16(0)) {
		fail(errors.New("TigerBeetle ledger or code exceeds protocol width"))
	}
	ledgerService, err := ledger.New(client, uint32(ledgerNumber), uint16(code))
	if err != nil {
		fail(err)
	}
	timeout := uint64Env("TIGERBEETLE_PENDING_TIMEOUT_SECONDS")
	if timeout > uint64(^uint32(0)) {
		fail(errors.New("pending timeout exceeds protocol width"))
	}
	orchestrator, err := orchestration.New(store, ledgerService, uint32(timeout))
	if err != nil {
		fail(err)
	}
	var updated intent.Intent
	switch *operation {
	case "reserve":
		updated, err = orchestrator.ReserveApproved(ctx, *intentID)
	case "post":
		updated, err = orchestrator.PostReserved(ctx, *intentID)
	case "void":
		updated, err = orchestrator.VoidReserved(ctx, *intentID)
	default:
		fail(errors.New("--operation must be one of reserve, post or void"))
	}
	if err != nil {
		fail(err)
	}
	fmt.Printf("intent %s moved to %s at %s\n", updated.IntentID, updated.State, time.Now().UTC().Format(time.RFC3339Nano))
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		fail(fmt.Errorf("%s must be supplied by approved environment configuration", name))
	}
	return value
}

func splitRequired(name string) []string {
	parts := strings.Split(required(name), ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
		if parts[index] == "" {
			fail(fmt.Errorf("%s contains an empty replica address", name))
		}
	}
	return parts
}

func requiredBool(name string, defaultValue bool) bool {
	value, present := os.LookupEnv(name)
	if !present || strings.TrimSpace(value) == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		fail(fmt.Errorf("%s must be a boolean", name))
	}
	return parsed
}

func validateProductionQuorum(clusterID tigerbeetle.Uint128, replicas []string) error {
	if clusterID == (tigerbeetle.Uint128{}) {
		return errors.New("TIGERBEETLE_CLUSTER_ID_HEX must not be the reserved zero test cluster when production quorum is required")
	}
	if len(replicas) != 6 {
		return fmt.Errorf("TIGERBEETLE_REQUIRE_SIX_REPLICAS requires exactly 6 replica addresses, got %d", len(replicas))
	}
	seen := make(map[string]struct{}, len(replicas))
	for _, replica := range replicas {
		host, port, err := net.SplitHostPort(replica)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf("replica address %q must be a non-empty host:port", replica)
		}
		if _, exists := seen[replica]; exists {
			return fmt.Errorf("replica address %q is duplicated", replica)
		}
		seen[replica] = struct{}{}
	}
	return nil
}

func uint64Env(name string) uint64 {
	value := required(name)
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 {
		fail(fmt.Errorf("%s must be a non-zero unsigned integer", name))
	}
	return parsed
}

func fail(err error) {
	log.Printf("financial-orchestrator: %v", err)
	os.Exit(1)
}
