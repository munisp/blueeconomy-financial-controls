//go:build integration

package revenue

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// insertIntake seeds one revenue-intake assessment row directly (the intake
// consumer's landing path is covered in internal/revenueintake; here the
// recon pipeline integration is under test).
func insertIntake(t *testing.T, store *Store, eventID, callRef, assessmentID string, totalMinor int64, currency, mappingError string) {
	t.Helper()
	var totalArg any
	if totalMinor >= 0 {
		totalArg = totalMinor
	}
	if _, err := store.pool.Exec(context.Background(), `
		INSERT INTO revenue_intake_assessments
		    (event_id, topic, event_type, producer, signer_kid, occurred_at, correlation_id,
		     domain, call_reference, schedule_id, assessment_id, total_minor, currency,
		     mapping_error, payload)
		VALUES ($1, 'finance.revenue-assessments.v1', 'revenue.assessment_issued',
		        's1-port-interoperability', 'port-interoperability-1', now(), $2,
		        'OFFSHORE_TERMINAL', $3, 'sched-1', $4, $5, $6, $7, '{}'::jsonb)`,
		eventID, "corr-"+eventID, callRef, assessmentID, totalArg, currency, mappingError); err != nil {
		t.Fatalf("insert intake: %v", err)
	}
}

// TestReconLandsIntakeAssessmentSettlement: a settlement remitted with the
// intake assessment's call reference marks the assessment
// SETTLEMENT_OBSERVED instead of raising UNMATCHED_SETTLEMENT.
func TestReconLandsIntakeAssessmentSettlement(t *testing.T) {
	store, pool, _, _ := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	eventID := uuid.NewString()
	insertIntake(t, store, eventID, "SBM-2026-0007", "assess-ext-1", 2500000, "USD", "")
	settlement, err := store.RecordSettlement(ctx, SettlementInput{
		BankReference: "SBM-2026-0007", AmountMinor: 2500000, Currency: "USD",
		PayerRef: "TERMINAL-OPERATOR", ValueDate: "2026-08-20",
	}, "idem-set-intake-1", "officer:collections")
	if err != nil {
		t.Fatalf("record settlement: %v", err)
	}

	asOf, _ := time.Parse("2006-01-02", "2026-08-31")
	if _, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC()); err != nil {
		t.Fatalf("run recon: %v", err)
	}
	var state, linkedSettlement string
	if err := pool.QueryRow(ctx, `
		SELECT state, settlement_id::text FROM revenue_intake_assessments WHERE event_id = $1`, eventID).
		Scan(&state, &linkedSettlement); err != nil {
		t.Fatal(err)
	}
	if state != "SETTLEMENT_OBSERVED" || linkedSettlement != settlement.SettlementID {
		t.Fatalf("intake state=%s settlement=%s", state, linkedSettlement)
	}
	exceptions, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, exception := range exceptions {
		if exception.SettlementID == settlement.SettlementID {
			t.Fatalf("intake-matched settlement raised %s", exception.Class)
		}
	}

	// Replay safety: a second run neither re-links nor raises.
	if _, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	after, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(exceptions) {
		t.Fatalf("re-run raised %d exceptions, want %d", len(after), len(exceptions))
	}
}

// TestReconIntakeAmountMismatch: a settlement referencing the intake
// assessment with the wrong amount raises AMOUNT_MISMATCH carrying the
// assessment's expected total.
func TestReconIntakeAmountMismatch(t *testing.T) {
	store, _, _, _ := openStore(t)
	ctx := context.Background()

	eventID := uuid.NewString()
	insertIntake(t, store, eventID, "CRZ-2026-0042", "assess-ext-2", 7400000, "NGN", "")
	settlement, err := store.RecordSettlement(ctx, SettlementInput{
		BankReference: "CRZ-2026-0042", AmountMinor: 7300000, Currency: "NGN",
		PayerRef: "CRUISE-OPERATOR", ValueDate: "2026-08-21",
	}, "idem-set-intake-2", "officer:collections")
	if err != nil {
		t.Fatal(err)
	}
	asOf, _ := time.Parse("2006-01-02", "2026-08-31")
	if _, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC()); err != nil {
		t.Fatal(err)
	}
	exceptions, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var found *Exception
	for index, exception := range exceptions {
		if exception.Class == ExceptionAmountMismatch && exception.SettlementID == settlement.SettlementID {
			found = &exceptions[index]
		}
	}
	if found == nil {
		t.Fatalf("no AMOUNT_MISMATCH for intake settlement; queue: %+v", exceptions)
	}
	if found.ExpectedMinor == nil || *found.ExpectedMinor != 7400000 {
		t.Fatalf("expected total: %+v", found.ExpectedMinor)
	}
}

// TestReconSurfacesUnmappableIntake: an authentic-but-unmappable intake
// event is surfaced as an UNMATCHED_STATEMENT-class recon item (landed in
// the exception queue, never guessed into a money record), idempotently.
func TestReconSurfacesUnmappableIntake(t *testing.T) {
	store, _, _, _ := openStore(t)
	ctx := context.Background()

	eventID := uuid.NewString()
	insertIntake(t, store, eventID, "", "", -1, "", "assessment domain-payload extension is missing")
	asOf, _ := time.Parse("2006-01-02", "2026-08-31")
	if _, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC()); err != nil {
		t.Fatal(err)
	}
	exceptions, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, exception := range exceptions {
		if exception.Class == ExceptionUnmatchedStatement {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("UNMATCHED_STATEMENT count %d, want 1; queue: %+v", count, exceptions)
	}
	// Re-run: the OPEN dedupe index keeps it a single item.
	if _, err := store.RunRecon(ctx, "MANUAL", "officer:recon", asOf.UTC()); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListExceptions(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(exceptions) {
		t.Fatalf("re-run raised %d exceptions, want %d", len(after), len(exceptions))
	}
}
