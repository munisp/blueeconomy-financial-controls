package cvff

import (
	"errors"
	"testing"
	"time"
)

func validApplication() Application {
	return Application{ApplicationID: "cvff-001", ExternalRef: "cvff-ref-001", BeneficiaryID: "beneficiary-001", Amount: 700_000_000, Currency: "USD", State: StateSubmitted}
}

func validAssignments() map[Role]string {
	return map[Role]string{
		RoleUnderwriterPrimary:   "kc-uw-primary",
		RoleUnderwriterSecondary: "kc-uw-secondary",
		RoleUnderwriterTertiary:  "kc-uw-tertiary",
		RoleNIMASAApprover:       "kc-nimasa",
		RoleReceivingBank:        "kc-bank",
		RoleBeneficiary:          "kc-beneficiary",
	}
}

func TestApplicationValidation(t *testing.T) {
	application := validApplication()
	if err := application.Validate(); err != nil {
		t.Fatalf("valid application rejected: %v", err)
	}
	application.Currency = "EUR"
	if err := application.Validate(); err == nil {
		t.Fatal("unapproved currency accepted")
	}
	application = validApplication()
	application.Amount = 0
	if err := application.Validate(); err == nil {
		t.Fatal("zero amount accepted")
	}
}

func TestRoleAssignmentsEnforceSeparation(t *testing.T) {
	if err := ValidateRoleAssignments(validAssignments()); err != nil {
		t.Fatalf("valid assignments rejected: %v", err)
	}
	shared := validAssignments()
	shared[RoleNIMASAApprover] = shared[RoleUnderwriterPrimary]
	if err := ValidateRoleAssignments(shared); !errors.Is(err, ErrRoleSeparation) {
		t.Fatalf("dual-role principal error = %v", err)
	}
	missing := validAssignments()
	delete(missing, RoleBeneficiary)
	if err := ValidateRoleAssignments(missing); !errors.Is(err, ErrRoleAssignmentInvalid) {
		t.Fatalf("incomplete assignments error = %v", err)
	}
}

func TestResolveReconciliationStateMachine(t *testing.T) {
	parked := validApplication()
	parked.State = StateReconciliationRequired
	resumed, err := ResolveReconciliation(parked, ResolutionResumeDisbursement)
	if err != nil || resumed.State != StateDisbursementPending {
		t.Fatalf("resume = %+v %v", resumed, err)
	}
	rejected, err := ResolveReconciliation(parked, ResolutionReject)
	if err != nil || rejected.State != StateRejected {
		t.Fatalf("reject = %+v %v", rejected, err)
	}
	// Only the reconciliation branch may be resolved.
	for _, state := range []State{StateSubmitted, StateUnderwritingPrimary, StateDisbursementPending, StateDisbursed, StateAudited, StateRejected} {
		candidate := validApplication()
		candidate.State = state
		if _, err := ResolveReconciliation(candidate, ResolutionResumeDisbursement); !errors.Is(err, ErrSequenceViolation) {
			t.Fatalf("resolution from %s error = %v", state, err)
		}
	}
	// Unknown resolutions fail closed.
	if _, err := ResolveReconciliation(parked, ReconciliationResolution("FORCE_APPROVE")); !errors.Is(err, ErrResolutionInvalid) {
		t.Fatalf("unknown resolution error = %v", err)
	}
	if _, err := ResolveReconciliation(parked, ReconciliationResolution("")); !errors.Is(err, ErrResolutionInvalid) {
		t.Fatalf("empty resolution error = %v", err)
	}
}

func approveChain(t *testing.T, application Application) Application {
	t.Helper()
	assignments := validAssignments()
	sequence := []struct {
		role     Role
		from, to State
	}{
		{RoleUnderwriterPrimary, StateUnderwritingPrimary, StateUnderwritingSecondary},
		{RoleUnderwriterSecondary, StateUnderwritingSecondary, StateUnderwritingTertiary},
		{RoleUnderwriterTertiary, StateUnderwritingTertiary, StateNIMASAApproval},
		{RoleNIMASAApprover, StateNIMASAApproval, StateBankConfirmation},
		{RoleReceivingBank, StateBankConfirmation, StateDisbursementPending},
		{RoleBeneficiary, StateDisbursementPending, StateDisbursed},
	}
	var err error
	application, err = BeginUnderwriting(application)
	if err != nil {
		t.Fatalf("begin underwriting: %v", err)
	}
	for _, step := range sequence {
		if application.State != step.from {
			t.Fatalf("state before %s decision = %s, want %s", step.role, application.State, step.from)
		}
		var approval Approval
		application, approval, err = ApplyDecision(application, assignments, assignments[step.role], DecisionApprove)
		if err != nil {
			t.Fatalf("%s decision failed: %v", step.role, err)
		}
		if approval.Role != step.role || approval.PrincipalID != assignments[step.role] || approval.ToState != step.to {
			t.Fatalf("unexpected approval entry: %+v", approval)
		}
		if application.State != step.to {
			t.Fatalf("state after %s decision = %s, want %s", step.role, application.State, step.to)
		}
	}
	return application
}

func TestFullFourPartyChain(t *testing.T) {
	application := approveChain(t, validApplication())
	if application.State != StateDisbursed {
		t.Fatalf("final state = %s, want DISBURSED", application.State)
	}
	audited, err := MarkAudited(application)
	if err != nil {
		t.Fatalf("audit close failed: %v", err)
	}
	if audited.State != StateAudited {
		t.Fatalf("audited state = %s", audited.State)
	}
	if _, _, err := ApplyDecision(audited, validAssignments(), "kc-nimasa", DecisionApprove); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("decision after terminal state error = %v", err)
	}
}

func TestSequenceViolationRejected(t *testing.T) {
	application := validApplication()
	assignments := validAssignments()
	if _, _, err := ApplyDecision(application, assignments, assignments[RoleNIMASAApprover], DecisionApprove); !errors.Is(err, ErrSequenceViolation) {
		t.Fatalf("decision on SUBMITTED error = %v", err)
	}
	application, err := BeginUnderwriting(application)
	if err != nil {
		t.Fatalf("begin underwriting: %v", err)
	}
	// NIMASA cannot decide before the underwriting tiers complete.
	if _, _, err := ApplyDecision(application, assignments, assignments[RoleNIMASAApprover], DecisionApprove); !errors.Is(err, ErrRoleNotAssigned) {
		t.Fatalf("early NIMASA decision error = %v", err)
	}
	// The tertiary underwriter cannot decide the primary tier.
	if _, _, err := ApplyDecision(application, assignments, assignments[RoleUnderwriterTertiary], DecisionApprove); !errors.Is(err, ErrRoleNotAssigned) {
		t.Fatalf("wrong-tier decision error = %v", err)
	}
}

func TestUnknownPrincipalRejected(t *testing.T) {
	application, err := BeginUnderwriting(validApplication())
	if err != nil {
		t.Fatalf("begin underwriting: %v", err)
	}
	if _, _, err := ApplyDecision(application, validAssignments(), "kc-intruder", DecisionApprove); !errors.Is(err, ErrRoleNotAssigned) {
		t.Fatalf("unassigned principal error = %v", err)
	}
}

func TestRejectionIsFailClosed(t *testing.T) {
	application, err := BeginUnderwriting(validApplication())
	if err != nil {
		t.Fatalf("begin underwriting: %v", err)
	}
	assignments := validAssignments()
	updated, approval, err := ApplyDecision(application, assignments, assignments[RoleUnderwriterPrimary], DecisionReject)
	if err != nil {
		t.Fatalf("primary rejection failed: %v", err)
	}
	if updated.State != StateRejected || approval.ToState != StateRejected {
		t.Fatalf("rejection outcome = %s/%s", updated.State, approval.ToState)
	}
	if _, _, err := ApplyDecision(updated, assignments, assignments[RoleUnderwriterSecondary], DecisionApprove); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("decision after rejection error = %v", err)
	}
}

func TestReconciliationBranch(t *testing.T) {
	application, err := BeginUnderwriting(validApplication())
	if err != nil {
		t.Fatalf("begin underwriting: %v", err)
	}
	flagged, err := RequireReconciliation(application)
	if err != nil {
		t.Fatalf("reconciliation branch failed: %v", err)
	}
	if flagged.State != StateReconciliationRequired {
		t.Fatalf("flagged state = %s", flagged.State)
	}
	if _, _, err := ApplyDecision(flagged, validAssignments(), "kc-uw-primary", DecisionApprove); !errors.Is(err, ErrSequenceViolation) {
		t.Fatalf("decision in reconciliation branch error = %v", err)
	}
}

func TestSLADeadlinesSkipWeekends(t *testing.T) {
	// Friday 2026-08-28 10:00 UTC.
	friday := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	if friday.Weekday() != time.Friday {
		t.Fatalf("fixture weekday = %v", friday.Weekday())
	}
	primary, err := SLADeadline(TierPrimary, friday)
	if err != nil {
		t.Fatalf("primary SLA: %v", err)
	}
	if want := friday.AddDate(0, 0, 7); !primary.Equal(want) {
		t.Fatalf("primary deadline = %v, want %v (5 business days skips 2 weekend days)", primary, want)
	}
	secondary, err := SLADeadline(TierSecondary, friday)
	if err != nil {
		t.Fatalf("secondary SLA: %v", err)
	}
	if want := friday.AddDate(0, 0, 5); !secondary.Equal(want) {
		t.Fatalf("secondary deadline = %v, want %v", secondary, want)
	}
	tertiary, err := SLADeadline(TierTertiary, friday)
	if err != nil {
		t.Fatalf("tertiary SLA: %v", err)
	}
	if want := friday.AddDate(0, 0, 4); !tertiary.Equal(want) {
		t.Fatalf("tertiary deadline = %v, want %v", tertiary, want)
	}
	expired, err := SLAExpired(TierTertiary, friday, tertiary.Add(time.Second))
	if err != nil || !expired {
		t.Fatalf("tertiary expiry = %v, %v", expired, err)
	}
	expired, err = SLAExpired(TierTertiary, friday, tertiary)
	if err != nil || expired {
		t.Fatalf("at-deadline expiry = %v, %v", expired, err)
	}
	if _, err := SLABusinessDays("QUATERNARY"); err == nil {
		t.Fatal("unknown tier accepted")
	}
}
