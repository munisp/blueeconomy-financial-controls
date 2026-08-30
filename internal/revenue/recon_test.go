package revenue

import (
	"testing"
	"time"
)

func openNote(id, doc string, usd int64, due string) DebitNote {
	return DebitNote{
		DebitNoteID: id, AssessmentID: "a-" + id, Agency: AgencyNPA, EntityRef: "ACME",
		DocumentNumber: doc, AmountUSDMinor: usd, EffectiveDate: "2026-01-01",
		DueDate: due, State: StateIssued, Maker: "maker-1",
	}
}

func reconAt(date string) time.Time {
	parsed, _ := time.Parse("2006-01-02", date)
	return parsed.UTC()
}

func exceptionClasses(plan reconPlan) map[string]int {
	counts := map[string]int{}
	for _, exception := range plan.Exceptions {
		counts[exception.Class]++
	}
	return counts
}

// TestPlanReconCleanMatch: note + settlement + statement line agree ->
// three-way match and the note settles.
func TestPlanReconCleanMatch(t *testing.T) {
	plan := planRecon(reconInput{
		Notes: []DebitNote{openNote("n1", "NPA-2026-000001", 5000, "2026-02-01")},
		Settlements: []Settlement{{
			SettlementID: "s1", DebitNoteID: "n1", BankReference: "BR-1",
			AmountMinor: 5000, Currency: "USD", PayerRef: "ACME", ValueDate: "2026-01-20",
		}},
		Lines: []statementLeg{{
			StatementID: "st1", LineNo: 1, BankReference: "BR-1",
			ValueDate: "2026-01-20", AmountMinor: 5000, Currency: "USD",
		}},
		AsOf: reconAt("2026-01-31"),
	})
	if len(plan.Matches) != 1 || len(plan.Exceptions) != 0 {
		t.Fatalf("plan: %+v", plan)
	}
	if plan.Matches[0].DebitNoteID != "n1" || plan.Matches[0].SettlementID != "s1" || plan.Matches[0].StatementID != "st1" {
		t.Fatalf("match: %+v", plan.Matches[0])
	}
	if plan.SettledNotes["n1"] != "s1" {
		t.Fatalf("settled: %+v", plan.SettledNotes)
	}
}

// TestPlanReconAllExceptionClasses exercises every exception class in one
// batch plus a pending (statement-late) leg that raises nothing.
func TestPlanReconAllExceptionClasses(t *testing.T) {
	plan := planRecon(reconInput{
		Notes: []DebitNote{
			openNote("n-mismatch", "NPA-2026-000010", 5000, "2026-03-01"),
			openNote("n-pastdue", "NPA-2026-000011", 7000, "2026-01-15"), // past due, no settlement
			openNote("n-pending", "NPA-2026-000012", 9000, "2026-03-01"),
		},
		Settlements: []Settlement{
			{SettlementID: "s-mismatch", DebitNoteID: "n-mismatch", BankReference: "BR-M",
				AmountMinor: 4999, Currency: "USD", PayerRef: "ACME", ValueDate: "2026-02-01"},
			{SettlementID: "s-orphan", DebitNoteID: "", BankReference: "BR-UNKNOWN",
				AmountMinor: 1000, Currency: "USD", PayerRef: "GHOST", ValueDate: "2026-02-01"},
			{SettlementID: "s-dup-1", DebitNoteID: "", BankReference: "NPA-2026-000012",
				AmountMinor: 9000, Currency: "USD", PayerRef: "ACME", ValueDate: "2026-02-02"},
			{SettlementID: "s-dup-2", DebitNoteID: "", BankReference: "NPA-2026-000012",
				AmountMinor: 9000, Currency: "USD", PayerRef: "ACME", ValueDate: "2026-02-03"},
		},
		Lines: []statementLeg{
			{StatementID: "st1", LineNo: 1, BankReference: "BR-ORPHAN-LINE",
				ValueDate: "2026-02-01", AmountMinor: 1234, Currency: "USD"},
		},
		AsOf: reconAt("2026-02-10"),
	})
	counts := exceptionClasses(plan)
	if counts[ExceptionAmountMismatch] != 1 {
		t.Fatalf("amount mismatch count: %+v\nplan: %+v", counts, plan.Exceptions)
	}
	if counts[ExceptionUnmatchedSettlement] != 1 {
		t.Fatalf("unmatched settlement count: %+v", counts)
	}
	if counts[ExceptionUnmatchedStatement] != 1 {
		t.Fatalf("unmatched statement count: %+v", counts)
	}
	if counts[ExceptionUnmatchedAssessment] != 1 {
		t.Fatalf("unmatched assessment count: %+v", counts)
	}
	if counts[ExceptionDuplicateBankRef] != 1 {
		t.Fatalf("duplicate bank ref count: %+v", counts)
	}
	if len(plan.Matches) != 0 {
		t.Fatalf("no clean matches expected: %+v", plan.Matches)
	}
}

// TestPlanReconStatementLateIsPending: a settlement whose statement leg has
// not arrived raises nothing and matches nothing — the next run completes it.
func TestPlanReconStatementLateIsPending(t *testing.T) {
	plan := planRecon(reconInput{
		Notes: []DebitNote{openNote("n1", "NPA-2026-000020", 5000, "2026-03-01")},
		Settlements: []Settlement{{
			SettlementID: "s1", DebitNoteID: "n1", BankReference: "BR-LATE",
			AmountMinor: 5000, Currency: "USD", PayerRef: "ACME", ValueDate: "2026-02-01",
		}},
		AsOf: reconAt("2026-02-10"),
	})
	if len(plan.Matches) != 0 || len(plan.Exceptions) != 0 {
		t.Fatalf("late statement must be pending, plan: %+v", plan)
	}
}

// TestPlanReconDocumentNumberIdentification: a settlement that names the
// debit note's document number as its bank reference identifies the note.
func TestPlanReconDocumentNumberIdentification(t *testing.T) {
	plan := planRecon(reconInput{
		Notes: []DebitNote{openNote("n1", "NPA-2026-000030", 5000, "2026-03-01")},
		Settlements: []Settlement{{
			SettlementID: "s1", BankReference: "NPA-2026-000030",
			AmountMinor: 5000, Currency: "USD", PayerRef: "ACME", ValueDate: "2026-02-01",
		}},
		Lines: []statementLeg{{
			StatementID: "st1", LineNo: 1, BankReference: "NPA-2026-000030",
			ValueDate: "2026-02-01", AmountMinor: 5000, Currency: "USD",
		}},
		AsOf: reconAt("2026-02-10"),
	})
	if len(plan.Matches) != 1 || plan.Matches[0].DebitNoteID != "n1" {
		t.Fatalf("document-number identification failed: %+v", plan)
	}
}
