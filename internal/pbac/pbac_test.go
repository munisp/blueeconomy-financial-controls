package pbac

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testEnforcer(t *testing.T) *Enforcer {
	t.Helper()
	enforcer, err := LoadEnforcer(filepath.Join("..", "..", "policies"))
	if err != nil {
		t.Fatalf("load shipped policy pack: %v", err)
	}
	return enforcer
}

func principal(subject string, roles ...string) Principal {
	return Principal{Subject: subject, Roles: roles}
}

func fiduciaryInput(subject string, roles []string, resource, action string) Input {
	return Input{
		Principal:      principal(subject, roles...),
		Resource:       resource,
		Action:         action,
		Classification: "FIDUCIARY_SEGREGATED",
	}
}

func validAssignments() map[string]string {
	return map[string]string{
		"UNDERWRITER_PRIMARY":   "kc-uw-primary",
		"UNDERWRITER_SECONDARY": "kc-uw-secondary",
		"UNDERWRITER_TERTIARY":  "kc-uw-tertiary",
		"NIMASA_APPROVER":       "kc-nimasa",
		"RECEIVING_BANK":        "kc-bank",
		"BENEFICIARY":           "kc-beneficiary",
	}
}

func TestAllowDenyMatrix(t *testing.T) {
	enforcer := testEnforcer(t)
	ctx := context.Background()
	for name, test := range map[string]struct {
		input Input
		allow bool
	}{
		"beneficiary reads applications":   {fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "read"), true},
		"beneficiary creates application":  {fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "create"), true},
		"beneficiary uploads":              {fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "upload"), true},
		"beneficiary denied auditor route": {fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.reports.dual-ledger", "read"), false},
		"auditor reads report":             {fiduciaryInput("kc-aud", []string{"auditor"}, "cvff.reports.dual-ledger", "read"), true},
		"auditor cannot mutate":            {fiduciaryInput("kc-aud", []string{"auditor"}, "cvff.applications", "create"), false},
		"auditor cannot decide":            {fiduciaryInput("kc-aud", []string{"auditor"}, "cvff.application", "decide"), false},
		"underwriter decides":              {fiduciaryInput("kc-uw", []string{"underwriter"}, "cvff.application", "decide"), true},
		"nimasa approver decides":          {fiduciaryInput("kc-ni", []string{"nimasa-approver"}, "cvff.application", "decide"), true},
		"receiving bank decides":           {fiduciaryInput("kc-bank", []string{"receiving-bank-officer"}, "cvff.application", "decide"), true},
		"beneficiary decides":              {fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.application", "decide"), true},
		"officer cannot decide":            {fiduciaryInput("kc-off", []string{"cvff-officer"}, "cvff.application", "decide"), false},
		"officer assigns roles":            {fiduciaryInput("kc-off", []string{"cvff-officer"}, "cvff.application.roles", "assign"), false}, // assignments absent
		"recon officer resolves":           {fiduciaryInput("kc-rec", []string{"reconciliation-officer"}, "cvff.application.reconciliation", "resolve"), true},
		"officer cannot resolve":           {fiduciaryInput("kc-off", []string{"cvff-officer"}, "cvff.application.reconciliation", "resolve"), false},
		"no roles denied":                  {fiduciaryInput("kc-anon", nil, "cvff.applications", "read"), false},
		"unknown resource denied":          {fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.admin", "read"), false},
		"unknown action denied":            {fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "delete"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := enforcer.Allow(ctx, test.input); got != test.allow {
				t.Fatalf("allow = %v, want %v", got, test.allow)
			}
		})
	}
}

func TestClassificationClearanceGating(t *testing.T) {
	enforcer := testEnforcer(t)
	ctx := context.Background()
	// Unknown classification fails closed even for a valid role.
	input := fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "read")
	input.Classification = "UNCLASSIFIED"
	if enforcer.Allow(ctx, input) {
		t.Fatal("unknown classification allowed")
	}
	// A token with no recognized role and no clearance claim has no effective
	// clearance: deny.
	input = fiduciaryInput("kc-ben", []string{"unknown-role"}, "cvff.applications", "read")
	if enforcer.Allow(ctx, input) {
		t.Fatal("principal without any recognized clearance allowed")
	}
	// An explicit low clearance claim cannot lower the role-derived floor...
	input = fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "read")
	input.Principal.Clearance = "PUBLIC"
	if !enforcer.Allow(ctx, input) {
		t.Fatal("role-derived clearance floor ignored")
	}
	// ...and a bogus clearance claim is ignored, never trusted.
	input.Principal.Clearance = "SUPERUSER"
	if !enforcer.Allow(ctx, input) {
		t.Fatal("bogus clearance claim broke role-derived floor")
	}
}

func TestTenantBinding(t *testing.T) {
	enforcer := testEnforcer(t)
	ctx := context.Background()
	input := fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "read")
	input.TenantID = "tenant-a"
	input.Principal.TenantID = "tenant-b"
	if enforcer.Allow(ctx, input) {
		t.Fatal("cross-tenant access allowed")
	}
	input.Principal.TenantID = "tenant-a"
	if !enforcer.Allow(ctx, input) {
		t.Fatal("same-tenant access denied")
	}
}

func TestSegregationOfDuties(t *testing.T) {
	enforcer := testEnforcer(t)
	ctx := context.Background()
	assignInput := func(subject string, assignments map[string]string) Input {
		input := fiduciaryInput(subject, []string{"cvff-officer"}, "cvff.application.roles", "assign")
		input.Assignments = assignments
		return input
	}
	if !enforcer.Allow(ctx, assignInput("kc-officer", validAssignments())) {
		t.Fatal("valid four-party assignment denied")
	}
	// One principal holding two chain roles violates SoD.
	collapsed := validAssignments()
	collapsed["NIMASA_APPROVER"] = collapsed["UNDERWRITER_PRIMARY"]
	if enforcer.Allow(ctx, assignInput("kc-officer", collapsed)) {
		t.Fatal("proposer==approver assignment allowed")
	}
	// The assigning officer must never be a party.
	selfDealing := validAssignments()
	selfDealing["RECEIVING_BANK"] = "kc-officer"
	if enforcer.Allow(ctx, assignInput("kc-officer", selfDealing)) {
		t.Fatal("officer-as-party assignment allowed")
	}
	// A missing chain role fails closed.
	missing := validAssignments()
	delete(missing, "BENEFICIARY")
	if enforcer.Allow(ctx, assignInput("kc-officer", missing)) {
		t.Fatal("incomplete assignment allowed")
	}
	// Disburser == auditor class violation: beneficiary collides with bank.
	bankBeneficiary := validAssignments()
	bankBeneficiary["BENEFICIARY"] = bankBeneficiary["RECEIVING_BANK"]
	if enforcer.Allow(ctx, assignInput("kc-officer", bankBeneficiary)) {
		t.Fatal("disburser==beneficiary assignment allowed")
	}
}

func TestLoadEnforcerFailClosed(t *testing.T) {
	if _, err := LoadEnforcer(""); err == nil {
		t.Fatal("empty directory accepted")
	}
	if _, err := LoadEnforcer(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("unreadable directory accepted")
	}
	empty := t.TempDir()
	if _, err := LoadEnforcer(empty); err == nil {
		t.Fatal("zero-policy directory accepted")
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "broken.rego"), []byte("package cvff\nnot rego syntax [[["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEnforcer(broken); err == nil {
		t.Fatal("unparseable policy accepted")
	}
	if _, err := NewEnforcer(nil); err == nil {
		t.Fatal("empty module set accepted")
	}
	// A pack without the allow rule compiles but always denies.
	enforcer, err := NewEnforcer(map[string]string{"other.rego": "package other\n\nimport rego.v1\n"})
	if err != nil {
		t.Fatalf("compile unrelated pack: %v", err)
	}
	if enforcer.Allow(context.Background(), fiduciaryInput("kc-ben", []string{"beneficiary"}, "cvff.applications", "read")) {
		t.Fatal("pack without allow rule allowed a request")
	}
}
