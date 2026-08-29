package intent

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type State string

const (
	StateDraft                  State = "DRAFT"
	StateApproved               State = "APPROVED"
	StateReservationRequested   State = "RESERVATION_REQUESTED"
	StateReserved               State = "RESERVED"
	StatePosted                 State = "POSTED"
	StateVoided                 State = "VOIDED"
	StateAmbiguous              State = "AMBIGUOUS"
	StateReconciliationRequired State = "RECONCILIATION_REQUIRED"
)

var (
	ErrNotFound          = errors.New("financial intent not found")
	ErrConflict          = errors.New("financial intent changed concurrently")
	ErrInvalidState      = errors.New("invalid financial intent state transition")
	ErrMakerChecker      = errors.New("maker and checker must be distinct")
	ErrImmutableConflict = errors.New("financial intent immutable fields conflict")
	// ErrInvalidResolution marks an officer disposition outside the approved
	// AMBIGUOUS resolutions.
	ErrInvalidResolution = errors.New("resolution must be RECONCILE or VOID")
	// ErrResolutionRejected marks an officer disposition contradicted by
	// observed ledger evidence (e.g. VOID when the funds were posted).
	ErrResolutionRejected = errors.New("resolution conflicts with observed ledger evidence")
)

// Resolution is the officer disposition of an AMBIGUOUS intent.
type Resolution string

const (
	// ResolutionReconcile returns the intent to RECONCILIATION_REQUIRED so
	// the evidence-driven reconciler re-examines it.
	ResolutionReconcile Resolution = "RECONCILE"
	// ResolutionVoid closes the intent VOIDED; any outstanding reservation
	// is compensated in the ledger before the state is recorded.
	ResolutionVoid Resolution = "VOID"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type CreateRequest struct {
	IntentID        string `json:"intent_id"`
	ExternalRef     string `json:"external_ref"`
	DebitAccountID  string `json:"debit_account_id"`
	CreditAccountID string `json:"credit_account_id"`
	Amount          uint64 `json:"amount"`
	Ledger          uint32 `json:"ledger"`
	Code            uint16 `json:"code"`
	Currency        string `json:"currency"`
	Maker           string `json:"maker"`
}

type Intent struct {
	CreateRequest
	State     State     `json:"state"`
	Checker   *string   `json:"checker,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int64     `json:"version"`
}

func (request CreateRequest) Validate() error {
	for name, value := range map[string]string{
		"intent_id": request.IntentID, "external_ref": request.ExternalRef,
		"debit_account_id": request.DebitAccountID, "credit_account_id": request.CreditAccountID,
		"currency": request.Currency, "maker": request.Maker,
	} {
		if value == "" || strings.TrimSpace(value) != value || len(value) > 256 || !idPattern.MatchString(value) {
			return fmt.Errorf("%s is not canonical approved identifier text", name)
		}
	}
	if request.DebitAccountID == request.CreditAccountID {
		return errors.New("debit and credit accounts must differ")
	}
	if request.Amount == 0 {
		return errors.New("amount must be non-zero")
	}
	if request.Ledger == 0 || request.Code == 0 {
		return errors.New("ledger and code must be non-zero")
	}
	if request.Currency != "NGN" && request.Currency != "USD" {
		return fmt.Errorf("currency %q is not an approved ledger currency (NGN, USD)", request.Currency)
	}
	return nil
}

func (intent Intent) Matches(request CreateRequest) bool {
	return intent.IntentID == request.IntentID && intent.ExternalRef == request.ExternalRef &&
		intent.DebitAccountID == request.DebitAccountID && intent.CreditAccountID == request.CreditAccountID &&
		intent.Amount == request.Amount && intent.Ledger == request.Ledger && intent.Code == request.Code &&
		intent.Currency == request.Currency && intent.Maker == request.Maker
}

func Approve(current Intent, checker string) (Intent, error) {
	if current.State != StateDraft {
		return Intent{}, ErrInvalidState
	}
	if strings.TrimSpace(checker) == "" || checker == current.Maker {
		return Intent{}, ErrMakerChecker
	}
	current.State = StateApproved
	current.Checker = &checker
	return current, nil
}

// ResolveAmbiguous applies the officer disposition of an AMBIGUOUS intent.
// AMBIGUOUS is produced only by the evidence-driven reconciler when the
// observed TigerBeetle records are contradictory or absent; it is not a
// terminal state — a financial controller must resolve it. The officer
// identity is the verified token subject recorded for audit.
func ResolveAmbiguous(current Intent, expectedVersion int64, officer string, resolution Resolution) (Intent, error) {
	if current.State != StateAmbiguous {
		return Intent{}, ErrInvalidState
	}
	if current.Version != expectedVersion {
		return Intent{}, ErrConflict
	}
	if officer == "" || strings.TrimSpace(officer) != officer || len(officer) > 256 || !idPattern.MatchString(officer) {
		return Intent{}, errors.New("officer is not canonical approved identifier text")
	}
	var next State
	switch resolution {
	case ResolutionReconcile:
		next = StateReconciliationRequired
	case ResolutionVoid:
		next = StateVoided
	default:
		return Intent{}, ErrInvalidResolution
	}
	if !ValidOperationalTransition(current.State, next) {
		return Intent{}, ErrInvalidState
	}
	current.State = next
	return current, nil
}

func ValidOperationalTransition(current, next State) bool {
	switch current {
	case StateApproved:
		return next == StateReservationRequested
	case StateReservationRequested:
		return next == StateReserved || next == StateAmbiguous || next == StateReconciliationRequired
	case StateReserved:
		return next == StatePosted || next == StateVoided || next == StateAmbiguous || next == StateReconciliationRequired
	case StateReconciliationRequired:
		return next == StateReserved || next == StatePosted || next == StateVoided || next == StateAmbiguous
	case StateAmbiguous:
		// Officer-gated resolutions only: back to evidence-driven
		// reconciliation, or closed VOIDED after compensating ledger handling.
		return next == StateReconciliationRequired || next == StateVoided
	default:
		return false
	}
}
