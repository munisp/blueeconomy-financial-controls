package reconciliation

import (
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
)

func testIntent(state intent.State) intent.Intent {
	return intent.Intent{
		CreateRequest: intent.CreateRequest{
			IntentID:        "intent-001",
			ExternalRef:     "external-001",
			DebitAccountID:  "debit-001",
			CreditAccountID: "credit-001",
			Amount:          1000,
			Ledger:          1,
			Code:            1,
			Currency:        "NGN",
			Maker:           "maker-001",
		},
		State: state,
	}
}

func testStatement(status string, amount uint64) StatementEntry {
	return StatementEntry{
		ExternalRef: "external-001",
		ProviderRef: "provider-001",
		Amount:      amount,
		Currency:    "NGN",
		Status:      status,
		ValueDate:   time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
	}
}

func TestReconcilePostedMatch(t *testing.T) {
	report, err := Reconcile([]intent.Intent{testIntent(intent.StatePosted)}, []StatementEntry{testStatement(StatementStatusSettled, 1000)}, []byte(`{"source":"approved-statement"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReconciliationOK || report.Matched != 1 || len(report.Findings) != 0 {
		t.Fatalf("unexpected clean report: %+v", report)
	}
}

func TestReconcileReportsMissingAndUnexpectedEntries(t *testing.T) {
	missing, err := Reconcile([]intent.Intent{testIntent(intent.StatePosted)}, nil, []byte("missing"))
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Findings) != 1 || missing.Findings[0].Kind != FindingMissingStatement {
		t.Fatalf("unexpected missing report: %+v", missing)
	}

	unexpectedStatement := testStatement(StatementStatusSettled, 1000)
	unexpectedStatement.ExternalRef = "external-unexpected"
	unexpected, err := Reconcile(nil, []StatementEntry{unexpectedStatement}, []byte("unexpected"))
	if err != nil {
		t.Fatal(err)
	}
	if len(unexpected.Findings) != 1 || unexpected.Findings[0].Kind != FindingUnexpectedEntry {
		t.Fatalf("unexpected extra-entry report: %+v", unexpected)
	}
}

func TestReconcileReportsAmountMismatch(t *testing.T) {
	report, err := Reconcile([]intent.Intent{testIntent(intent.StatePosted)}, []StatementEntry{testStatement(StatementStatusSettled, 999)}, []byte("mismatch"))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconciliationOK || len(report.Findings) != 1 || report.Findings[0].Kind != FindingAmountMismatch {
		t.Fatalf("unexpected mismatch report: %+v", report)
	}
}

func TestReconcileVoidedMatch(t *testing.T) {
	report, err := Reconcile([]intent.Intent{testIntent(intent.StateVoided)}, []StatementEntry{testStatement(StatementStatusVoided, 1000)}, []byte("voided"))
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReconciliationOK || report.Matched != 1 {
		t.Fatalf("unexpected void report: %+v", report)
	}
}
