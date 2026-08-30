// settlement-sync mirrors posted TigerBeetle collection transfers into
// settlement_records (the recon-readable mirror). It is a supervised loop
// (the outbox-publisher convention): poll, mirror idempotently, advance the
// durable cursor.
//
// Honest unconfigured behavior: when TIGERBEETLE_REPLICA_ADDRESSES is unset
// the sync logs the fail-closed skip and exits 0 — it mirrors NOTHING and
// fabricates no rows. When the address IS configured, every other required
// setting (cluster id, ledger->currency map, collection code, signing key)
// fails the boot on absence.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
	"github.com/munisp/blueeconomy-financial-controls/internal/revenue"
	"github.com/munisp/blueeconomy-financial-controls/internal/settlementsync"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

func main() {
	logger := log.New(os.Stdout, "settlement-sync: ", log.LstdFlags|log.LUTC)
	if err := run(logger); err != nil {
		if errors.Is(err, settlementsync.ErrUnconfigured) {
			logger.Print(err)
			return
		}
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run(logger *log.Logger) error {
	addresses := strings.TrimSpace(os.Getenv("TIGERBEETLE_REPLICA_ADDRESSES"))
	if addresses == "" {
		return settlementsync.ErrUnconfigured
	}
	databaseURL, err := required("DATABASE_URL")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pipeline, err := setupTelemetry(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := pipeline.Shutdown(context.Background()); err != nil {
			logger.Printf("telemetry shutdown failed: %v", err)
		}
	}()

	pool, err := pipeline.NewPGXPool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	signer, err := signerFromEnv()
	if err != nil {
		return err
	}
	store, err := revenue.NewStore(pool, signer)
	if err != nil {
		return err
	}

	clusterID, err := tigerbeetle.HexStringToUint128(strings.TrimSpace(os.Getenv("TIGERBEETLE_CLUSTER_ID_HEX")))
	if err != nil {
		return fmt.Errorf("parse TIGERBEETLE_CLUSTER_ID_HEX: %w", err)
	}
	replicas := strings.Split(addresses, ",")
	for i := range replicas {
		replicas[i] = strings.TrimSpace(replicas[i])
		if replicas[i] == "" {
			return errors.New("TIGERBEETLE_REPLICA_ADDRESSES contains an empty address")
		}
	}
	client, err := tigerbeetle.NewClient(clusterID, replicas)
	if err != nil {
		return fmt.Errorf("create TigerBeetle client: %w", err)
	}
	defer client.Close()

	ledgers, err := settlementsync.ParseLedgerCurrencies(os.Getenv(settlementsync.LedgerCurrenciesEnv))
	if err != nil {
		return err
	}
	code, err := requiredUint("TIGERBEETLE_CODE", 16)
	if err != nil {
		return err
	}
	limit, err := optionalUint("TB_SYNC_BATCH_LIMIT", 512, 32)
	if err != nil {
		return err
	}
	intervalSeconds, err := optionalUint("TB_SYNC_INTERVAL_SECONDS", 30, 32)
	if err != nil {
		return err
	}
	syncer, err := settlementsync.NewSyncer(client, store, settlementsync.Config{
		Ledgers: ledgers,
		Code:    uint16(code),
		Limit:   uint32(limit),
	})
	if err != nil {
		return err
	}
	logger.Printf("mirroring collection transfers (code %d) on %d ledger(s) every %ds",
		code, len(ledgers), intervalSeconds)
	return settlementsync.Run(ctx, syncer, time.Duration(intervalSeconds)*time.Second, logger)
}

func setupTelemetry(ctx context.Context) (*telemetry.Telemetry, error) {
	config, err := telemetry.LoadConfig("settlement-sync")
	if err != nil {
		return nil, fmt.Errorf("load telemetry config: %w", err)
	}
	pipeline, err := telemetry.Setup(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("setup telemetry: %w", err)
	}
	telemetry.InstallDefault(pipeline)
	return pipeline, nil
}

// signerFromEnv loads the revenue envelope signing key (env-only secret) —
// the revenue.Store contract requires it even though the mirror itself
// signs nothing (the outbox publisher signs at drain).
func signerFromEnv() (*envelope.Signer, error) {
	encoded := strings.TrimSpace(os.Getenv("REVENUE_ENVELOPE_SIGNING_KEY"))
	kid := strings.TrimSpace(os.Getenv("REVENUE_ENVELOPE_KEY_ID"))
	if encoded == "" || kid == "" {
		return nil, errors.New("REVENUE_ENVELOPE_SIGNING_KEY and REVENUE_ENVELOPE_KEY_ID are required (fail-closed)")
	}
	seed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(seed) != 32 {
		return nil, errors.New("REVENUE_ENVELOPE_SIGNING_KEY must be a base64url 32-byte Ed25519 seed")
	}
	return envelope.NewSigner(kid, seed)
}

func required(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func requiredUint(name string, bitSize int) (uint64, error) {
	value, err := required(name)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseUint(value, 10, bitSize)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be a non-zero unsigned integer", name)
	}
	return parsed, nil
}

func optionalUint(name string, fallback uint64, bitSize int) (uint64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, bitSize)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be a non-zero unsigned integer", name)
	}
	return parsed, nil
}
