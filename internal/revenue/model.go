// Package revenue implements the W-FEAT-7 revenue-assurance chain for the
// BlueEconomy platform: debit-note issuance against tariff assessments, the
// TSA (Treasury Single Account) remittance split over versioned,
// effective-dated rules, and three-way reconciliation across the
// assessment/debit-note ledger, settlement/collection records (the
// materialized mirror of the TigerBeetle ledger entries) and signed TSA/bank
// statement ingest.
//
// Doctrine mirrors the tariff engine: split rules are DATA (never code
// constants), money is integer minor units, computations are deterministic
// in their asOf window, replay is idempotency-key safe, dual control is
// enforced in SQL, and every artifact crossing a trust boundary is sealed
// as envelope v1.0 (FHIR R4 Bundle + JWS-EdDSA over RFC 8785 JCS).
package revenue

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound reports a missing record.
	ErrNotFound = errors.New("revenue record not found")
	// ErrIdempotencyConflict reports a key replay against a different request.
	ErrIdempotencyConflict = errors.New("idempotency key is bound to a different request")
	// ErrInvalidTransition rejects a state move outside the lifecycle.
	ErrInvalidTransition = errors.New("debit-note state transition is not permitted")
	// ErrMakerChecker rejects self-approval (maker == checker/actor).
	ErrMakerChecker = errors.New("maker and checker must be distinct")
	// ErrSplitIncomplete rejects a rule window that does not sum to 10000 bps.
	ErrSplitIncomplete = errors.New("active split rules do not sum to 10000 bps")
	// ErrDuplicateBankRef reports a conflicting bank reference at ingest.
	ErrDuplicateBankRef = errors.New("bank reference already ingested with different content")
)

// Agencies (CBN appears only as the TSA host bank, never as a charging agency).
const (
	AgencyNPA    = "NPA"
	AgencyNIMASA = "NIMASA"
	AgencyNIWA   = "NIWA"
	AgencyFMMBE  = "FMMBE"
)

// Debit-note lifecycle states. DRAFT is the pre-issuance maker state.
const (
	StateDraft     = "DRAFT"
	StateIssued    = "ISSUED"
	StateAcked     = "ACKED"
	StateDisputed  = "DISPUTED"
	StateSettled   = "SETTLED"
	StateCancelled = "CANCELLED"
)

// TSA split beneficiaries.
const (
	BeneficiaryAgencyRetained = "AGENCY_RETAINED"
	BeneficiaryFGN            = "FGN_CONSOLIDATED"
	BeneficiaryCVFFFiduciary  = "CVFF_FIDUCIARY"
)

// Rule states (versioned data lifecycle, mirrors tariff).
const (
	RuleDraft   = "DRAFT"
	RuleActive  = "ACTIVE"
	RuleRetired = "RETIRED"
)

// Recon exception classes.
const (
	ExceptionUnmatchedAssessment = "UNMATCHED_ASSESSMENT"
	ExceptionUnmatchedSettlement = "UNMATCHED_SETTLEMENT"
	ExceptionUnmatchedStatement  = "UNMATCHED_STATEMENT"
	ExceptionAmountMismatch      = "AMOUNT_MISMATCH"
	ExceptionDuplicateBankRef    = "DUPLICATE_BANK_REF"
)

// Exception states.
const (
	ExceptionOpen     = "OPEN"
	ExceptionResolved = "RESOLVED"
)

// Envelope artifact kinds.
const (
	ArtifactDebitNote        = "DEBIT_NOTE"
	ArtifactRemittanceAdvice = "REMITTANCE_ADVICE"
	ArtifactBankStatement    = "BANK_STATEMENT"
)

// reconEngineActor is the system identity recorded on recon-driven
// transitions and raised exceptions.
const reconEngineActor = "recon-engine"

// IssueRequest is one debit-note issuance command against an assessment.
type IssueRequest struct {
	AssessmentID  string `json:"assessmentId"`
	DueDate       string `json:"dueDate"` // YYYY-MM-DD
	EffectiveDate string `json:"effectiveDate,omitempty"`
}

// Validate fails closed on a malformed issuance request.
func (request IssueRequest) Validate() error {
	if strings.TrimSpace(request.AssessmentID) == "" || len(request.AssessmentID) > 128 {
		return errors.New("assessmentId is required")
	}
	if _, err := time.Parse("2006-01-02", request.DueDate); err != nil {
		return errors.New("dueDate must be YYYY-MM-DD")
	}
	if request.EffectiveDate != "" {
		if _, err := time.Parse("2006-01-02", request.EffectiveDate); err != nil {
			return errors.New("effectiveDate must be YYYY-MM-DD")
		}
	}
	return nil
}

// DebitNoteLine is one charged line carried from the assessment.
type DebitNoteLine struct {
	LineNo             int    `json:"lineNo"`
	Instrument         string `json:"instrument"`
	Agency             string `json:"agency"`
	AmountMinor        int64  `json:"amountMinor"`
	Currency           string `json:"currency"`
	StatutoryReference string `json:"statutoryReference,omitempty"`
}

// DebitNote is the payable instrument. The envelope fields are nil until
// issuance seals the signed representation.
type DebitNote struct {
	DebitNoteID    string          `json:"debitNoteId"`
	AssessmentID   string          `json:"assessmentId"`
	Agency         string          `json:"agency"`
	EntityRef      string          `json:"entityRef"`
	DocumentNumber string          `json:"documentNumber,omitempty"`
	AmountUSDMinor int64           `json:"amountUsdMinor"`
	AmountNGNMinor int64           `json:"amountNgnMinor"`
	EffectiveDate  string          `json:"effectiveDate"`
	DueDate        string          `json:"dueDate"`
	State          string          `json:"state"`
	CancelReason   string          `json:"cancelReason,omitempty"`
	Lines          []DebitNoteLine `json:"lines"`
	Maker          string          `json:"maker"`
	Checker        string          `json:"checker,omitempty"`
	CorrelationID  string          `json:"correlationId"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// Transition is one audited lifecycle move.
type Transition struct {
	TransitionID string    `json:"transitionId"`
	DebitNoteID  string    `json:"debitNoteId"`
	FromState    string    `json:"fromState"`
	ToState      string    `json:"toState"`
	Reason       string    `json:"reason,omitempty"`
	Actor        string    `json:"actor"`
	Approver     string    `json:"approver,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// SplitRuleRow is one versioned, effective-dated TSA split rule.
type SplitRuleRow struct {
	RuleID             string
	RevenueLine        string
	Agency             string
	Beneficiary        string
	ShareBps           int64
	StatutoryReference string
	Provisional        bool
	EffectiveFrom      time.Time
	EffectiveTo        *time.Time
	State              string
	Maker              string
	Checker            string
}

// Allocation is one deterministic output leg of a split.
type Allocation struct {
	Beneficiary string `json:"beneficiary"`
	ShareBps    int64  `json:"shareBps"`
	AmountMinor int64  `json:"amountMinor"`
}

// RemittanceAdvice is the signed record of one settlement's split.
type RemittanceAdvice struct {
	AdviceID     string       `json:"adviceId"`
	SettlementID string       `json:"settlementId"`
	RevenueLine  string       `json:"revenueLine"`
	Agency       string       `json:"agency"`
	AsOf         string       `json:"asOf"`
	AmountMinor  int64        `json:"amountMinor"`
	Currency     string       `json:"currency"`
	Allocations  []Allocation `json:"allocations"`
	CreatedBy    string       `json:"createdBy"`
	CreatedAt    time.Time    `json:"createdAt"`
}

// Settlement is one collection record (mirror of a TigerBeetle ledger entry).
type Settlement struct {
	SettlementID  string    `json:"settlementId"`
	DebitNoteID   string    `json:"debitNoteId,omitempty"`
	BankReference string    `json:"bankReference"`
	TBTransferID  string    `json:"tbTransferId,omitempty"`
	AmountMinor   int64     `json:"amountMinor"`
	Currency      string    `json:"currency"`
	PayerRef      string    `json:"payerRef"`
	ValueDate     string    `json:"valueDate"`
	RecordedBy    string    `json:"recordedBy"`
	CreatedAt     time.Time `json:"createdAt"`
}

// StatementRequest is the structured statement carried inside a signed
// ingest envelope (the JCS-canonical payload).
type StatementRequest struct {
	Bank         string          `json:"bank"`
	AccountRef   string          `json:"accountRef"`
	StatementRef string          `json:"statementRef"`
	PeriodStart  string          `json:"periodStart"`
	PeriodEnd    string          `json:"periodEnd"`
	Lines        []StatementLine `json:"lines"`
}

// Validate fails closed on a malformed statement.
func (request StatementRequest) Validate() error {
	if strings.TrimSpace(request.Bank) == "" || len(request.Bank) > 64 {
		return errors.New("bank is required")
	}
	if strings.TrimSpace(request.AccountRef) == "" || len(request.AccountRef) > 128 {
		return errors.New("accountRef is required")
	}
	if strings.TrimSpace(request.StatementRef) == "" || len(request.StatementRef) > 128 {
		return errors.New("statementRef is required")
	}
	start, err := time.Parse("2006-01-02", request.PeriodStart)
	if err != nil {
		return errors.New("periodStart must be YYYY-MM-DD")
	}
	end, err := time.Parse("2006-01-02", request.PeriodEnd)
	if err != nil {
		return errors.New("periodEnd must be YYYY-MM-DD")
	}
	if end.Before(start) {
		return errors.New("periodEnd must not precede periodStart")
	}
	if len(request.Lines) == 0 || len(request.Lines) > 10000 {
		return errors.New("statement must carry 1..10000 lines")
	}
	for index, line := range request.Lines {
		if err := line.Validate(); err != nil {
			return fmt.Errorf("line %d: %w", index+1, err)
		}
	}
	return nil
}

// StatementLine is one structured bank statement entry.
type StatementLine struct {
	BankReference string `json:"bankReference"`
	ValueDate     string `json:"valueDate"`
	Direction     string `json:"direction"` // CREDIT | DEBIT
	AmountMinor   int64  `json:"amountMinor"`
	Currency      string `json:"currency"`
	Narrative     string `json:"narrative,omitempty"`
}

// Validate fails closed on a malformed statement line.
func (line StatementLine) Validate() error {
	if strings.TrimSpace(line.BankReference) == "" || len(line.BankReference) > 128 {
		return errors.New("bankReference is required")
	}
	if _, err := time.Parse("2006-01-02", line.ValueDate); err != nil {
		return errors.New("valueDate must be YYYY-MM-DD")
	}
	if line.Direction != "CREDIT" && line.Direction != "DEBIT" {
		return errors.New("direction must be CREDIT or DEBIT")
	}
	if line.AmountMinor <= 0 {
		return errors.New("amountMinor must be positive")
	}
	if line.Currency != "USD" && line.Currency != "NGN" {
		return errors.New("currency must be USD or NGN")
	}
	if len(line.Narrative) > 512 {
		return errors.New("narrative is too long")
	}
	return nil
}

// Match is one completed three-way reconciliation.
type Match struct {
	MatchID         string    `json:"matchId"`
	RunID           string    `json:"runId"`
	DebitNoteID     string    `json:"debitNoteId"`
	SettlementID    string    `json:"settlementId"`
	StatementID     string    `json:"statementId"`
	StatementLineNo int       `json:"statementLineNo"`
	AmountMinor     int64     `json:"amountMinor"`
	Currency        string    `json:"currency"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Exception is one reconciliation exception-queue entry.
type Exception struct {
	ExceptionID       string     `json:"exceptionId"`
	RunID             string     `json:"runId"`
	Class             string     `json:"class"`
	State             string     `json:"state"`
	DebitNoteID       string     `json:"debitNoteId,omitempty"`
	SettlementID      string     `json:"settlementId,omitempty"`
	StatementID       string     `json:"statementId,omitempty"`
	StatementLineNo   int        `json:"statementLineNo,omitempty"`
	ExpectedMinor     *int64     `json:"expectedAmountMinor,omitempty"`
	ActualMinor       *int64     `json:"actualAmountMinor,omitempty"`
	Currency          string     `json:"currency,omitempty"`
	Detail            string     `json:"detail"`
	Resolver          string     `json:"resolver,omitempty"`
	ResolutionNote    string     `json:"resolutionNote,omitempty"`
	ResolvedAt        *time.Time `json:"resolvedAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
}

// RunSummary reports one completed recon batch.
type RunSummary struct {
	RunID          string    `json:"runId"`
	State          string    `json:"state"`
	MatchedCount   int       `json:"matchedCount"`
	ExceptionCount int       `json:"exceptionCount"`
	StartedBy      string    `json:"startedBy"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt"`
}

// validTransition reports whether from->to is inside the lifecycle.
func validTransition(from, to string) bool {
	switch from {
	case StateDraft:
		return to == StateIssued || to == StateCancelled
	case StateIssued:
		return to == StateAcked || to == StateDisputed || to == StateSettled || to == StateCancelled
	case StateAcked:
		return to == StateSettled || to == StateDisputed
	case StateDisputed:
		return to == StateAcked || to == StateSettled || to == StateCancelled
	}
	return false
}
