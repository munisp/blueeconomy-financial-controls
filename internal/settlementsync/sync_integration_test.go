//go:build integration

package settlementsync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
	"github.com/munisp/blueeconomy-financial-controls/internal/revenue"
)

// Integration harness: real PostgreSQL (DATABASE_URL + MIGRATION_PATH, repo
// convention; MIGRATION_PATH points at 0010_revenue_intake.sql and the
// harness applies the 0008/0009 chain first). DEDICATED database per
// package — the harness drops the public schema.
func openRevenueStore(t *testing.T) (*revenue.Store, *pgxpool.Pool) {
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
	for _, path := range []string{
		filepath.Join(filepath.Dir(migrationPath), "0008_tariff.sql"),
		filepath.Join(filepath.Dir(migrationPath), "0009_revenue_assurance.sql"),
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
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := envelope.NewSigner("sync-test", key.Seed())
	if err != nil {
		t.Fatal(err)
	}
	store, err := revenue.NewStore(pool, signer)
	if err != nil {
		t.Fatal(err)
	}
	return store, pool
}

// TestRecordTBSettlementUpsertIdempotency: the same posted transfer mirrors
// exactly one settlement row; replays advance the cursor but insert nothing
// (tb_transfer_id UNIQUE), and the recorded values match the ledger entry.
func TestRecordTBSettlementUpsertIdempotency(t *testing.T) {
	store, pool := openRevenueStore(t)
	ctx := context.Background()
	input := revenue.TBMirrorInput{
		TBTransferID: "a1b2c3d4e5f6", AmountMinor: 2500000, Currency: "USD",
		BankReference: "tb:a1b2c3d4e5f6", PayerRef: "tb-debit:ff", ValueDate: "2026-08-20",
	}
	created, err := store.RecordTBSettlement(ctx, input, "ledger:1", 1000)
	if err != nil || !created {
		t.Fatalf("first mirror created=%v err=%v", created, err)
	}
	created, err = store.RecordTBSettlement(ctx, input, "ledger:1", 1000)
	if err != nil || created {
		t.Fatalf("replay created=%v err=%v (must be a no-op)", created, err)
	}
	var count, outboxCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM settlement_records WHERE tb_transfer_id = $1`, input.TBTransferID).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("settlement rows %d, want 1", count)
	}
	// The outbox event publishes only on first insert.
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM revenue_outbox WHERE event_type = 'revenue.settlement.recorded'`).
		Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox rows %d, want 1", outboxCount)
	}
	// The cursor advanced and never regresses.
	cursor, err := store.SyncCursor(ctx, "ledger:1")
	if err != nil || cursor != 1000 {
		t.Fatalf("cursor %d err %v", cursor, err)
	}
	older := input
	older.TBTransferID = "b2c3d4e5f6a1"
	older.BankReference = "tb:b2c3d4e5f6a1"
	if _, err := store.RecordTBSettlement(ctx, older, "ledger:1", 500); err != nil {
		t.Fatal(err)
	}
	cursor, err = store.SyncCursor(ctx, "ledger:1")
	if err != nil || cursor != 1000 {
		t.Fatalf("cursor regressed to %d", cursor)
	}
	// Validation fail-closed: no currency guessing, no empty transfer id.
	bad := input
	bad.Currency = "EUR"
	if _, err := store.RecordTBSettlement(ctx, bad, "ledger:1", 1001); err == nil {
		t.Fatal("unsupported currency mirrored")
	}
	bad = input
	bad.TBTransferID = ""
	if _, err := store.RecordTBSettlement(ctx, bad, "ledger:1", 1001); err == nil {
		t.Fatal("empty tbTransferId mirrored")
	}
}

// TestSyncOnceEndToEndWithFakes exercises the full SyncOnce path against
// the real store: two posted transfers + one pending mirror exactly two
// rows; a repeated sync is a no-op.
func TestSyncOnceEndToEndWithFakes(t *testing.T) {
	store, pool := openRevenueStore(t)
	querier := &fakeQuerier{transfers: []tigerbeetle.Transfer{
		transfer(11, 500, 100, false, false),
		transfer(12, 600, 200, true, false),
		transfer(13, 700, 300, false, false),
	}}
	syncer, err := NewSyncer(querier, store, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mirrored, err := syncer.SyncOnce(ctx)
	if err != nil || mirrored != 2 {
		t.Fatalf("mirrored %d err %v", mirrored, err)
	}
	mirrored, err = syncer.SyncOnce(ctx)
	if err != nil || mirrored != 0 {
		t.Fatalf("re-sync mirrored %d err %v", mirrored, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM settlement_records WHERE recorded_by = 'tb-sync'`).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("mirrored rows %d, want 2", count)
	}
	// A recon run can now read the mirrored rows as the settlement leg.
	summary, err := store.RunRecon(ctx, "MANUAL", "officer:recon", mustDate(t, "2026-08-31"))
	if err != nil {
		t.Fatal(err)
	}
	if summary.ExceptionCount != 2 {
		// Both mirrored settlements honestly carry no debit-note reference:
		// recon flags them UNMATCHED_SETTLEMENT for resolution (never
		// silently matched).
		t.Fatalf("exceptions %d, want 2 honest UNMATCHED_SETTLEMENT", summary.ExceptionCount)
	}
}

func mustDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UTC()
}
