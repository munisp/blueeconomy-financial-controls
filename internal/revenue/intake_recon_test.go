package revenue

import "testing"

// TestPlanReconIntakeSettlementMatch: a settlement remitting with the intake
// assessment's call reference matches the assessment-side intake leg — no
// exception, no debit-note match row.
func TestPlanReconIntakeSettlementMatch(t *testing.T) {
	plan := planRecon(reconInput{
		Intake: []IntakeAssessment{{
			EventID: "evt-1", CallReference: "SBM-2026-0007", AssessmentID: "a-ext-1",
			TotalMinor: 2500000, Currency: "USD",
		}},
		Settlements: []Settlement{{
			SettlementID: "s1", BankReference: "SBM-2026-0007",
			AmountMinor: 2500000, Currency: "USD", PayerRef: "OP", ValueDate: "2026-08-20",
		}},
		AsOf: reconAt("2026-08-31"),
	})
	if len(plan.IntakeMatches) != 1 || plan.IntakeMatches[0].EventID != "evt-1" || plan.IntakeMatches[0].SettlementID != "s1" {
		t.Fatalf("intake matches: %+v", plan.IntakeMatches)
	}
	if len(plan.Exceptions) != 0 || len(plan.Matches) != 0 {
		t.Fatalf("plan: %+v", plan)
	}
}

// TestPlanReconIntakeAmountMismatch: wrong amount against an intake
// assessment is an AMOUNT_MISMATCH, never a silent match.
func TestPlanReconIntakeAmountMismatch(t *testing.T) {
	plan := planRecon(reconInput{
		Intake: []IntakeAssessment{{
			EventID: "evt-2", CallReference: "CRZ-1", TotalMinor: 7400000, Currency: "NGN",
		}},
		Settlements: []Settlement{{
			SettlementID: "s2", BankReference: "CRZ-1",
			AmountMinor: 7300000, Currency: "NGN", PayerRef: "OP", ValueDate: "2026-08-20",
		}},
		AsOf: reconAt("2026-08-31"),
	})
	if len(plan.IntakeMatches) != 0 {
		t.Fatalf("mismatched intake matched: %+v", plan.IntakeMatches)
	}
	if exceptionClasses(plan)[ExceptionAmountMismatch] != 1 {
		t.Fatalf("exceptions: %+v", plan.Exceptions)
	}
	if plan.Exceptions[0].ExpectedMinor == nil || *plan.Exceptions[0].ExpectedMinor != 7400000 {
		t.Fatalf("expected: %+v", plan.Exceptions[0].ExpectedMinor)
	}
}

// TestPlanReconIntakeDebitNotePrecedence: a settlement that names a debit
// note is never diverted to the intake leg even when the reference matches.
func TestPlanReconIntakeDebitNotePrecedence(t *testing.T) {
	plan := planRecon(reconInput{
		Notes: []DebitNote{openNote("n1", "NPA-2026-000099", 5000, "2026-12-01")},
		Intake: []IntakeAssessment{{
			EventID: "evt-3", CallReference: "BR-SHARED", TotalMinor: 5000, Currency: "USD",
		}},
		Settlements: []Settlement{{
			SettlementID: "s3", DebitNoteID: "n1", BankReference: "BR-SHARED",
			AmountMinor: 5000, Currency: "USD", PayerRef: "OP", ValueDate: "2026-08-20",
		}},
		AsOf: reconAt("2026-08-31"),
	})
	if len(plan.IntakeMatches) != 0 {
		t.Fatalf("debit-note settlement diverted to intake: %+v", plan.IntakeMatches)
	}
	// The note leg stays pending its statement line — no exception.
	if len(plan.Exceptions) != 0 {
		t.Fatalf("exceptions: %+v", plan.Exceptions)
	}
}

// TestPlanReconUnmappableIntakeSurfaced: authentic-but-unmappable intake is
// surfaced as UNMATCHED_STATEMENT and never matched.
func TestPlanReconUnmappableIntakeSurfaced(t *testing.T) {
	plan := planRecon(reconInput{
		Intake: []IntakeAssessment{{
			EventID: "evt-4", MappingError: "assessment domain-payload extension is missing",
		}},
		Settlements: []Settlement{{
			SettlementID: "s4", BankReference: "evt-4",
			AmountMinor: 100, Currency: "USD", PayerRef: "OP", ValueDate: "2026-08-20",
		}},
		AsOf: reconAt("2026-08-31"),
	})
	if len(plan.IntakeMatches) != 0 {
		t.Fatalf("unmappable intake matched: %+v", plan.IntakeMatches)
	}
	classes := exceptionClasses(plan)
	if classes[ExceptionUnmatchedStatement] != 1 {
		t.Fatalf("exceptions: %+v", plan.Exceptions)
	}
	// The settlement itself still identifies nothing.
	if classes[ExceptionUnmatchedSettlement] != 1 {
		t.Fatalf("exceptions: %+v", plan.Exceptions)
	}
}

// TestPlanReconIntakeUnknownSettlement: a settlement referencing neither a
// note nor an intake assessment stays UNMATCHED_SETTLEMENT.
func TestPlanReconIntakeUnknownSettlement(t *testing.T) {
	plan := planRecon(reconInput{
		Intake: []IntakeAssessment{{
			EventID: "evt-5", CallReference: "SBM-1", TotalMinor: 100, Currency: "USD",
		}},
		Settlements: []Settlement{{
			SettlementID: "s5", BankReference: "BR-UNKNOWN",
			AmountMinor: 100, Currency: "USD", PayerRef: "OP", ValueDate: "2026-08-20",
		}},
		AsOf: reconAt("2026-08-31"),
	})
	if classes := exceptionClasses(plan); classes[ExceptionUnmatchedSettlement] != 1 {
		t.Fatalf("exceptions: %+v", plan.Exceptions)
	}
	if len(plan.IntakeMatches) != 0 {
		t.Fatalf("intake matches: %+v", plan.IntakeMatches)
	}
}
