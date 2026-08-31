package tradefinance

import (
	"errors"
	"fmt"
	"time"
)

// Product is one of the four standardized starter trade-finance products
// (CamelONE 12-product catalogue, WP-6 starter set of 4).
type Product string

const (
	ProductImportLCFacilitation      Product = "IMPORT_LC_FACILITATION"
	ProductExportPreshipmentFinance  Product = "EXPORT_PRESHIPMENT_FINANCE"
	ProductInvoiceReceivablesFinance Product = "INVOICE_RECEIVABLES_FINANCE"
	ProductDutyDeferralGuarantee     Product = "DUTY_DEFERRAL_GUARANTEE"
)

// Products is the closed starter catalogue.
var Products = []Product{
	ProductImportLCFacilitation, ProductExportPreshipmentFinance,
	ProductInvoiceReceivablesFinance, ProductDutyDeferralGuarantee,
}

func validProduct(product Product) bool {
	for _, approved := range Products {
		if product == approved {
			return true
		}
	}
	return false
}

// State is the lifecycle state of a trade-finance application.
type State string

const (
	StateApplication         State = "APPLICATION"
	StateKYCConsentCheck     State = "KYC_CONSENT_CHECK"
	StateBankReview          State = "BANK_REVIEW"
	StateRegulatoryClearance State = "REGULATORY_CLEARANCE"
	StateApproved            State = "APPROVED"
	StateDisbursementPending State = "DISBURSEMENT_PENDING"
	StateDisbursed           State = "DISBURSED"
	StateSettled             State = "SETTLED"
	StateDeclined            State = "DECLINED"
)

// Role identifies one party in the trade-finance approval chain. The chain
// reuses the CVFF four-party machinery pattern: each role approves only its
// own lifecycle part and no principal holds two roles on one application.
type Role string

const (
	RoleBankKYCOfficer           Role = "BANK_KYC_OFFICER"
	RoleBankCreditOfficer        Role = "BANK_CREDIT_OFFICER"
	RoleCustomsComplianceOfficer Role = "CUSTOMS_COMPLIANCE_OFFICER"
	RoleBankTreasuryOfficer      Role = "BANK_TREASURY_OFFICER"
	// RoleTrader is the trader-side principal: it accepts the approved
	// facility terms (moving APPROVED→DISBURSEMENT_PENDING) and confirms
	// settlement (DISBURSED→SETTLED).
	RoleTrader Role = "TRADER"
)

// chainRoles is the assignment order; every role must be bound exactly once.
var chainRoles = []Role{
	RoleBankKYCOfficer, RoleBankCreditOfficer, RoleCustomsComplianceOfficer, RoleBankTreasuryOfficer, RoleTrader,
}

// Decision is the outcome a party records for its lifecycle part.
type Decision string

const (
	DecisionApprove Decision = "APPROVE"
	DecisionReject  Decision = "REJECT"
)

var (
	ErrNotFound              = errors.New("tradefinance application not found")
	ErrConflict              = errors.New("tradefinance application changed concurrently")
	ErrInvalidState          = errors.New("invalid tradefinance application state transition")
	ErrRoleSeparation        = errors.New("principal must not hold more than one tradefinance role on one application")
	ErrRoleNotAssigned       = errors.New("principal is not assigned the required tradefinance role")
	ErrSequenceViolation     = errors.New("tradefinance decision is out of lifecycle sequence")
	ErrTerminalState         = errors.New("tradefinance application is in a terminal state")
	ErrRoleAssignmentInvalid = errors.New("tradefinance role assignments must cover each role exactly once")
	ErrConsentRequired       = errors.New("KYC/consent check requires an active consent covering the trader and bank")
	ErrLedgerAccountsMissing = errors.New("facility and settlement ledger accounts are required before approval")
)

// Application is a trade-finance request moving through the approval chain.
type Application struct {
	ApplicationID       string    `json:"application_id"`
	ExternalRef         string    `json:"external_ref"`
	TraderID            string    `json:"trader_id"`
	BankID              string    `json:"bank_id"`
	ConsentID           string    `json:"consent_id"`
	Product             Product   `json:"product"`
	Amount              uint64    `json:"amount"`
	Currency            string    `json:"currency"`
	State               State     `json:"state"`
	FacilityAccountID   string    `json:"facility_account_id,omitempty"`
	SettlementAccountID string    `json:"settlement_account_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	Version             int64     `json:"version"`
}

// Approval is one immutable decision entry in the application audit trail.
type Approval struct {
	ApprovalID    string    `json:"approval_id"`
	ApplicationID string    `json:"application_id"`
	Role          Role      `json:"role"`
	PrincipalID   string    `json:"principal_id"`
	Decision      Decision  `json:"decision"`
	FromState     State     `json:"from_state"`
	ToState       State     `json:"to_state"`
	CreatedAt     time.Time `json:"created_at"`
}

// ValidateCurrency constrains monetary values to the approved ledger currencies.
func ValidateCurrency(currency string) error {
	switch currency {
	case "NGN", "USD":
		return nil
	default:
		return fmt.Errorf("currency %q is not an approved ledger currency (NGN, USD)", currency)
	}
}

func (application Application) Validate() error {
	for name, value := range map[string]string{
		"application_id": application.ApplicationID,
		"external_ref":   application.ExternalRef,
		"trader_id":      application.TraderID,
		"bank_id":        application.BankID,
		"consent_id":     application.ConsentID,
	} {
		if err := ValidateIdentifier(name, value); err != nil {
			return err
		}
	}
	if !validProduct(application.Product) {
		return fmt.Errorf("product %q is not in the standardized catalogue", application.Product)
	}
	if application.Amount == 0 {
		return errors.New("amount must be non-zero")
	}
	return ValidateCurrency(application.Currency)
}

// ValidateRoleAssignments enforces strict separation of duties: every role
// is held by exactly one principal and no principal holds two roles.
func ValidateRoleAssignments(assignments map[Role]string) error {
	for _, role := range chainRoles {
		principal, ok := assignments[role]
		if !ok {
			return fmt.Errorf("%w: missing %s", ErrRoleAssignmentInvalid, role)
		}
		if err := ValidateIdentifier("principal_id", principal); err != nil {
			return fmt.Errorf("%s: %w", role, err)
		}
	}
	seen := map[string]Role{}
	for role, principal := range assignments {
		if held, exists := seen[principal]; exists {
			return fmt.Errorf("%w: principal %q holds %s and %s", ErrRoleSeparation, principal, held, role)
		}
		seen[principal] = role
	}
	return nil
}

// requiredRole maps each decision state to the single role allowed to decide it.
func requiredRole(state State) (Role, bool) {
	switch state {
	case StateKYCConsentCheck:
		return RoleBankKYCOfficer, true
	case StateBankReview:
		return RoleBankCreditOfficer, true
	case StateRegulatoryClearance:
		return RoleCustomsComplianceOfficer, true
	case StateApproved:
		return RoleTrader, true
	case StateDisbursementPending:
		return RoleBankTreasuryOfficer, true
	case StateDisbursed:
		return RoleTrader, true
	default:
		return "", false
	}
}

// approvedSuccessor maps each state to the state reached on approval.
func approvedSuccessor(state State) (State, bool) {
	switch state {
	case StateApplication:
		return StateKYCConsentCheck, true
	case StateKYCConsentCheck:
		return StateBankReview, true
	case StateBankReview:
		return StateRegulatoryClearance, true
	case StateRegulatoryClearance:
		return StateApproved, true
	case StateApproved:
		return StateDisbursementPending, true
	case StateDisbursementPending:
		return StateDisbursed, true
	case StateDisbursed:
		return StateSettled, true
	default:
		return "", false
	}
}

// Terminal reports whether no further decision or transition is possible.
func (state State) Terminal() bool {
	return state == StateDeclined || state == StateSettled
}

// BeginReview moves a submitted application into the KYC/consent check.
func BeginReview(current Application) (Application, error) {
	if current.State.Terminal() {
		return Application{}, ErrTerminalState
	}
	if current.State != StateApplication {
		return Application{}, ErrSequenceViolation
	}
	next, _ := approvedSuccessor(StateApplication)
	current.State = next
	return current, nil
}

// ApplyDecision records one party's decision (CVFF machinery reuse). The
// decision is valid only when the principal holds the role required for the
// application's current state and the chain runs in sequence. Any rejection
// moves the application to the fail-closed DECLINED state.
func ApplyDecision(current Application, assignments map[Role]string, principalID string, decision Decision) (Application, Approval, error) {
	if current.State.Terminal() {
		return Application{}, Approval{}, ErrTerminalState
	}
	if decision != DecisionApprove && decision != DecisionReject {
		return Application{}, Approval{}, ErrInvalidState
	}
	required, decidable := requiredRole(current.State)
	if !decidable {
		return Application{}, Approval{}, ErrSequenceViolation
	}
	if err := ValidateIdentifier("principal_id", principalID); err != nil {
		return Application{}, Approval{}, err
	}
	assigned, ok := assignments[required]
	if !ok || assigned != principalID {
		return Application{}, Approval{}, fmt.Errorf("%w: state %s requires %s", ErrRoleNotAssigned, current.State, required)
	}
	for role, holder := range assignments {
		if holder == principalID && role != required {
			return Application{}, Approval{}, fmt.Errorf("%w: principal %q holds %s and %s", ErrRoleSeparation, principalID, required, role)
		}
	}
	approval := Approval{
		ApplicationID: current.ApplicationID,
		Role:          required,
		PrincipalID:   principalID,
		Decision:      decision,
		FromState:     current.State,
	}
	if decision == DecisionReject {
		current.State = StateDeclined
		approval.ToState = StateDeclined
		return current, approval, nil
	}
	next, ok := approvedSuccessor(current.State)
	if !ok {
		return Application{}, Approval{}, ErrInvalidState
	}
	current.State = next
	approval.ToState = next
	return current, approval, nil
}
