package main

import (
	"errors"
	"flag"
	"fmt"
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

func main() {
	operation := flag.String("operation", "", "real ledger operation: account, reserve, post or void")
	idHex := flag.String("id-hex", "", "primary account/transfer ID")
	debitHex := flag.String("debit-account-id-hex", "", "debit account ID for reserve")
	creditHex := flag.String("credit-account-id-hex", "", "credit account ID for reserve")
	pendingHex := flag.String("pending-transfer-id-hex", "", "pending transfer ID for post or void")
	amount := flag.Uint64("amount", 0, "reserve amount in the configured smallest currency unit")
	timeout := flag.Uint("timeout", 0, "pending transfer timeout in seconds")
	history := flag.Bool("history", true, "enable TigerBeetle account history")
	flag.Parse()

	if strings.TrimSpace(*operation) == "" {
		fail(errors.New("--operation is required"))
	}
	clusterID, err := tigerbeetle.HexStringToUint128(strings.TrimSpace(os.Getenv("TIGERBEETLE_CLUSTER_ID_HEX")))
	if err != nil {
		fail(fmt.Errorf("parse TIGERBEETLE_CLUSTER_ID_HEX: %w", err))
	}
	replicas := strings.Split(strings.TrimSpace(os.Getenv("TIGERBEETLE_REPLICA_ADDRESSES")), ",")
	if len(replicas) == 0 || replicas[0] == "" {
		fail(errors.New("TIGERBEETLE_REPLICA_ADDRESSES must contain approved replica addresses"))
	}
	for i := range replicas {
		replicas[i] = strings.TrimSpace(replicas[i])
		if replicas[i] == "" {
			fail(errors.New("TIGERBEETLE_REPLICA_ADDRESSES contains an empty address"))
		}
	}
	client, err := tigerbeetle.NewClient(clusterID, replicas)
	if err != nil {
		fail(fmt.Errorf("create TigerBeetle client: %w", err))
	}
	defer client.Close()
	ledgerNumber, err := parseUintEnv("TIGERBEETLE_LEDGER")
	if err != nil {
		fail(err)
	}
	code, err := parseUintEnv("TIGERBEETLE_CODE")
	if err != nil {
		fail(err)
	}
	if ledgerNumber > uint64(^uint32(0)) || code > uint64(^uint16(0)) {
		fail(errors.New("TIGERBEETLE_LEDGER or TIGERBEETLE_CODE exceeds its protocol width"))
	}
	service, err := ledger.New(client, uint32(ledgerNumber), uint16(code))
	if err != nil {
		fail(err)
	}
	// One-shot CLI: operations carry client-side TigerBeetle spans (noop when
	// telemetry is disabled) under a background trace.
	ctx := context.Background()

	switch *operation {
	case "account":
		err = service.CreateAccountTraced(ctx, parseID(*idHex), *history)
	case "reserve":
		if uint64(*timeout) > uint64(^uint32(0)) {
			fail(errors.New("--timeout exceeds the TigerBeetle uint32 protocol width"))
		}
		err = service.ReserveTraced(ctx, parseID(*idHex), parseID(*debitHex), parseID(*creditHex), *amount, uint32(*timeout))
	case "post":
		err = service.PostTraced(ctx, parseID(*idHex), parseID(*pendingHex))
	case "void":
		err = service.VoidTraced(ctx, parseID(*idHex), parseID(*pendingHex))
	default:
		fail(fmt.Errorf("unsupported --operation %q", *operation))
	}
	if err != nil {
		fail(err)
	}
	fmt.Printf("completed real TigerBeetle operation %s with id %s\n", *operation, *idHex)
}

func parseID(value string) tigerbeetle.Uint128 {
	id, err := tigerbeetle.HexStringToUint128(strings.TrimSpace(value))
	if err != nil || id == (tigerbeetle.Uint128{}) {
		fail(fmt.Errorf("invalid non-zero Uint128 ID %q: %w", value, err))
	}
	return id
}

func parseUintEnv(name string) (uint64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, fmt.Errorf("%s must be injected by the approved environment configuration", name)
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be a non-zero unsigned integer", name)
	}
	return parsed, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tigerbeetle-ledger:", err)
	os.Exit(1)
}
