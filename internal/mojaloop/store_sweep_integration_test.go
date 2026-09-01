//go:build integration

package mojaloop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
)

// TestRealPostgresCallbackOrderingAndSweep exercises the FC-3c store paths
// against a real PostgreSQL: first-seen ordering and the RESERVED-timeout
// sweep with its audit trail.
func TestRealPostgresCallbackOrderingAndSweep(t *testing.T) {
	ctx := context.Background()
	store, err := intent.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resetPublicSchema(t, ctx, store)
	for _, migrationPath := range []string{
		// 0007 alters both fx_rates and mojaloop_transfer_callbacks, so the
		// full chain is required (FC-3 made the sweep migration monolithic).
		filepath.Join("..", "..", "db", "migrations", "0001_financial_intents.sql"),
		os.Getenv("MOJALOOP_MIGRATION_PATH"),
		filepath.Join("..", "..", "db", "migrations", "0003_cvff_disbursement.sql"),
		filepath.Join("..", "..", "db", "migrations", "0007_fc3_stranded_states.sql"),
	} {
		migration, err := os.ReadFile(filepath.Clean(migrationPath))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	callbacks := NewCallbackStore(store.Pool())

	// First-seen COMMITTED is rejected: the reservation must be observed
	// before money settles.
	firstSeenCommitted := TransferCallback{TransferIdentity: TransferIdentity{TransferID: "transfer-sweep-001", PayerFSP: "FMMBE", PayeeFSP: "PARTNER01", Amount: "100", Currency: "NGN"}, TransferState: TransferCommitted, Fulfilment: "fulfilment-x"}
	if _, _, err := callbacks.ApplyCallback(ctx, firstSeenCommitted, []byte(`{"transferId":"transfer-sweep-001","transferState":"COMMITTED"}`)); !errors.Is(err, ErrInvalidTransferState) {
		t.Fatalf("first-seen COMMITTED error = %v, want invalid transition", err)
	}

	reserved := TransferCallback{TransferIdentity: TransferIdentity{TransferID: "transfer-sweep-002", PayerFSP: "FMMBE", PayeeFSP: "PARTNER01", Amount: "100", Currency: "NGN"}, TransferState: TransferReserved}
	if _, _, err := callbacks.ApplyCallback(ctx, reserved, []byte(`{"transferId":"transfer-sweep-002","transferState":"RESERVED"}`)); err != nil {
		t.Fatal(err)
	}
	// Fresh reservation: nothing to expire yet.
	now := time.Now().UTC()
	expired, err := callbacks.SweepReservedTimeouts(ctx, time.Hour, now)
	if err != nil || expired != 0 {
		t.Fatalf("fresh sweep = %d, %v; want 0", expired, err)
	}
	// Age the reservation beyond the TTL and sweep again.
	if _, err := store.Pool().Exec(ctx, `UPDATE mojaloop_transfer_callbacks SET updated_at = $1 WHERE transfer_id = $2`, now.Add(-2*time.Hour), "transfer-sweep-002"); err != nil {
		t.Fatal(err)
	}
	expired, err = callbacks.SweepReservedTimeouts(ctx, time.Hour, now)
	if err != nil || expired != 1 {
		t.Fatalf("expired sweep = %d, %v; want 1", expired, err)
	}
	var timeoutEvents int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM mojaloop_callback_timeout_events WHERE transfer_id = $1`, "transfer-sweep-002").Scan(&timeoutEvents); err != nil {
		t.Fatal(err)
	}
	if timeoutEvents != 1 {
		t.Fatalf("timeout audit events = %d, want 1", timeoutEvents)
	}
	// Idempotent: a second sweep does not re-mark.
	expired, err = callbacks.SweepReservedTimeouts(ctx, time.Hour, now)
	if err != nil || expired != 0 {
		t.Fatalf("repeat sweep = %d, %v; want 0", expired, err)
	}
	// The Hub-signed terminal callback remains acceptable after the local
	// timeout (Hub truth is never fabricated, nor is real settlement
	// evidence refused).
	committed := reserved
	committed.TransferState = TransferCommitted
	committed.Fulfilment = "fulfilment-002"
	updated, _, err := callbacks.ApplyCallback(ctx, committed, []byte(`{"transferId":"transfer-sweep-002","transferState":"COMMITTED","fulfilment":"fulfilment-002"}`))
	if err != nil || updated.TransferState != TransferCommitted {
		t.Fatalf("post-timeout commit failed: %+v %v", updated, err)
	}
}
