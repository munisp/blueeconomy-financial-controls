// stamps-intake consumes the tax-stamps service's signed excise stamp
// lifecycle events from the four stamps.* topics and lands them in the
// financial-controls revenue pipeline. Every message is
// JWS-verified against the env-only trusted authority keys and deduplicated
// by event id; nothing unverified is ever landed, and the consumer group
// blocks (fail-closed) rather than committing an offset it could not land.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/munisp/blueeconomy-financial-controls/internal/stampsintake"
	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

func main() {
	logger := log.New(os.Stdout, "stamps-intake: ", log.LstdFlags|log.LUTC)
	if err := run(logger); err != nil {
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run(logger *log.Logger) error {
	databaseURL, err := required("DATABASE_URL")
	if err != nil {
		return err
	}
	brokersRaw, err := required("KAFKA_BROKERS")
	if err != nil {
		return err
	}
	groupID, err := required("STAMPS_INTAKE_GROUP_ID")
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
	store, err := stampsintake.NewStore(pool)
	if err != nil {
		return err
	}
	verifier, err := stampsintake.VerifierFromEnv()
	if err != nil {
		return err
	}
	consumer, err := stampsintake.NewConsumer(stampsintake.Config{
		Brokers: strings.Split(brokersRaw, ","),
		GroupID: groupID,
	}, verifier, store, logger)
	if err != nil {
		return err
	}
	defer consumer.Close()
	logger.Printf("consuming %v (group %s)", stampsintake.Topics, groupID)
	return consumer.Run(ctx)
}

func setupTelemetry(ctx context.Context) (*telemetry.Telemetry, error) {
	config, err := telemetry.LoadConfig("stamps-intake")
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

func required(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}
