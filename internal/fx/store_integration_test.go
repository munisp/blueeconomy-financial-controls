//go:build integration

package fx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealPostgresRateExpiry exercises the FC-3a store paths against a real
// PostgreSQL: the pending-confirmation TTL sweep and the fail-closed reading
// of an expired rate.
func TestRealPostgresRateExpiry(t *testing.T) {
	ctx := context.Background()
	databaseURL := os.Getenv("DATABASE_URL")
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, migrationPath := range []string{
		filepath.Join("..", "..", "db", "migrations", "0001_financial_intents.sql"),
		filepath.Join("..", "..", "db", "migrations", "0003_cvff_disbursement.sql"),
		filepath.Join("..", "..", "db", "migrations", "0007_fc3_stranded_states.sql"),
	} {
		migration, err := os.ReadFile(migrationPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	effectiveDate := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rate, err := NewRate("11111111-1111-1111-1111-111111111111", 1_550_250_000, effectiveDate, "kc-maker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enter(ctx, rate); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Within the confirmation window: nothing expires.
	expired, err := store.ExpirePendingConfirmation(ctx, time.Hour, now)
	if err != nil || expired != 0 {
		t.Fatalf("fresh sweep = %d, %v; want 0", expired, err)
	}
	// Age the entry beyond the TTL; the sweep expires it.
	if _, err := store.pool.Exec(ctx, `UPDATE fx_rates SET created_at = $1 WHERE rate_id = $2`, now.Add(-2*time.Hour), rate.RateID); err != nil {
		t.Fatal(err)
	}
	expired, err = store.ExpirePendingConfirmation(ctx, time.Hour, now)
	if err != nil || expired != 1 {
		t.Fatalf("expired sweep = %d, %v; want 1", expired, err)
	}
	// Fail-closed: an expired rate can no longer be confirmed and reads as
	// unavailable, never as a default rate.
	if _, err := store.Confirm(ctx, rate.RateID, "kc-checker"); !errors.Is(err, ErrRateExpired) {
		t.Fatalf("confirm expired error = %v, want ErrRateExpired", err)
	}
	if _, err := store.ConfirmedForDate(ctx, effectiveDate); !errors.Is(err, ErrRateNotFound) {
		t.Fatalf("confirmed lookup after expiry error = %v, want ErrRateNotFound", err)
	}
	// Idempotent: a second sweep does not re-expire.
	expired, err = store.ExpirePendingConfirmation(ctx, time.Hour, now)
	if err != nil || expired != 0 {
		t.Fatalf("repeat sweep = %d, %v; want 0", expired, err)
	}
	if _, err := store.ExpirePendingConfirmation(ctx, 0, now); err == nil {
		t.Fatal("non-positive ttl accepted")
	}
}
