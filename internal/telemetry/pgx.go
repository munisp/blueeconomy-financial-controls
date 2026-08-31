package telemetry

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplyPoolEnv overrides pgx pool sizing from the environment. Every knob is
// optional: unset (or <= 0) values keep the pgx defaults, so behavior is
// unchanged for existing deployments.
//
//	DB_POOL_MAX_CONNS          max open connections   (default: pgx default, max(4, NumCPU))
//	DB_POOL_MIN_CONNS          min idle connections   (default: 0)
//	DB_POOL_MAX_CONN_IDLE_SEC  max connection idle time in seconds (default: 1800)
//	DB_POOL_MAX_CONN_LIFE_SEC  max connection lifetime in seconds (default: 3600)
func ApplyPoolEnv(config *pgxpool.Config) error {
	if raw := os.Getenv("DB_POOL_MAX_CONNS"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return fmt.Errorf("DB_POOL_MAX_CONNS must be a positive integer, got %q", raw)
		}
		config.MaxConns = int32(value)
	}
	if raw := os.Getenv("DB_POOL_MIN_CONNS"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return fmt.Errorf("DB_POOL_MIN_CONNS must be a non-negative integer, got %q", raw)
		}
		config.MinConns = int32(value)
	}
	if raw := os.Getenv("DB_POOL_MAX_CONN_IDLE_SEC"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return fmt.Errorf("DB_POOL_MAX_CONN_IDLE_SEC must be a positive integer, got %q", raw)
		}
		config.MaxConnIdleTime = time.Duration(value) * time.Second
	}
	if raw := os.Getenv("DB_POOL_MAX_CONN_LIFE_SEC"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return fmt.Errorf("DB_POOL_MAX_CONN_LIFE_SEC must be a positive integer, got %q", raw)
		}
		config.MaxConnLifetime = time.Duration(value) * time.Second
	}
	return nil
}

// NewPGXPool opens a pgx pool with the otelpgx tracer installed: every query,
// batch, copy and acquire emits a client span that joins the caller trace.
// With telemetry disabled the tracer runs over a noop provider and records
// nothing; pool semantics are unchanged.
func (telemetry *Telemetry) NewPGXPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres DSN: %w", err)
	}
	config.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTracerProvider(telemetry.TracerProvider()))
	if err := ApplyPoolEnv(config); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	return pool, nil
}
