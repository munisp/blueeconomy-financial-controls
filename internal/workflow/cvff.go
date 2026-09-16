// Package workflow implements the Temporal-orchestrated CVFF disbursement
// rail: application intake, per-tier underwriting with SLA timers and
// escalation, four-party approval, disbursement and audit commit.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"go.temporal.io/sdk/workflow"
)

const (
	// SignalUnderwritingDecision is the legacy shared tier signal. It is
	// retained only so in-flight senders fail loudly; the workflow never
	// listens on it. Each tier waits on its own scoped signal (H1) so a
	// duplicate or early decision can never leak into the next tier's wait.
	SignalUnderwritingDecision = "cvff.underwriting-decision"
	// SignalUnderwritingDecisionPrimary carries the PRIMARY tier decision.
	SignalUnderwritingDecisionPrimary = "cvff.underwriting-decision.primary"
	// SignalUnderwritingDecisionSecondary carries the SECONDARY tier decision.
	SignalUnderwritingDecisionSecondary = "cvff.underwriting-decision.secondary"
	// SignalUnderwritingDecisionTertiary carries the TERTIARY tier decision.
	SignalUnderwritingDecisionTertiary = "cvff.underwriting-decision.tertiary"
	// SignalNIMASADecision carries the NIMASA approver decision.
	SignalNIMASADecision = "cvff.nimasa-decision"
	// SignalBankConfirmation carries the receiving-bank confirmation.
	SignalBankConfirmation = "cvff.bank-confirmation"
	// SignalBeneficiaryConfirmation carries the beneficiary confirmation.
	SignalBeneficiaryConfirmation = "cvff.beneficiary-confirmation"
	// SignalReconciliationResolution carries a reconciliation officer's
	// resolution while the workflow is parked in RECONCILIATION_REQUIRED.
	SignalReconciliationResolution = "cvff.reconciliation-resolution"

	// QueryState returns the current lifecycle state for observer replay.
	QueryState = "cvff.state"
	// QueryHistory returns the recorded decision entries for observer replay.
	QueryHistory = "cvff.history"
)

// Stable activity names: workflow histories reference these across deployments.
const (
	ActivityBeginUnderwriting     = "cvff.begin-underwriting"
	ActivityRecordDecision        = "cvff.record-decision"
	ActivityRecordEscalation      = "cvff.record-escalation"
	ActivityRequireReconciliation = "cvff.require-reconciliation"
	ActivityDisburse              = "cvff.disburse"
	ActivityResolveReconciliation = "cvff.resolve-reconciliation"
	ActivityCommitAudit           = "cvff.commit-audit"
)

// DecisionSignal is the payload for every party decision signal. PrincipalID
// is the Keycloak subject of the deciding party.
type DecisionSignal struct {
	PrincipalID string        `json:"principal_id"`
	Decision    cvff.Decision `json:"decision"`
}

// ResolutionSignal is the payload for a reconciliation officer's resolution
// of the RECONCILIATION_REQUIRED branch.
type ResolutionSignal struct {
	PrincipalID string                        `json:"principal_id"`
	Resolution  cvff.ReconciliationResolution `json:"resolution"`
}

// DisbursementInput starts a CVFFDisbursementWorkflow.
type DisbursementInput struct {
	ApplicationID string `json:"application_id"`
}

// DisbursementResult reports the terminal workflow outcome.
type DisbursementResult struct {
	ApplicationID string     `json:"application_id"`
	State         cvff.State `json:"state"`
	Escalations   int        `json:"escalations"`
}

// Activities groups the durable side effects the workflow invokes. Each method
// maps to a Temporal activity implemented against PostgreSQL/TigerBeetle in
// production and mocked in tests.
type Activities struct {
	// BeginUnderwriting moves SUBMITTED -> UNDERWRITING_PRIMARY.
	BeginUnderwriting func(ctx context.Context, applicationID string) (cvff.State, error)
	// RecordDecision applies one party decision through the state machine and
	// returns the resulting state.
	RecordDecision func(ctx context.Context, applicationID, principalID string, decision cvff.Decision) (cvff.State, error)
	// RecordEscalation appends an SLA-expiry audit event (fail-closed: it never
	// advances the approval chain).
	RecordEscalation func(ctx context.Context, applicationID string, tier cvff.UnderwritingTier, deadline time.Time) error
	// RequireReconciliation parks a non-terminal application in the
	// fail-closed RECONCILIATION_REQUIRED branch after a rejected decision
	// stage (H1): the workflow then waits for the officer's resolution
	// instead of terminating and stranding the application.
	RequireReconciliation func(ctx context.Context, applicationID string) (cvff.State, error)
	// Disburse posts the FX-paired disbursement ledger entries.
	Disburse func(ctx context.Context, applicationID string) error
	// ResolveReconciliation applies a reconciliation officer's resolution to
	// the RECONCILIATION_REQUIRED branch and returns the resulting state;
	// resumeTarget is the stage the workflow bound when it parked.
	ResolveReconciliation func(ctx context.Context, applicationID, principalID string, resolution cvff.ReconciliationResolution, resumeTarget cvff.State) (cvff.State, error)
	// CommitAudit closes the lifecycle for a disbursed application.
	CommitAudit func(ctx context.Context, applicationID string) error
}

// underwritingStage describes one tier wait: signal name, tier and SLA.
type underwritingStage struct {
	tier   cvff.UnderwritingTier
	signal string
}

var underwritingStages = []underwritingStage{
	{cvff.TierPrimary, SignalUnderwritingDecisionPrimary},
	{cvff.TierSecondary, SignalUnderwritingDecisionSecondary},
	{cvff.TierTertiary, SignalUnderwritingDecisionTertiary},
}

// CVFFWorkflow binds the workflow definition to its activity dependencies.
// Activities are carried on the struct because workflow arguments must be
// serializable and function values are not.
type CVFFWorkflow struct{ activities *Activities }

// NewCVFFWorkflow fails closed when activities are absent.
func NewCVFFWorkflow(activities *Activities) (*CVFFWorkflow, error) {
	if activities == nil {
		return nil, errors.New("cvff activities are required")
	}
	return &CVFFWorkflow{activities: activities}, nil
}

// CVFFDisbursementWorkflow drives one application through intake, per-tier
// underwriting with SLA timers, NIMASA approval, bank confirmation,
// beneficiary confirmation, disbursement and audit commit. Any rejection or
// activity failure terminates the workflow fail-closed; SLA timer expiry
// raises an escalation audit event and keeps waiting.
func (workflowDef *CVFFWorkflow) CVFFDisbursementWorkflow(ctx workflow.Context, input DisbursementInput) (DisbursementResult, error) {
	if input.ApplicationID == "" {
		return DisbursementResult{}, errors.New("application_id is required")
	}
	result := DisbursementResult{ApplicationID: input.ApplicationID, State: cvff.StateSubmitted}
	history := make([]cvff.Approval, 0)
	if err := workflow.SetQueryHandler(ctx, QueryState, func() (cvff.State, error) { return result.State, nil }); err != nil {
		return result, fmt.Errorf("register state query: %w", err)
	}
	if err := workflow.SetQueryHandler(ctx, QueryHistory, func() ([]cvff.Approval, error) { return history, nil }); err != nil {
		return result, fmt.Errorf("register history query: %w", err)
	}
	options := workflow.ActivityOptions{StartToCloseTimeout: 2 * time.Minute}
	ctx = workflow.WithActivityOptions(ctx, options)

	// Intake: begin underwriting.
	if err := workflow.ExecuteActivity(ctx, ActivityBeginUnderwriting, input.ApplicationID).Get(ctx, &result.State); err != nil {
		return result, fmt.Errorf("begin underwriting: %w", err)
	}

	for stageIndex := 0; stageIndex < len(underwritingStages); {
		stage := underwritingStages[stageIndex]
		state, decision, escalations, err := awaitTierDecision(ctx, input.ApplicationID, stage)
		result.Escalations += escalations
		if err != nil {
			// A rejected decision (duplicate/early signal, wrong principal)
			// must never terminate the rail and strand the application in
			// UNDERWRITING_*: park in RECONCILIATION_REQUIRED and resume the
			// interrupted tier only on the officer's resolution (H1).
			resumed, parkErr := parkForReconciliation(ctx, input.ApplicationID, stateForTier(stage.tier))
			if parkErr != nil {
				result.State = state
				return result, parkErr
			}
			result.State = resumed
			if resumed == cvff.StateRejected {
				return result, nil
			}
			continue
		}
		result.State = state
		history = append(history, decision)
		if result.State == cvff.StateRejected {
			return result, nil
		}
		stageIndex++
	}

	// NIMASA approval, then receiving-bank confirmation.
	for _, stage := range []struct {
		signal string
		state  cvff.State
	}{
		{SignalNIMASADecision, cvff.StateNIMASAApproval},
		{SignalBankConfirmation, cvff.StateBankConfirmation},
	} {
		for {
			decision, err := awaitDecision(ctx, input.ApplicationID, stage.signal)
			if err != nil {
				resumed, parkErr := parkForReconciliation(ctx, input.ApplicationID, stage.state)
				if parkErr != nil {
					return result, parkErr
				}
				result.State = resumed
				if resumed == cvff.StateRejected {
					return result, nil
				}
				continue
			}
			history = append(history, decision)
			if decision.Decision == cvff.DecisionReject {
				result.State = cvff.StateRejected
				return result, nil
			}
			result.State = decision.ToState
			break
		}
	}

	// Disbursement executes only after NIMASA and the receiving bank approved.
	if result.State != cvff.StateDisbursementPending {
		return result, fmt.Errorf("approval chain ended in %s, want %s", result.State, cvff.StateDisbursementPending)
	}
	for {
		if err := workflow.ExecuteActivity(ctx, ActivityDisburse, input.ApplicationID).Get(ctx, nil); err == nil {
			break
		}
		// The rail failed closed: the application now sits in
		// RECONCILIATION_REQUIRED and only a reconciliation officer's
		// resolution may move it. The workflow parks on the resolution
		// signal instead of terminating, so a remediated application can
		// resume disbursement; a REJECT resolution closes the chain.
		result.State = cvff.StateReconciliationRequired
		var resolution ResolutionSignal
		workflow.GetSignalChannel(ctx, SignalReconciliationResolution).Receive(ctx, &resolution)
		var resolved cvff.State
		if err := workflow.ExecuteActivity(ctx, ActivityResolveReconciliation, input.ApplicationID, resolution.PrincipalID, resolution.Resolution, cvff.StateDisbursementPending).Get(ctx, &resolved); err != nil {
			return result, fmt.Errorf("resolve reconciliation: %w", err)
		}
		result.State = resolved
		if resolved == cvff.StateRejected {
			return result, nil
		}
		if resolved != cvff.StateDisbursementPending {
			return result, fmt.Errorf("reconciliation resolution ended in %s, want %s", resolved, cvff.StateDisbursementPending)
		}
	}

	// Beneficiary confirmation of receipt closes the disbursement.
	var beneficiary cvff.Approval
	for {
		decision, err := awaitDecision(ctx, input.ApplicationID, SignalBeneficiaryConfirmation)
		if err != nil {
			resumed, parkErr := parkForReconciliation(ctx, input.ApplicationID, cvff.StateDisbursementPending)
			if parkErr != nil {
				return result, parkErr
			}
			result.State = resumed
			if resumed == cvff.StateRejected {
				return result, nil
			}
			continue
		}
		beneficiary = decision
		break
	}
	history = append(history, beneficiary)
	if beneficiary.Decision == cvff.DecisionReject {
		result.State = cvff.StateRejected
		return result, nil
	}
	result.State = beneficiary.ToState
	if result.State != cvff.StateDisbursed {
		return result, fmt.Errorf("beneficiary confirmation ended in %s, want %s", result.State, cvff.StateDisbursed)
	}
	if err := workflow.ExecuteActivity(ctx, ActivityCommitAudit, input.ApplicationID).Get(ctx, nil); err != nil {
		return result, fmt.Errorf("commit audit: %w", err)
	}
	result.State = cvff.StateAudited
	return result, nil
}

// awaitTierDecision waits for one underwriting tier decision. The per-tier SLA
// timer runs concurrently; on expiry an escalation activity records an audit
// event and the wait continues (fail-closed, never auto-approve).
func awaitTierDecision(ctx workflow.Context, applicationID string, stage underwritingStage) (cvff.State, cvff.Approval, int, error) {
	var approval cvff.Approval
	escalations := 0
	for {
		entered := workflow.Now(ctx)
		// Business-day deadlines are computed on the durable clock; the timer
		// uses calendar duration derived from the business-day deadline.
		deadline, err := cvff.SLADeadline(stage.tier, entered)
		if err != nil {
			return "", approval, escalations, err
		}
		timer := workflow.NewTimer(ctx, deadline.Sub(entered))
		signalChan := workflow.GetSignalChannel(ctx, stage.signal)
		var signal DecisionSignal
		selector := workflow.NewSelector(ctx)
		decided := false
		expired := false
		selector.AddReceive(signalChan, func(channel workflow.ReceiveChannel, more bool) {
			channel.Receive(ctx, &signal)
			decided = true
		})
		selector.AddFuture(timer, func(future workflow.Future) {
			expired = true
		})
		selector.Select(ctx)
		if decided {
			var state cvff.State
			err := workflow.ExecuteActivity(ctx, ActivityRecordDecision, applicationID, signal.PrincipalID, signal.Decision).Get(ctx, &state)
			if err != nil {
				return state, approval, escalations, fmt.Errorf("record %s decision: %w", stage.tier, err)
			}
			approval = cvff.Approval{
				ApplicationID: applicationID,
				Role:          roleForTier(stage.tier),
				PrincipalID:   signal.PrincipalID,
				Decision:      signal.Decision,
				ToState:       state,
			}
			return state, approval, escalations, nil
		}
		if expired {
			if err := workflow.ExecuteActivity(ctx, ActivityRecordEscalation, applicationID, stage.tier, deadline).Get(ctx, nil); err != nil {
				return "", approval, escalations, fmt.Errorf("record %s escalation: %w", stage.tier, err)
			}
			escalations++
			continue
		}
	}
}

// awaitDecision waits for a single party decision signal on non-timed stages.
func awaitDecision(ctx workflow.Context, applicationID, signal string) (cvff.Approval, error) {
	var payload DecisionSignal
	workflow.GetSignalChannel(ctx, signal).Receive(ctx, &payload)
	var state cvff.State
	if err := workflow.ExecuteActivity(ctx, ActivityRecordDecision, applicationID, payload.PrincipalID, payload.Decision).Get(ctx, &state); err != nil {
		return cvff.Approval{}, fmt.Errorf("record decision on %s: %w", signal, err)
	}
	return cvff.Approval{ApplicationID: applicationID, PrincipalID: payload.PrincipalID, Decision: payload.Decision, ToState: state}, nil
}

// parkForReconciliation moves the application into the fail-closed
// RECONCILIATION_REQUIRED branch and waits for the reconciliation officer's
// resolution. RESUME_DISBURSEMENT returns the application to the bound
// resumeTarget (the interrupted stage) so the correct party's decision can
// be recorded; REJECT closes the chain. An undeliverable resolution never
// abandons the branch — the workflow keeps waiting (H1).
func parkForReconciliation(ctx workflow.Context, applicationID string, resumeTarget cvff.State) (cvff.State, error) {
	if err := workflow.ExecuteActivity(ctx, ActivityRequireReconciliation, applicationID).Get(ctx, nil); err != nil {
		return "", fmt.Errorf("require reconciliation: %w", err)
	}
	for {
		var resolution ResolutionSignal
		workflow.GetSignalChannel(ctx, SignalReconciliationResolution).Receive(ctx, &resolution)
		var resolved cvff.State
		if err := workflow.ExecuteActivity(ctx, ActivityResolveReconciliation, applicationID, resolution.PrincipalID, resolution.Resolution, resumeTarget).Get(ctx, &resolved); err != nil {
			// Invalid or undeliverable resolutions are consumed and ignored;
			// the parked branch is never silently abandoned.
			continue
		}
		return resolved, nil
	}
}

// stateForTier maps an underwriting tier to the lifecycle state the
// application occupies while that tier's decision is awaited.
func stateForTier(tier cvff.UnderwritingTier) cvff.State {
	switch tier {
	case cvff.TierPrimary:
		return cvff.StateUnderwritingPrimary
	case cvff.TierSecondary:
		return cvff.StateUnderwritingSecondary
	default:
		return cvff.StateUnderwritingTertiary
	}
}

func roleForTier(tier cvff.UnderwritingTier) cvff.Role {
	switch tier {
	case cvff.TierPrimary:
		return cvff.RoleUnderwriterPrimary
	case cvff.TierSecondary:
		return cvff.RoleUnderwriterSecondary
	default:
		return cvff.RoleUnderwriterTertiary
	}
}
