package reconciliation

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
)

const (
	StatementStatusSettled = "SETTLED"
	StatementStatusVoided  = "VOIDED"
)

type StatementEntry struct {
	ExternalRef string    `json:"external_ref"`
	ProviderRef string    `json:"provider_ref"`
	Amount      uint64    `json:"amount"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	ValueDate   time.Time `json:"value_date"`
}

type FindingKind string

const (
	FindingMissingStatement FindingKind = "MISSING_STATEMENT"
	FindingUnexpectedEntry  FindingKind = "UNEXPECTED_ENTRY"
	FindingDuplicateEntry   FindingKind = "DUPLICATE_STATEMENT"
	FindingStatusMismatch   FindingKind = "STATUS_MISMATCH"
	FindingAmountMismatch   FindingKind = "AMOUNT_MISMATCH"
	FindingCurrencyMismatch FindingKind = "CURRENCY_MISMATCH"
)

type Finding struct {
	Kind        FindingKind `json:"kind"`
	ExternalRef string      `json:"external_ref"`
	IntentID    string      `json:"intent_id,omitempty"`
	ProviderRef string      `json:"provider_ref,omitempty"`
	Detail      string      `json:"detail"`
}

type Report struct {
	GeneratedAt      time.Time `json:"generated_at"`
	InputSHA256      string    `json:"input_sha256"`
	IntentsExamined  int       `json:"intents_examined"`
	StatementsSeen   int       `json:"statements_seen"`
	Matched          int       `json:"matched"`
	Findings         []Finding `json:"findings"`
	ReconciliationOK bool      `json:"reconciliation_ok"`
}

func Reconcile(intents []intent.Intent, statements []StatementEntry, inputBytes []byte) (Report, error) {
	if len(intents) == 0 && len(statements) == 0 {
		return Report{}, errors.New("reconciliation inputs must not both be empty")
	}
	internalByRef := make(map[string]intent.Intent, len(intents))
	for _, item := range intents {
		if err := item.CreateRequest.Validate(); err != nil {
			return Report{}, fmt.Errorf("intent %q is invalid: %w", item.IntentID, err)
		}
		if _, exists := internalByRef[item.ExternalRef]; exists {
			return Report{}, fmt.Errorf("duplicate internal external_ref %q", item.ExternalRef)
		}
		internalByRef[item.ExternalRef] = item
	}
	statementByRef := make(map[string]StatementEntry, len(statements))
	findings := make([]Finding, 0)
	for _, statement := range statements {
		if statement.ExternalRef == "" || statement.ProviderRef == "" || statement.Amount == 0 || statement.Currency == "" {
			return Report{}, errors.New("statement entry has missing required identity or monetary field")
		}
		if statement.Status != StatementStatusSettled && statement.Status != StatementStatusVoided {
			return Report{}, fmt.Errorf("statement %q has unsupported status %q", statement.ExternalRef, statement.Status)
		}
		if _, exists := statementByRef[statement.ExternalRef]; exists {
			findings = append(findings, Finding{Kind: FindingDuplicateEntry, ExternalRef: statement.ExternalRef, Detail: "external statement contains duplicate external_ref"})
			continue
		}
		statementByRef[statement.ExternalRef] = statement
	}
	for externalRef, item := range internalByRef {
		statement, exists := statementByRef[externalRef]
		if !exists {
			findings = append(findings, Finding{Kind: FindingMissingStatement, ExternalRef: externalRef, IntentID: item.IntentID, Detail: "approved internal intent has no external statement entry"})
			continue
		}
		expectedStatus := StatementStatusSettled
		if item.State == intent.StateVoided {
			expectedStatus = StatementStatusVoided
		}
		if item.State != intent.StatePosted && item.State != intent.StateVoided {
			findings = append(findings, Finding{Kind: FindingStatusMismatch, ExternalRef: externalRef, IntentID: item.IntentID, ProviderRef: statement.ProviderRef, Detail: fmt.Sprintf("internal state %q is not reconcilable", item.State)})
		} else if statement.Status != expectedStatus {
			findings = append(findings, Finding{Kind: FindingStatusMismatch, ExternalRef: externalRef, IntentID: item.IntentID, ProviderRef: statement.ProviderRef, Detail: fmt.Sprintf("expected %q, received %q", expectedStatus, statement.Status)})
		}
		if statement.Amount != item.Amount {
			findings = append(findings, Finding{Kind: FindingAmountMismatch, ExternalRef: externalRef, IntentID: item.IntentID, ProviderRef: statement.ProviderRef, Detail: fmt.Sprintf("expected %d, received %d", item.Amount, statement.Amount)})
		}
		if statement.Currency != item.Currency {
			findings = append(findings, Finding{Kind: FindingCurrencyMismatch, ExternalRef: externalRef, IntentID: item.IntentID, ProviderRef: statement.ProviderRef, Detail: fmt.Sprintf("expected %q, received %q", item.Currency, statement.Currency)})
		}
		if len(findings) == 0 || findings[len(findings)-1].ExternalRef != externalRef {
			// The report count is derived below; no mutable match list is required.
		}
	}
	for externalRef, statement := range statementByRef {
		if _, exists := internalByRef[externalRef]; !exists {
			findings = append(findings, Finding{Kind: FindingUnexpectedEntry, ExternalRef: externalRef, ProviderRef: statement.ProviderRef, Detail: "external statement entry has no internal intent"})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].ExternalRef == findings[j].ExternalRef {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].ExternalRef < findings[j].ExternalRef
	})
	matched := 0
	for externalRef, item := range internalByRef {
		statement, exists := statementByRef[externalRef]
		if !exists || (item.State != intent.StatePosted && item.State != intent.StateVoided) || statement.Amount != item.Amount || statement.Currency != item.Currency {
			continue
		}
		expectedStatus := StatementStatusSettled
		if item.State == intent.StateVoided {
			expectedStatus = StatementStatusVoided
		}
		if statement.Status == expectedStatus {
			matched++
		}
	}
	return Report{
		GeneratedAt:      time.Now().UTC(),
		InputSHA256:      fmt.Sprintf("%x", sha256.Sum256(inputBytes)),
		IntentsExamined:  len(intents),
		StatementsSeen:   len(statements),
		Matched:          matched,
		Findings:         findings,
		ReconciliationOK: len(findings) == 0 && matched == len(intents),
	}, nil
}

func MarshalReport(report Report) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
