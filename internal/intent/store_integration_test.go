//go:build integration

package intent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// resetPublicSchema gives each integration test a clean public schema —
// the repo convention for the real-PostgreSQL harnesses — so several
// integration tests can share one database (e.g. in CI).
func resetPublicSchema(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	if err := store.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}

func TestRealPostgresIntentFlow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resetPublicSchema(t, ctx, store)
	migration, err := os.ReadFile(filepath.Clean(os.Getenv("MIGRATION_PATH")))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	request := validRequest()
	created, err := store.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.State != StateDraft || created.Version != 1 {
		t.Fatalf("unexpected created intent: %+v", created)
	}
	replay, err := store.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.IntentID != created.IntentID || replay.Version != created.Version {
		t.Fatal("exact replay did not return retained intent")
	}
	conflict := request
	conflict.Amount++
	if _, err := store.Create(ctx, conflict); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	approved, err := store.Approve(ctx, request.IntentID, 1, "checker-001")
	if err != nil || approved.State != StateApproved || approved.Version != 2 {
		t.Fatalf("approval failed: %+v %v", approved, err)
	}
	requested, err := store.Transition(ctx, request.IntentID, 2, StateReservationRequested)
	if err != nil || requested.State != StateReservationRequested || requested.Version != 3 {
		t.Fatalf("reservation request failed: %+v %v", requested, err)
	}
	reserved, err := store.Transition(ctx, request.IntentID, 3, StateReserved)
	if err != nil || reserved.State != StateReserved || reserved.Version != 4 {
		t.Fatalf("reserve transition failed: %+v %v", reserved, err)
	}
	posted, err := store.Transition(ctx, request.IntentID, 4, StatePosted)
	if err != nil || posted.State != StatePosted || posted.Version != 5 {
		t.Fatalf("post transition failed: %+v %v", posted, err)
	}
	listed, err := store.ListReconciliationIntents(ctx)
	if err != nil || len(listed) != 1 || listed[0].ExternalRef != request.ExternalRef || listed[0].State != StatePosted {
		t.Fatalf("reconciliation listing failed: %+v %v", listed, err)
	}
	var outboxCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM financial_intent_outbox WHERE intent_id = $1`, request.IntentID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 5 {
		t.Fatalf("outbox count = %d, want 5", outboxCount)
	}
}
