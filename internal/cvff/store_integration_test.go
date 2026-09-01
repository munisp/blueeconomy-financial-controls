//go:build integration

package cvff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rateDate() time.Time {
	return time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
}

// TestRealPostgresCVFFFlow verifies the four-party chain against real
// PostgreSQL: role separation enforced by the database, immutable approvals,
// in-sequence decisions and outbox records. Requires DATABASE_URL and
// MIGRATION_PATH (db/migrations/0003_cvff_disbursement.sql, with the prior
// migrations applied).
// resetPublicSchema gives each integration test a clean public schema —
// the repo convention for the real-PostgreSQL harnesses — so several
// integration tests can share one database (e.g. in CI).
func resetPublicSchema(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	if err := store.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}

func TestRealPostgresCVFFFlow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resetPublicSchema(t, ctx, store)
	for _, name := range []string{"0001", "0002", "0003", "0004", "0005"} {
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

// TestRealPostgresProductionWiring verifies the production paths added for
// the four-party rail: role assignment for intake-created applications
// (idempotent, conflict on divergence), disbursement legs persisting the
// CBN-rate conversion basis and the RECONCILIATION_REQUIRED resolution path.
func TestRealPostgresProductionWiring(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resetPublicSchema(t, ctx, store)
	for _, name := range []string{"0001", "0002", "0003", "0004", "0005"} {
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

	// An intake-style application (recorded without role assignments).
	application := validApplication()
	application.ApplicationID = "cvff-wire-001"
	application.ExternalRef = "cvff-wire-ref-001"
	if _, err := store.SubmitIntake(ctx, Intake{
		ApplicationID: application.ApplicationID, IdempotencyKey: application.ExternalRef,
		BeneficiaryID: application.BeneficiaryID, VesselName: "MV Wiring", IMONumber: "9074729",
		OfficialNumber: "NIMASA-4421", VesselClass: VesselClassCargoCoaster, CabotageRoute: RouteLagosOnne,
		Amount: application.Amount, Currency: "NGN",
		BusinessName: "Wiring Logistics Ltd", BusinessRCNumber: "RC123456", BusinessAddress: "14 Marina Road, Lagos",
	}); err != nil {
		t.Fatalf("submit intake: %v", err)
	}

	// Role assignment: officer binds the parties; replay idempotent;
	// divergence and self-dealing refused.
	assignments := validAssignments()
	retained, err := store.AssignRoles(ctx, application.ApplicationID, "kc-officer-1", assignments)
	if err != nil {
		t.Fatalf("assign roles: %v", err)
	}
	if !roleAssignmentsEqual(retained, assignments) {
		t.Fatalf("retained assignments = %v", retained)
	}
	if _, err := store.AssignRoles(ctx, application.ApplicationID, "kc-officer-1", assignments); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	divergent := validAssignments()
	divergent[RoleNIMASAApprover] = "kc-nimasa-other"
	if _, err := store.AssignRoles(ctx, application.ApplicationID, "kc-officer-1", divergent); !errors.Is(err, ErrConflict) {
		t.Fatalf("divergent replay error = %v", err)
	}
	if _, err := store.ResolveReconciliation(ctx, application.ApplicationID, 1, "kc-officer-1", ResolutionResumeDisbursement); !errors.Is(err, ErrSequenceViolation) {
		t.Fatalf("resolution outside branch error = %v", err)
	}

	// Disbursement legs persist the conversion basis with rate + timestamps.
	// The legs reference a dual-control-confirmed CBN rate row (rate_id is a
	// UUID FK to fx_rates), so seed the confirmed rate first.
	const seedRateID = "00000000-0000-4000-8000-0000000000a1"
	if err := store.Exec(ctx, fmt.Sprintf(`INSERT INTO fx_rates
		(rate_id, base_currency, quote_currency, ngn_per_usd_micro, effective_date, maker, checker, state, created_at, confirmed_at)
		VALUES ('%s', 'USD', 'NGN', 1550250000, '%s', 'kc-maker-1', 'kc-checker-1', 'CONFIRMED', now(), now())`,
		seedRateID, rateDate().Format("2006-01-02"))); err != nil {
		t.Fatalf("seed confirmed CBN rate: %v", err)
	}
	legs := DisbursementLegs{
		ApplicationID: application.ApplicationID, RateID: seedRateID, NGNPerUSDMicro: 1_550_250_000,
		RateEffectiveDate: rateDate(), FeeTransferID: "fee-t-1", CostTransferID: "cost-t-1",
		FeeNGNMinor: 2_712_937_500, CostUSDMinor: 700_000_000, CostNGNEquivalent: 1_085_175_000_000,
	}
	if err := store.RecordDisbursementLegs(ctx, legs); err != nil {
		t.Fatalf("record legs: %v", err)
	}
	persisted, err := store.DisbursementLegs(ctx, application.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.RateEffectiveDate.Equal(rateDate()) || persisted.CreatedAt.IsZero() {
		t.Fatalf("persisted conversion basis = %+v", persisted)
	}

	// The reconciliation branch resolves under the officer identity.
	current, err := store.Get(ctx, application.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	current, err = store.Transition(ctx, current.ApplicationID, current.Version, RequireReconciliation, "cvff.reconciliation_required")
	if err != nil {
		t.Fatalf("require reconciliation: %v", err)
	}
	// A chain party may not resolve its own application's deadlock.
	if _, err := store.ResolveReconciliation(ctx, application.ApplicationID, current.Version, assignments[RoleBeneficiary], ResolutionResumeDisbursement); !errors.Is(err, ErrRoleSeparation) {
		t.Fatalf("party resolution error = %v", err)
	}
	resumed, err := store.ResolveReconciliation(ctx, application.ApplicationID, current.Version, "kc-recon-officer", ResolutionResumeDisbursement)
	if err != nil {
		t.Fatalf("resume resolution: %v", err)
	}
	if resumed.State != StateDisbursementPending {
		t.Fatalf("resumed state = %s", resumed.State)
	}
	// Re-park and reject.
	if _, err := store.Transition(ctx, resumed.ApplicationID, resumed.Version, RequireReconciliation, "cvff.reconciliation_required"); err != nil {
		t.Fatal(err)
	}
	rejected, err := store.ResolveReconciliation(ctx, resumed.ApplicationID, resumed.Version+1, "kc-recon-officer", ResolutionReject)
	if err != nil || rejected.State != StateRejected {
		t.Fatalf("reject resolution: %+v %v", rejected, err)
	}
	if _, err := store.AssignRoles(ctx, application.ApplicationID, "kc-officer-1", validAssignments()); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("assignment on closed application error = %v", err)
	}
}
