//go:build integration

package outbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration harness: real PostgreSQL (DATABASE_URL + MIGRATION_PATH, repo
// convention). MIGRATION_PATH points at 0009_revenue_assurance.sql; the
// harness applies the 0008 tariff migration from the same directory first
// (0009 references tariff_assessments). DEDICATED database per package —
// the harness drops the public schema.
func openSource(t *testing.T) (*PostgresSource, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}
	migrationPath := os.Getenv("MIGRATION_PATH")
	if migrationPath == "" {
		t.Fatal("MIGRATION_PATH is required for integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	// The source drains every registered outbox table, so the full chain of
	// outbox-carrying migrations must be present.
	for _, path := range []string{
		filepath.Join(filepath.Dir(migrationPath), "0001_financial_intents.sql"),
		filepath.Join(filepath.Dir(migrationPath), "0003_cvff_disbursement.sql"),
		filepath.Join(filepath.Dir(migrationPath), "0008_tariff.sql"),
		migrationPath,
	} {
		statement, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read migration %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(statement)); err != nil {
			t.Fatalf("apply migration %s: %v", path, err)
		}
	}
	return NewPostgresSource(pool), pool
}

// TestRevenueOutboxDrainRegistration proves the W-CLOSE-FC publisher
// registration: revenue_outbox rows are drained in creation order with
// their subject id, map onto the finance.revenue.v1 envelope contract, and
// MarkPublished is a one-shot (replay-safe) stamp.
func TestRevenueOutboxDrainRegistration(t *testing.T) {
	source, pool := openSource(t)
	ctx := context.Background()

	eventID := uuid.New()
	payload, _ := json.Marshal(map[string]any{
		"settlementId": "set-001", "bankReference": "tb:abc",
		"amountMinor": 125000, "currency": "USD", "actor": "tb-sync",
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO revenue_outbox (event_id, subject_id, event_type, payload, created_at)
		VALUES ($1, 'set-001', 'revenue.settlement.recorded', $2, $3)`,
		eventID, payload, time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert revenue_outbox row: %v", err)
	}

	events, err := source.Unpublished(ctx, 10)
	if err != nil {
		t.Fatalf("unpublished: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("unpublished %d events, want 1: %+v", len(events), events)
	}
	event := events[0]
	if event.EventID != eventID.String() || event.SubjectID != "set-001" ||
		event.EventType != "revenue.settlement.recorded" {
		t.Fatalf("drained event: %+v", event)
	}
	// The envelope mapping is registered (unknown types fail closed).
	envelope, err := BuildEnvelope(event)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if envelope.EventType != "finance.revenue.v1" {
		t.Fatalf("envelope event type %q, want finance.revenue.v1", envelope.EventType)
	}

	if err := source.MarkPublished(ctx, event); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	remaining, err := source.Unpublished(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("still unpublished after mark: %+v", remaining)
	}
	// Marking twice fails (no silent re-stamp).
	if err := source.MarkPublished(ctx, event); err == nil {
		t.Fatal("double mark accepted")
	}
}
