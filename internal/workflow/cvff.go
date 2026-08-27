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
	// SignalUnderwritingDecision carries one consortium tier decision.
	SignalUnderwritingDecision = "cvff.underwriting-decision"
	// SignalNIMASADecision carries the NIMASA approver decision.
	SignalNIMASADecision = "cvff.nimasa-decision"
	// SignalBankConfirmation carries the receiving-bank confirmation.
	SignalBankConfirmation = "cvff.bank-confirmation"
	// SignalBeneficiaryConfirmation carries the beneficiary confirmation.
	SignalBeneficiaryConfirmation = "cvff.beneficiary-confirmation"

	// QueryState returns the current lifecycle state for observer replay.
	QueryState = "cvff.state"
	// QueryHistory returns the recorded decision entries for observer replay.
	QueryHistory = "cvff.history"
)

// Stable activity names: workflow histories reference these across deployments.
const (
	ActivityBeginUnderwriting = "cvff.begin-underwriting"
	ActivityRecordDecision    = "cvff.record-decision"
	ActivityRecordEscalation  = "cvff.record-escalation"
	ActivityDisburse          = "cvff.disburse"
	ActivityCommitAudit       = "cvff.commit-audit"
)

// DecisionSignal is the payload for every party decision signal. PrincipalID
// is the Keycloak subject of the deciding party.
type DecisionSignal struct {
	PrincipalID string        `json:"principal_id"`
	Decision    cvff.Decision `json:"decision"`
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
	// Disburse posts the FX-paired disbursement ledger entries.
	Disburse func(ctx context.Context, applicationID string) error
	// CommitAudit closes the lifecycle for a disbursed application.
	CommitAudit func(ctx context.Context, applicationID string) error
}

// underwritingStage describes one tier wait: signal name, tier and SLA.
type underwritingStage struct {
	tier   cvff.UnderwritingTier
	signal string
}

var underwritingStages = []underwritingStage{
	{cvff.TierPrimary, SignalUnderwritingDecision},
	{cvff.TierSecondary, SignalUnderwritingDecision},
	{cvff.TierTertiary, SignalUnderwritingDecision},
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

	for _, stage := range underwritingStages {
		state, decision, escalations, err := awaitTierDecision(ctx, input.ApplicationID, stage)
		if err != nil {
			result.State = state
			return result, err
		}
		result.State = state
		result.Escalations += escalations
		history = append(history, decision)
		if result.State == cvff.StateRejected {
			return result, nil
		}
	}

	// NIMASA approval, then receiving-bank confirmation.
	for _, signal := range []string{SignalNIMASADecision, SignalBankConfirmation} {
		decision, err := awaitDecision(ctx, input.ApplicationID, signal)
		if err != nil {
			return result, err
		}
		history = append(history, decision)
		if decision.Decision == cvff.DecisionReject {
			result.State = cvff.StateRejected
			return result, nil
		}
		result.State = decision.ToState
	}

	// Disbursement executes only after NIMASA and the receiving bank approved.
	if result.State != cvff.StateDisbursementPending {
		return result, fmt.Errorf("approval chain ended in %s, want %s", result.State, cvff.StateDisbursementPending)
	}
	if err := workflow.ExecuteActivity(ctx, ActivityDisburse, input.ApplicationID).Get(ctx, nil); err != nil {
		return result, fmt.Errorf("disburse: %w", err)
	}

	// Beneficiary confirmation of receipt closes the disbursement.
	beneficiary, err := awaitDecision(ctx, input.ApplicationID, SignalBeneficiaryConfirmation)
	if err != nil {
		return result, err
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
