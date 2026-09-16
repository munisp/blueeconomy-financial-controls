package cvff

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// State is the lifecycle state of a CVFF disbursement application.
type State string

const (
	StateSubmitted              State = "SUBMITTED"
	StateUnderwritingPrimary    State = "UNDERWRITING_PRIMARY"
	StateUnderwritingSecondary  State = "UNDERWRITING_SECONDARY"
	StateUnderwritingTertiary   State = "UNDERWRITING_TERTIARY"
	StateNIMASAApproval         State = "NIMASA_APPROVAL"
	StateBankConfirmation       State = "BANK_CONFIRMATION"
	StateDisbursementPending    State = "DISBURSEMENT_PENDING"
	StateDisbursed              State = "DISBURSED"
	StateAudited                State = "AUDITED"
	StateRejected               State = "REJECTED"
	StateReconciliationRequired State = "RECONCILIATION_REQUIRED"
)

// Role identifies one party in the four-party CVFF approval chain. Each role
// approves only its own lifecycle part.
type Role string

const (
	RoleUnderwriterPrimary   Role = "UNDERWRITER_PRIMARY"
	RoleUnderwriterSecondary Role = "UNDERWRITER_SECONDARY"
	RoleUnderwriterTertiary  Role = "UNDERWRITER_TERTIARY"
	RoleNIMASAApprover       Role = "NIMASA_APPROVER"
	RoleReceivingBank        Role = "RECEIVING_BANK"
	RoleBeneficiary          Role = "BENEFICIARY"
	// RoleReconciliationOfficer is the only actor allowed to resolve the
	// fail-closed RECONCILIATION_REQUIRED branch. It is deliberately outside
	// the four-party chain: no chain principal may clear its own deadlock.
	RoleReconciliationOfficer Role = "RECONCILIATION_OFFICER"
)

// Decision is the outcome a party records for its lifecycle part.
type Decision string

const (
	DecisionApprove Decision = "APPROVE"
	DecisionReject  Decision = "REJECT"
)

var (
	ErrNotFound              = errors.New("cvff application not found")
	ErrConflict              = errors.New("cvff application changed concurrently")
	ErrInvalidState          = errors.New("invalid cvff application state transition")
	ErrRoleSeparation        = errors.New("principal must not hold more than one cvff role on one application")
	ErrRoleNotAssigned       = errors.New("principal is not assigned the required cvff role")
	ErrSequenceViolation     = errors.New("cvff decision is out of lifecycle sequence")
	ErrTerminalState         = errors.New("cvff application is in a terminal state")
	ErrRoleAssignmentInvalid = errors.New("cvff role assignments must cover each role exactly once")
	ErrResolutionInvalid     = errors.New("unknown cvff reconciliation resolution")
)

// ReconciliationResolution is the operator decision that closes the
// fail-closed RECONCILIATION_REQUIRED branch.
type ReconciliationResolution string

const (
	// ResolutionResumeDisbursement returns the application to
	// DISBURSEMENT_PENDING so the rail retries after the contradictory or
	// missing evidence has been remediated (e.g. the CBN rate was posted).
	ResolutionResumeDisbursement ReconciliationResolution = "RESUME_DISBURSEMENT"
	// ResolutionReject closes the application as REJECTED when the
	// reconciliation evidence shows the disbursement must never proceed.
	ResolutionReject ReconciliationResolution = "REJECT"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Application is a CVFF disbursement request moving through the four-party
// approval chain.
type Application struct {
	ApplicationID string    `json:"application_id"`
	ExternalRef   string    `json:"external_ref"`
	BeneficiaryID string    `json:"beneficiary_id"`
	Amount        uint64    `json:"amount"`
	Currency      string    `json:"currency"`
	State         State     `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Version       int64     `json:"version"`
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

func ValidateIdentifier(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 256 || !idPattern.MatchString(value) {
		return fmt.Errorf("%s is not canonical approved identifier text", name)
	}
	return nil
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
		"beneficiary_id": application.BeneficiaryID,
	} {
		if err := ValidateIdentifier(name, value); err != nil {
			return err
		}
	}
	if application.Amount == 0 {
		return errors.New("amount must be non-zero")
	}
	return ValidateCurrency(application.Currency)
}

// ValidateRoleAssignments enforces strict separation of duties: every role is
// held by exactly one principal and no principal holds two roles.
func ValidateRoleAssignments(assignments map[Role]string) error {
	for _, role := range []Role{RoleUnderwriterPrimary, RoleUnderwriterSecondary, RoleUnderwriterTertiary, RoleNIMASAApprover, RoleReceivingBank, RoleBeneficiary} {
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

// RequiredRoleForState maps each decision state to the single role allowed
// to decide it; ok is false for non-decision states.
func RequiredRoleForState(state State) (Role, bool) {
	return requiredRole(state)
}

// requiredRole maps each decision state to the single role allowed to decide it.
func requiredRole(state State) (Role, bool) {
	switch state {
	case StateUnderwritingPrimary:
		return RoleUnderwriterPrimary, true
	case StateUnderwritingSecondary:
		return RoleUnderwriterSecondary, true
	case StateUnderwritingTertiary:
		return RoleUnderwriterTertiary, true
	case StateNIMASAApproval:
		return RoleNIMASAApprover, true
	case StateBankConfirmation:
		return RoleReceivingBank, true
	case StateDisbursementPending:
		return RoleBeneficiary, true
	default:
		return "", false
	}
}

// approvedSuccessor maps each decision state to the state reached on approval.
func approvedSuccessor(state State) (State, bool) {
	switch state {
	case StateSubmitted:
		return StateUnderwritingPrimary, true
	case StateUnderwritingPrimary:
		return StateUnderwritingSecondary, true
	case StateUnderwritingSecondary:
		return StateUnderwritingTertiary, true
	case StateUnderwritingTertiary:
		return StateNIMASAApproval, true
	case StateNIMASAApproval:
		return StateBankConfirmation, true
	case StateBankConfirmation:
		return StateDisbursementPending, true
	case StateDisbursementPending:
		return StateDisbursed, true
	case StateDisbursed:
		return StateAudited, true
	default:
		return "", false
	}
}

// Terminal reports whether no further decision or transition is possible.
func (state State) Terminal() bool {
	return state == StateRejected || state == StateAudited
}

// BeginUnderwriting moves a submitted application into the underwriting chain.
func BeginUnderwriting(current Application) (Application, error) {
	if current.State.Terminal() {
		return Application{}, ErrTerminalState
	}
	if current.State != StateSubmitted {
		return Application{}, ErrSequenceViolation
	}
	next, _ := approvedSuccessor(StateSubmitted)
	current.State = next
	return current, nil
}

// ApplyDecision records one party's decision. The decision is valid only when
// the principal holds the role required for the application's current state and
// the chain is executed in sequence. Any rejection moves the application to the
// fail-closed REJECTED state.
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
		current.State = StateRejected
		approval.ToState = StateRejected
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

// MarkAudited closes the lifecycle for a disbursed application.
func MarkAudited(current Application) (Application, error) {
	if current.State.Terminal() {
		return Application{}, ErrTerminalState
	}
	if current.State != StateDisbursed {
		return Application{}, ErrSequenceViolation
	}
	current.State = StateAudited
	return current, nil
}

// RequireReconciliation moves a non-terminal application into the fail-closed
// RECONCILIATION_REQUIRED branch after contradictory or missing evidence.
func RequireReconciliation(current Application) (Application, error) {
	if current.State.Terminal() {
		return Application{}, ErrTerminalState
	}
	current.State = StateReconciliationRequired
	return current, nil
}

// ResolveReconciliation moves an application out of RECONCILIATION_REQUIRED:
// RESUME_DISBURSEMENT returns it to DISBURSEMENT_PENDING for a remediated
// retry, REJECT closes it as REJECTED. Any other state or resolution fails
// closed; the branch is never silently abandoned and never auto-resolved.
func ResolveReconciliation(current Application, resolution ReconciliationResolution) (Application, error) {
	return ResolveReconciliationTo(current, resolution, StateDisbursementPending)
}

// resumableStates are the only states a reconciliation resolution may return
// an application to: every party-decision wait state and the disbursement
// gate. Resuming into any other state (e.g. AUDITED) is rejected fail-closed.
var resumableStates = map[State]struct{}{
	StateUnderwritingPrimary:   {},
	StateUnderwritingSecondary: {},
	StateUnderwritingTertiary:  {},
	StateNIMASAApproval:        {},
	StateBankConfirmation:      {},
	StateDisbursementPending:   {},
}

// ResolveReconciliationTo is ResolveReconciliation with an explicit resume
// target bound by the workflow when it parked: a decision-stage failure
// (duplicate/early signal, role rejection) resumes into the interrupted stage
// so the correct party's decision can be recorded, never silently skipped.
func ResolveReconciliationTo(current Application, resolution ReconciliationResolution, resumeTarget State) (Application, error) {
	if current.State != StateReconciliationRequired {
		return Application{}, ErrSequenceViolation
	}
	switch resolution {
	case ResolutionResumeDisbursement:
		if _, ok := resumableStates[resumeTarget]; !ok {
			return Application{}, fmt.Errorf("%w: resume target %s is not a party-decision or disbursement state", ErrResolutionInvalid, resumeTarget)
		}
		current.State = resumeTarget
	case ResolutionReject:
		current.State = StateRejected
	default:
		return Application{}, ErrResolutionInvalid
	}
	return current, nil
}

// underwritingTier identifies one PLI consortium underwriting layer.
type UnderwritingTier string

const (
	TierPrimary   UnderwritingTier = "PRIMARY"
	TierSecondary UnderwritingTier = "SECONDARY"
	TierTertiary  UnderwritingTier = "TERTIARY"
)

// SLABusinessDays is the approved per-tier underwriting service level:
// PRIMARY 5 business days, SECONDARY 3, TERTIARY 2.
func SLABusinessDays(tier UnderwritingTier) (int, error) {
	switch tier {
	case TierPrimary:
		return 5, nil
	case TierSecondary:
		return 3, nil
	case TierTertiary:
		return 2, nil
	default:
		return 0, fmt.Errorf("unknown underwriting tier %q", tier)
	}
}

// TierForState maps an underwriting sub-state to its consortium tier.
func TierForState(state State) (UnderwritingTier, bool) {
	switch state {
	case StateUnderwritingPrimary:
		return TierPrimary, true
	case StateUnderwritingSecondary:
		return TierSecondary, true
	case StateUnderwritingTertiary:
		return TierTertiary, true
	default:
		return "", false
	}
}

// SLADeadline returns the instant a tier decision is due: the entered time plus
// the tier's business days, skipping Saturdays and Sundays.
func SLADeadline(tier UnderwritingTier, entered time.Time) (time.Time, error) {
	days, err := SLABusinessDays(tier)
	if err != nil {
		return time.Time{}, err
	}
	deadline := entered
	for added := 0; added < days; {
		deadline = deadline.Add(24 * time.Hour)
		if deadline.Weekday() != time.Saturday && deadline.Weekday() != time.Sunday {
			added++
		}
	}
	return deadline, nil
}

// SLAExpired reports whether the tier decision is overdue at the given time.
func SLAExpired(tier UnderwritingTier, entered, now time.Time) (bool, error) {
	deadline, err := SLADeadline(tier, entered)
	if err != nil {
		return false, err
	}
	return now.After(deadline), nil
}
