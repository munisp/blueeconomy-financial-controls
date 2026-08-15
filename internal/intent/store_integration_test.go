//go:build integration

package intent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRealPostgresIntentFlow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
	var outboxCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM financial_intent_outbox WHERE intent_id = $1`, request.IntentID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 3 {
		t.Fatalf("outbox count = %d, want 3", outboxCount)
	}
}
