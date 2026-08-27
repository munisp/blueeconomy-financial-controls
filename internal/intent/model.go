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
	default:
		return false
	}
}
