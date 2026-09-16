package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// stubStore is an in-memory ApplicationStore exercising the real state machine.
type stubStore struct {
	application cvff.Application
	assignments map[cvff.Role]string
	escalations []cvff.UnderwritingTier
}

func newStubStore() *stubStore {
	return &stubStore{
		application: cvff.Application{
			ApplicationID: "cvff-001", ExternalRef: "cvff-ref-001", BeneficiaryID: "beneficiary-001",
			Amount: 700_000_000, Currency: "USD", State: cvff.StateSubmitted, Version: 1,
		},
		assignments: map[cvff.Role]string{
			cvff.RoleUnderwriterPrimary:   "kc-uw-primary",
			cvff.RoleUnderwriterSecondary: "kc-uw-secondary",
			cvff.RoleUnderwriterTertiary:  "kc-uw-tertiary",
			cvff.RoleNIMASAApprover:       "kc-nimasa",
			cvff.RoleReceivingBank:        "kc-bank",
			cvff.RoleBeneficiary:          "kc-beneficiary",
		},
	}
}

func (store *stubStore) Get(_ context.Context, applicationID string) (cvff.Application, error) {
	if applicationID != store.application.ApplicationID {
		return cvff.Application{}, cvff.ErrNotFound
	}
	return store.application, nil
}

func (store *stubStore) RecordDecision(_ context.Context, applicationID string, _ int64, principalID string, decision cvff.Decision) (cvff.Application, cvff.Approval, error) {
	if applicationID != store.application.ApplicationID {
		return cvff.Application{}, cvff.Approval{}, cvff.ErrNotFound
	}
	updated, approval, err := cvff.ApplyDecision(store.application, store.assignments, principalID, decision)
	if err != nil {
		return cvff.Application{}, cvff.Approval{}, err
	}
	store.application = updated
	store.application.Version++
	return store.application, approval, nil
}

func (store *stubStore) Transition(_ context.Context, applicationID string, _ int64, move func(cvff.Application) (cvff.Application, error), _ string) (cvff.Application, error) {
	if applicationID != store.application.ApplicationID {
		return cvff.Application{}, cvff.ErrNotFound
	}
	updated, err := move(store.application)
	if err != nil {
		return cvff.Application{}, err
	}
	store.application = updated
	store.application.Version++
	return store.application, nil
}

func (store *stubStore) RecordEscalation(_ context.Context, applicationID string, tier cvff.UnderwritingTier, _ time.Time) error {
	if applicationID != store.application.ApplicationID {
		return cvff.ErrNotFound
	}
	store.escalations = append(store.escalations, tier)
	return nil
}

func (store *stubStore) ResolveReconciliation(_ context.Context, applicationID string, _ int64, _ string, resolution cvff.ReconciliationResolution) (cvff.Application, error) {
	return store.ResolveReconciliationTo(context.Background(), applicationID, 0, "", resolution, cvff.StateDisbursementPending)
}

func (store *stubStore) ResolveReconciliationTo(_ context.Context, applicationID string, _ int64, _ string, resolution cvff.ReconciliationResolution, resumeTarget cvff.State) (cvff.Application, error) {
	if applicationID != store.application.ApplicationID {
		return cvff.Application{}, cvff.ErrNotFound
	}
	updated, err := cvff.ResolveReconciliationTo(store.application, resolution, resumeTarget)
	if err != nil {
		return cvff.Application{}, err
	}
	store.application = updated
	store.application.Version++
	return store.application, nil
}

type stubDisburser struct {
	calls      int
	failures   int
	failureErr error
	store      *stubStore
}

// Disburse mirrors the production rail contract: a failure first moves the
// application into the fail-closed RECONCILIATION_REQUIRED branch, then
// returns the error.
func (disburser *stubDisburser) Disburse(_ context.Context, applicationID string) error {
	disburser.calls++
	if disburser.failures > 0 {
		disburser.failures--
		if disburser.store != nil && disburser.store.application.ApplicationID == applicationID {
			if updated, err := cvff.RequireReconciliation(disburser.store.application); err == nil {
				disburser.store.application = updated
				disburser.store.application.Version++
			}
		}
		if disburser.failureErr != nil {
			return disburser.failureErr
		}
		return errors.New("tigerbeetle cluster unreachable")
	}
	return nil
}

func newTestEnvironment(t *testing.T) (*testsuite.TestWorkflowEnvironment, *stubStore, *stubDisburser, *CVFFWorkflow) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	// Friday: business-day SLA deadlines cross the weekend deterministically.
	env.SetStartTime(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC))
	store := newStubStore()
	disburser := &stubDisburser{store: store}
	activities, err := NewActivities(store, disburser)
	require.NoError(t, err)
	env.RegisterActivityWithOptions(activities.BeginUnderwriting, activity.RegisterOptions{Name: ActivityBeginUnderwriting})
	env.RegisterActivityWithOptions(activities.RecordDecision, activity.RegisterOptions{Name: ActivityRecordDecision})
	env.RegisterActivityWithOptions(activities.RecordEscalation, activity.RegisterOptions{Name: ActivityRecordEscalation})
	env.RegisterActivityWithOptions(activities.RequireReconciliation, activity.RegisterOptions{Name: ActivityRequireReconciliation})
	env.RegisterActivityWithOptions(activities.Disburse, activity.RegisterOptions{Name: ActivityDisburse})
	env.RegisterActivityWithOptions(activities.ResolveReconciliation, activity.RegisterOptions{Name: ActivityResolveReconciliation})
	env.RegisterActivityWithOptions(activities.CommitAudit, activity.RegisterOptions{Name: ActivityCommitAudit})
	definition, err := NewCVFFWorkflow(activities)
	require.NoError(t, err)
	return env, store, disburser, definition
}

func signalAllParties(env *testsuite.TestWorkflowEnvironment, after time.Duration) {
	decisions := []struct {
		signal    string
		principal string
	}{
		{SignalUnderwritingDecisionPrimary, "kc-uw-primary"},
		{SignalUnderwritingDecisionSecondary, "kc-uw-secondary"},
		{SignalUnderwritingDecisionTertiary, "kc-uw-tertiary"},
		{SignalNIMASADecision, "kc-nimasa"},
		{SignalBankConfirmation, "kc-bank"},
		{SignalBeneficiaryConfirmation, "kc-beneficiary"},
	}
	for index, decision := range decisions {
		decision := decision
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(decision.signal, DecisionSignal{PrincipalID: decision.principal, Decision: cvff.DecisionApprove})
		}, after+time.Duration(index)*time.Minute)
	}
}

func TestWorkflowHappyPathAudits(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	signalAllParties(env, time.Minute)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateAudited, result.State)
	require.Equal(t, 0, result.Escalations)
	require.Equal(t, 1, disburser.calls)
	require.Equal(t, cvff.StateAudited, store.application.State)
}

func TestWorkflowNIMASARejectionIsFailClosed(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	decisions := []struct {
		signal    string
		principal string
		decision  cvff.Decision
	}{
		{SignalUnderwritingDecisionPrimary, "kc-uw-primary", cvff.DecisionApprove},
		{SignalUnderwritingDecisionSecondary, "kc-uw-secondary", cvff.DecisionApprove},
		{SignalUnderwritingDecisionTertiary, "kc-uw-tertiary", cvff.DecisionApprove},
		{SignalNIMASADecision, "kc-nimasa", cvff.DecisionReject},
	}
	for index, decision := range decisions {
		decision := decision
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(decision.signal, DecisionSignal{PrincipalID: decision.principal, Decision: decision.decision})
		}, time.Minute+time.Duration(index)*time.Minute)
	}
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateRejected, result.State)
	require.Equal(t, 0, disburser.calls, "rejected application must never disburse")
	require.Equal(t, cvff.StateRejected, store.application.State)
}

// TestWorkflowRoleViolationParksAndRecovers is the H1 regression: a decision
// from an unassigned principal (or a duplicate/early signal that reaches
// RecordDecision) no longer terminates the workflow and strands the
// application in UNDERWRITING_*. The application parks in
// RECONCILIATION_REQUIRED; the officer's RESUME resolution re-awaits the
// interrupted tier and the correct party's decision completes the chain.
func TestWorkflowRoleViolationParksAndRecovers(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalUnderwritingDecisionPrimary, DecisionSignal{PrincipalID: "kc-intruder", Decision: cvff.DecisionApprove})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalReconciliationResolution, ResolutionSignal{PrincipalID: "kc-recon-officer", Resolution: cvff.ResolutionResumeDisbursement})
	}, 3*time.Minute)
	signalAllParties(env, 5*time.Minute)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateAudited, result.State)
	require.Equal(t, cvff.StateAudited, store.application.State)
	require.Equal(t, 1, disburser.calls)
}

// TestWorkflowDecisionStageReconciliationRejectClosesChain: a rejected
// decision stage parked in RECONCILIATION_REQUIRED closes as REJECTED on the
// officer's REJECT resolution instead of resuming.
func TestWorkflowDecisionStageReconciliationRejectClosesChain(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalUnderwritingDecisionSecondary, DecisionSignal{PrincipalID: "kc-intruder", Decision: cvff.DecisionApprove})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalUnderwritingDecisionPrimary, DecisionSignal{PrincipalID: "kc-uw-primary", Decision: cvff.DecisionApprove})
	}, 2*time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalReconciliationResolution, ResolutionSignal{PrincipalID: "kc-recon-officer", Resolution: cvff.ResolutionReject})
	}, 4*time.Minute)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateRejected, result.State)
	require.Equal(t, cvff.StateRejected, store.application.State)
	require.Equal(t, 0, disburser.calls, "rejected application must never disburse")
}

// TestWorkflowCrossTierSignalCannotLeak: a SECONDARY-tier decision signalled
// while PRIMARY is awaited is scoped to its own channel; it is consumed only
// when the SECONDARY tier waits, never misapplied to PRIMARY.
func TestWorkflowCrossTierSignalCannotLeak(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	env.RegisterDelayedCallback(func() {
		// Early SECONDARY decision (correct principal, wrong time): buffered
		// on the secondary channel and applied only at the secondary stage.
		env.SignalWorkflow(SignalUnderwritingDecisionSecondary, DecisionSignal{PrincipalID: "kc-uw-secondary", Decision: cvff.DecisionApprove})
	}, time.Minute)
	signalAllParties(env, 3*time.Minute)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateAudited, result.State)
	require.Equal(t, cvff.StateAudited, store.application.State)
	require.Equal(t, 1, disburser.calls)
}

func TestWorkflowSLAExpiryEscalatesAndContinues(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	// PRIMARY SLA is 5 business days: from Friday 2026-08-28 the deadline is
	// Friday 2026-09-04 10:00 UTC. Signal after 8 days so the timer fires once.
	signalAllParties(env, 8*24*time.Hour)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateAudited, result.State)
	require.Equal(t, 1, result.Escalations)
	require.Equal(t, []cvff.UnderwritingTier{cvff.TierPrimary}, store.escalations)
	require.Equal(t, 1, disburser.calls)
}

// TestWorkflowDisbursementFailureParksAndResumes: a disbursement failure
// parks the workflow in RECONCILIATION_REQUIRED; the reconciliation officer's
// RESUME signal retries disbursement through the same deterministic workflow
// and the chain completes. A REJECT signal would close it as REJECTED.
func TestWorkflowDisbursementFailureParksAndResumes(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	disburser.failures = 1
	signalAllParties(env, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalReconciliationResolution, ResolutionSignal{PrincipalID: "kc-recon-officer", Resolution: cvff.ResolutionResumeDisbursement})
	}, 12*time.Minute)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateAudited, result.State)
	require.Equal(t, 2, disburser.calls, "disbursement retried after resume")
	require.Equal(t, cvff.StateAudited, store.application.State)
}

func TestWorkflowReconciliationRejectClosesChain(t *testing.T) {
	env, store, disburser, definition := newTestEnvironment(t)
	disburser.failures = 1
	signalAllParties(env, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalReconciliationResolution, ResolutionSignal{PrincipalID: "kc-recon-officer", Resolution: cvff.ResolutionReject})
	}, 12*time.Minute)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{ApplicationID: "cvff-001"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result DisbursementResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, cvff.StateRejected, result.State)
	require.Equal(t, 1, disburser.calls, "rejected reconciliation never retries disbursement")
	require.Equal(t, cvff.StateRejected, store.application.State)
}

func TestNewActivitiesFailsClosed(t *testing.T) {
	if _, err := NewActivities(nil, &stubDisburser{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewActivities(newStubStore(), nil); err == nil {
		t.Fatal("nil disburser accepted")
	}
	if _, err := NewCVFFWorkflow(nil); err == nil {
		t.Fatal("nil activities accepted")
	}
}

func TestWorkflowRejectsMissingApplicationID(t *testing.T) {
	env, _, _, definition := newTestEnvironment(t)
	env.ExecuteWorkflow(definition.CVFFDisbursementWorkflow, DisbursementInput{})
	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
}

func TestStubStoreMatchesStateMachine(t *testing.T) {
	store := newStubStore()
	if _, _, err := store.RecordDecision(context.Background(), "cvff-001", 1, "kc-nimasa", cvff.DecisionApprove); !errors.Is(err, cvff.ErrSequenceViolation) {
		t.Fatalf("premature decision error = %v", err)
	}
	if _, err := store.Transition(context.Background(), "unknown", 1, cvff.BeginUnderwriting, "cvff.underwriting_started"); !errors.Is(err, cvff.ErrNotFound) {
		t.Fatalf("unknown application error = %v", err)
	}
	if _, _, err := store.RecordDecision(context.Background(), "unknown", 1, "kc-uw-primary", cvff.DecisionApprove); !errors.Is(err, cvff.ErrNotFound) {
		t.Fatalf("unknown decision application error = %v", err)
	}
	if err := store.RecordEscalation(context.Background(), "unknown", cvff.TierPrimary, time.Now()); !errors.Is(err, cvff.ErrNotFound) {
		t.Fatalf("unknown escalation application error = %v", err)
	}
}
