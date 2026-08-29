package intent

import "testing"

func validRequest() CreateRequest {
	return CreateRequest{IntentID: "intent-001", ExternalRef: "ref-001", DebitAccountID: "100", CreditAccountID: "200", Amount: 1000, Ledger: 1, Code: 1, Currency: "NGN", Maker: "maker-001"}
}

func TestValidationAndImmutableMatch(t *testing.T) {
	request := validRequest()
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	retained := Intent{CreateRequest: request}
	if !retained.Matches(request) {
		t.Fatal("exact request did not match")
	}
	request.Amount++
	if retained.Matches(request) {
		t.Fatal("changed amount matched")
	}
}

func TestApprovalRequiresDistinctChecker(t *testing.T) {
	current := Intent{CreateRequest: validRequest(), State: StateDraft}
	if _, err := Approve(current, current.Maker); err != ErrMakerChecker {
		t.Fatalf("same maker/checker error = %v", err)
	}
	approved, err := Approve(current, "checker-001")
	if err != nil {
		t.Fatalf("approval failed: %v", err)
	}
	if approved.State != StateApproved || approved.Checker == nil || *approved.Checker != "checker-001" {
		t.Fatalf("unexpected approval: %+v", approved)
	}
}

func TestOperationalTransitions(t *testing.T) {
	if !ValidOperationalTransition(StateApproved, StateReservationRequested) {
		t.Fatal("approval to reservation not allowed")
	}
	if !ValidOperationalTransition(StateReservationRequested, StateReserved) {
		t.Fatal("reservation to reserved not allowed")
	}
	if !ValidOperationalTransition(StateReserved, StatePosted) || !ValidOperationalTransition(StateReserved, StateVoided) {
		t.Fatal("reserved terminal outcomes not allowed")
	}
	if !ValidOperationalTransition(StateReservationRequested, StateAmbiguous) {
		t.Fatal("ambiguous reservation outcome not allowed")
	}
	if ValidOperationalTransition(StateDraft, StatePosted) || ValidOperationalTransition(StatePosted, StateVoided) {
		t.Fatal("invalid financial transition accepted")
	}
}

func TestReconciliationRequiredTransitions(t *testing.T) {
	for _, next := range []State{StateReserved, StatePosted, StateVoided, StateAmbiguous} {
		if !ValidOperationalTransition(StateReconciliationRequired, next) {
			t.Fatalf("expected reconciliation-required -> %s to be allowed", next)
		}
	}
	for _, next := range []State{StateApproved, StateReservationRequested, StateReconciliationRequired} {
		if ValidOperationalTransition(StateReconciliationRequired, next) {
			t.Fatalf("expected reconciliation-required -> %s to be rejected", next)
		}
	}
}

func TestAmbiguousTransitions(t *testing.T) {
	for _, next := range []State{StateReconciliationRequired, StateVoided} {
		if !ValidOperationalTransition(StateAmbiguous, next) {
			t.Fatalf("expected ambiguous -> %s to be allowed (officer resolution)", next)
		}
	}
	for _, next := range []State{StateDraft, StateApproved, StateReservationRequested, StateReserved, StatePosted, StateAmbiguous} {
		if ValidOperationalTransition(StateAmbiguous, next) {
			t.Fatalf("expected ambiguous -> %s to be rejected", next)
		}
	}
}

func ambiguousIntent() Intent {
	return Intent{CreateRequest: validRequest(), State: StateAmbiguous, Version: 3}
}

func TestResolveAmbiguousOfficerDispositions(t *testing.T) {
	reconciled, err := ResolveAmbiguous(ambiguousIntent(), 3, "officer-001", ResolutionReconcile)
	if err != nil || reconciled.State != StateReconciliationRequired {
		t.Fatalf("reconcile resolution = %v, %s", err, reconciled.State)
	}
	voided, err := ResolveAmbiguous(ambiguousIntent(), 3, "officer-001", ResolutionVoid)
	if err != nil || voided.State != StateVoided {
		t.Fatalf("void resolution = %v, %s", err, voided.State)
	}
}

func TestResolveAmbiguousFailClosed(t *testing.T) {
	if _, err := ResolveAmbiguous(Intent{CreateRequest: validRequest(), State: StateDraft, Version: 1}, 1, "officer-001", ResolutionVoid); err != ErrInvalidState {
		t.Fatalf("non-ambiguous resolution error = %v", err)
	}
	if _, err := ResolveAmbiguous(ambiguousIntent(), 99, "officer-001", ResolutionVoid); err != ErrConflict {
		t.Fatalf("stale version error = %v", err)
	}
	if _, err := ResolveAmbiguous(ambiguousIntent(), 3, "", ResolutionVoid); err == nil {
		t.Fatal("empty officer accepted")
	}
	if _, err := ResolveAmbiguous(ambiguousIntent(), 3, "officer 001", ResolutionVoid); err == nil {
		t.Fatal("non-canonical officer accepted")
	}
	if _, err := ResolveAmbiguous(ambiguousIntent(), 3, "officer-001", Resolution("FORCE_POST")); err != ErrInvalidResolution {
		t.Fatalf("unknown resolution error = %v", err)
	}
}
