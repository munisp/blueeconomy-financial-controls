package tradefinance

import (
	"errors"
	"testing"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

func validAssignments() map[Role]string {
	return map[Role]string{
		RoleBankKYCOfficer:           "kyc-officer-1",
		RoleBankCreditOfficer:        "credit-officer-1",
		RoleCustomsComplianceOfficer: "customs-officer-1",
		RoleBankTreasuryOfficer:      "treasury-officer-1",
		RoleTrader:                   "trader-001",
	}
}

func validApplication() Application {
	return Application{
		ApplicationID: "tf-app-001", ExternalRef: "tf-ref-001", TraderID: "trader-001",
		BankID: "bank-gtb", ConsentID: "tf-con-001", Product: ProductImportLCFacilitation,
		Amount: 250_000_00, Currency: "NGN",
	}
}

func TestApplicationValidate(t *testing.T) {
	app := validApplication()
	if err := app.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, product := range Products {
		app.Product = product
		if err := app.Validate(); err != nil {
			t.Fatalf("product %s rejected: %v", product, err)
		}
	}
	app = validApplication()
	app.Product = "MARGIN_LENDING"
	if err := app.Validate(); err == nil {
		t.Fatal("non-catalogue product accepted")
	}
	app = validApplication()
	app.Amount = 0
	if err := app.Validate(); err == nil {
		t.Fatal("zero amount accepted")
	}
	app = validApplication()
	app.Currency = "GHS"
	if err := app.Validate(); err == nil {
		t.Fatal("unapproved currency accepted")
	}
}

func TestRoleAssignmentSeparation(t *testing.T) {
	assignments := validAssignments()
	if err := ValidateRoleAssignments(assignments); err != nil {
		t.Fatal(err)
	}
	assignments[RoleBankTreasuryOfficer] = assignments[RoleBankKYCOfficer]
	if err := ValidateRoleAssignments(assignments); !errors.Is(err, ErrRoleSeparation) {
		t.Fatalf("dual-role assignment error = %v", err)
	}
}

func TestFourPartyDecisionChain(t *testing.T) {
	assignments := validAssignments()
	current := validApplication()
	current.State = StateApplication
	var err error
	current, err = BeginReview(current)
	if err != nil || current.State != StateKYCConsentCheck {
		t.Fatalf("begin review: %+v %v", current, err)
	}
	chain := []struct {
		role Role
		to   State
	}{
		{RoleBankKYCOfficer, StateBankReview},
		{RoleBankCreditOfficer, StateRegulatoryClearance},
		{RoleCustomsComplianceOfficer, StateApproved},
		{RoleTrader, StateDisbursementPending},
		{RoleBankTreasuryOfficer, StateDisbursed},
		{RoleTrader, StateSettled},
	}
	for _, step := range chain {
		// Out-of-turn principal must fail.
		if _, _, err := ApplyDecision(current, assignments, "intruder", DecisionApprove); !errors.Is(err, ErrRoleNotAssigned) {
			t.Fatalf("intruder at %s error = %v", current.State, err)
		}
		current, _, err = ApplyDecision(current, assignments, assignments[step.role], DecisionApprove)
		if err != nil {
			t.Fatalf("%s decision: %v", step.role, err)
		}
		if current.State != step.to {
			t.Fatalf("state after %s = %s, want %s", step.role, current.State, step.to)
		}
	}
	if !current.State.Terminal() {
		t.Fatal("SETTLED is not terminal")
	}
	if _, _, err := ApplyDecision(current, assignments, assignments[RoleTrader], DecisionApprove); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("decision on terminal error = %v", err)
	}
}

func TestRejectionFailsClosed(t *testing.T) {
	assignments := validAssignments()
	current := validApplication()
	current.State = StateKYCConsentCheck
	declined, approval, err := ApplyDecision(current, assignments, assignments[RoleBankKYCOfficer], DecisionReject)
	if err != nil {
		t.Fatal(err)
	}
	if declined.State != StateDeclined || approval.ToState != StateDeclined {
		t.Fatalf("rejection outcome = %+v", declined)
	}
	if !declined.State.Terminal() {
		t.Fatal("DECLINED is not terminal")
	}
}

func TestDisbursementDeterministicIDs(t *testing.T) {
	first, err := ReserveTransferID("tf-app-001")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReserveTransferID("tf-app-001")
	if err != nil {
		t.Fatal(err)
	}
	other, err := ReserveTransferID("tf-app-002")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("reserve transfer id is not deterministic")
	}
	if first == other {
		t.Fatal("reserve transfer id collides across applications")
	}
	if first == (tigerbeetle.Uint128{}) {
		t.Fatal("reserve transfer id is zero")
	}
	disburse, _ := DisburseTransferID("tf-app-001")
	settle, _ := SettleTransferID("tf-app-001")
	if disburse == first || settle == first || disburse == settle {
		t.Fatal("transfer id domains collide")
	}
}
