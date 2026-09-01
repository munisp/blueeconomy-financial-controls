//go:build integration

package intent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRealPostgresVoidAndResolve exercises the FC-2/FC-3b store paths against
// a real PostgreSQL: maker DRAFT void with audit, and the officer AMBIGUOUS
// resolutions with audit.
func TestRealPostgresVoidAndResolve(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resetPublicSchema(t, ctx, store)
	for _, migrationPath := range []string{
		os.Getenv("MIGRATION_PATH"),
		filepath.Join("..", "..", "db", "migrations", "0006_intent_officer_resolution.sql"),
	} {
		migration, err := os.ReadFile(filepath.Clean(migrationPath))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}

	// Maker void of a DRAFT.
	voidRequest := validRequest()
	voidRequest.IntentID = "intent-void-001"
	voidRequest.ExternalRef = "ref-void-001"
	if _, err := store.Create(ctx, voidRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VoidDraft(ctx, voidRequest.IntentID, 1, "checker-001"); !errors.Is(err, ErrNotMaker) {
		t.Fatalf("non-maker void error = %v", err)
	}
	voided, err := store.VoidDraft(ctx, voidRequest.IntentID, 1, voidRequest.Maker)
	if err != nil || voided.State != StateVoided {
		t.Fatalf("maker void failed: %+v %v", voided, err)
	}
	if _, err := store.VoidDraft(ctx, voidRequest.IntentID, 2, voidRequest.Maker); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("re-void error = %v", err)
	}

	// Officer resolution of an AMBIGUOUS intent.
	resolveRequest := validRequest()
	resolveRequest.IntentID = "intent-resolve-001"
	resolveRequest.ExternalRef = "ref-resolve-001"
	if _, err := store.Create(ctx, resolveRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveAmbiguous(ctx, resolveRequest.IntentID, 1, "officer-001", ResolutionVoid); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("resolve on DRAFT error = %v", err)
	}
	if _, err := store.Approve(ctx, resolveRequest.IntentID, 1, "checker-001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(ctx, resolveRequest.IntentID, 2, StateReservationRequested); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition(ctx, resolveRequest.IntentID, 3, StateReconciliationRequired); err != nil {
		t.Fatal(err)
	}
	ambiguous, err := store.Transition(ctx, resolveRequest.IntentID, 4, StateAmbiguous)
	if err != nil || ambiguous.State != StateAmbiguous {
		t.Fatalf("park ambiguous failed: %+v %v", ambiguous, err)
	}
	if _, err := store.ResolveAmbiguous(ctx, resolveRequest.IntentID, 4, "officer-001", ResolutionVoid); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-version resolve error = %v", err)
	}
	if _, err := store.ResolveAmbiguous(ctx, resolveRequest.IntentID, 5, "officer 001", ResolutionVoid); err == nil {
		t.Fatal("non-canonical officer accepted")
	}
	reconciled, err := store.ResolveAmbiguous(ctx, resolveRequest.IntentID, 5, "officer-001", ResolutionReconcile)
	if err != nil || reconciled.State != StateReconciliationRequired {
		t.Fatalf("reconcile resolution failed: %+v %v", reconciled, err)
	}
	if _, err := store.Transition(ctx, resolveRequest.IntentID, 6, StateAmbiguous); err != nil {
		t.Fatal(err)
	}
	resolvedVoid, err := store.ResolveAmbiguous(ctx, resolveRequest.IntentID, 7, "officer-001", ResolutionVoid)
	if err != nil || resolvedVoid.State != StateVoided {
		t.Fatalf("void resolution failed: %+v %v", resolvedVoid, err)
	}

	var voidEvents, resolveEvents int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM financial_intent_outbox WHERE intent_id = $1 AND event_type = 'financial_intent.voided'`, voidRequest.IntentID).Scan(&voidEvents); err != nil {
		t.Fatal(err)
	}
	if voidEvents != 1 {
		t.Fatalf("void audit events = %d, want 1", voidEvents)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM financial_intent_outbox WHERE intent_id = $1 AND event_type = 'financial_intent.officer_resolved'`, resolveRequest.IntentID).Scan(&resolveEvents); err != nil {
		t.Fatal(err)
	}
	if resolveEvents != 2 {
		t.Fatalf("officer audit events = %d, want 2", resolveEvents)
	}
	var officer string
	if err := store.pool.QueryRow(ctx, `SELECT payload->>'officer' FROM financial_intent_outbox WHERE intent_id = $1 AND event_type = 'financial_intent.officer_resolved' ORDER BY created_at LIMIT 1`, resolveRequest.IntentID).Scan(&officer); err != nil {
		t.Fatal(err)
	}
	if officer != "officer-001" {
		t.Fatalf("audit officer = %q, want officer-001", officer)
	}
}
