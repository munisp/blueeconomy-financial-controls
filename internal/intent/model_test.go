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
