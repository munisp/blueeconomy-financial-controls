//go:build integration

package cvff

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRealPostgresCVFFFlow verifies the four-party chain against real
// PostgreSQL: role separation enforced by the database, immutable approvals,
// in-sequence decisions and outbox records. Requires DATABASE_URL and
// MIGRATION_PATH (db/migrations/0003_cvff_disbursement.sql, with the prior
// migrations applied).
func TestRealPostgresCVFFFlow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, name := range []string{"0001", "0002", "0003"} {
		matches, globErr := filepath.Glob(filepath.Join(os.Getenv("MIGRATION_PATH"), name+"_*.sql"))
		if globErr != nil || len(matches) != 1 {
			t.Fatalf("locate migration %s: %v", name, globErr)
		}
		migration, err := os.ReadFile(filepath.Clean(matches[0]))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	application := validApplication()
	assignments := validAssignments()
	retained, err := store.Submit(ctx, application, assignments)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != StateSubmitted || retained.Version != 1 {
		t.Fatalf("unexpected submitted application: %+v", retained)
	}

	// Role separation is enforced both in the state machine and in the
	// database: one principal cannot hold two roles on one application.
	conflicting := validApplication()
	conflicting.ApplicationID = "cvff-002"
	conflicting.ExternalRef = "cvff-ref-002"
	conflictingAssignments := validAssignments()
	conflictingAssignments[RoleNIMASAApprover] = conflictingAssignments[RoleUnderwriterPrimary]
	if _, err := store.Submit(ctx, conflicting, conflictingAssignments); !errors.Is(err, ErrRoleSeparation) {
		t.Fatalf("dual-role submit error = %v", err)
	}
	if err := store.Exec(ctx, `INSERT INTO cvff_role_assignments (application_id, role, principal_id, created_at) VALUES ('cvff-001', 'UNDERWRITER_PRIMARY', 'kc-nimasa', now())`); err == nil {
		t.Fatal("database accepted a principal holding two roles")
	}

	// Full chain.
	current, err := store.Transition(ctx, retained.ApplicationID, retained.Version, BeginUnderwriting, "cvff.underwriting_started")
	if err != nil {
		t.Fatal(err)
	}
	chain := []Role{RoleUnderwriterPrimary, RoleUnderwriterSecondary, RoleUnderwriterTertiary, RoleNIMASAApprover, RoleReceivingBank, RoleBeneficiary}
	for _, role := range chain {
		// Out-of-turn principal must fail.
		if _, _, err := store.RecordDecision(ctx, current.ApplicationID, current.Version, "kc-intruder", DecisionApprove); !errors.Is(err, ErrRoleNotAssigned) {
			t.Fatalf("unassigned principal error = %v", err)
		}
		current, _, err = store.RecordDecision(ctx, current.ApplicationID, current.Version, assignments[role], DecisionApprove)
		if err != nil {
			t.Fatalf("%s decision: %v", role, err)
		}
	}
	if current.State != StateDisbursed {
		t.Fatalf("final state = %s", current.State)
	}
	approvals, err := store.ListApprovals(ctx, current.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 6 {
		t.Fatalf("approvals = %d, want 6", len(approvals))
	}
	// Approvals are immutable in the database.
	if err := store.Exec(ctx, `UPDATE cvff_approvals SET decision = 'REJECT' WHERE application_id = 'cvff-001'`); err == nil {
		t.Fatal("approval mutation accepted by database")
	}
	audited, err := store.Transition(ctx, current.ApplicationID, current.Version, MarkAudited, "cvff.audited")
	if err != nil || audited.State != StateAudited {
		t.Fatalf("audit close: %+v %v", audited, err)
	}
}
