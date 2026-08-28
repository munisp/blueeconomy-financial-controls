package cvffapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/munisp/blueeconomy-financial-controls/internal/workflow"
	"go.temporal.io/api/serviceerror"
	temporalclient "go.temporal.io/sdk/client"
)

// WorkflowStarter enters one recorded application into the disbursement rail.
// Implementations must be idempotent: re-starting an application that already
// has a running workflow is success, never an error.
type WorkflowStarter interface {
	StartDisbursement(ctx context.Context, applicationID string) error
}

// TemporalWorkflowName is the registered name of the CVFF disbursement
// workflow on the shared task queue.
const TemporalWorkflowName = "CVFFDisbursementWorkflow"

// TemporalStarter starts CVFFDisbursementWorkflow instances through the
// Temporal frontend. The workflow ID derives from the application ID so the
// start is naturally idempotent: an already-started execution maps to
// success, which makes intake retries with the same Idempotency-Key safe.
type TemporalStarter struct {
	client    temporalclient.Client
	taskQueue string
}

// NewTemporalStarter fails closed when the client or task queue is absent.
func NewTemporalStarter(client temporalclient.Client, taskQueue string) (*TemporalStarter, error) {
	if client == nil {
		return nil, errors.New("temporal client is required")
	}
	if taskQueue == "" || strings.TrimSpace(taskQueue) != taskQueue {
		return nil, errors.New("temporal task queue is required and must be canonical")
	}
	return &TemporalStarter{client: client, taskQueue: taskQueue}, nil
}

// WorkflowIDFor is the deterministic disbursement workflow ID of one
// application: cvff-disbursement-<application_id>. Every start and signal
// for the application addresses this ID, which is what makes restarts and
// retries naturally idempotent.
func WorkflowIDFor(applicationID string) string {
	return "cvff-disbursement-" + applicationID
}

func (starter *TemporalStarter) StartDisbursement(ctx context.Context, applicationID string) error {
	if applicationID == "" || strings.TrimSpace(applicationID) != applicationID {
		return errors.New("application ID is required and must be canonical")
	}
	_, err := starter.client.ExecuteWorkflow(ctx, temporalclient.StartWorkflowOptions{
		ID:        WorkflowIDFor(applicationID),
		TaskQueue: starter.taskQueue,
	}, TemporalWorkflowName, workflow.DisbursementInput{ApplicationID: applicationID})
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return nil
		}
		return fmt.Errorf("start disbursement workflow: %w", err)
	}
	return nil
}

// ErrWorkflowNotRunning marks a signal addressed to an application whose
// disbursement workflow has no running execution. It is surfaced as a
// retryable service error, never silently dropped.
var ErrWorkflowNotRunning = errors.New("disbursement workflow is not running for this application")

// WorkflowSignaler delivers party decisions and reconciliation resolutions
// to the application's deterministic disbursement workflow.
type WorkflowSignaler interface {
	// SignalDecision delivers one four-party decision signal.
	SignalDecision(ctx context.Context, applicationID, signalName string, signal workflow.DecisionSignal) error
	// SignalResolution delivers one reconciliation-resolution signal to a
	// workflow parked in RECONCILIATION_REQUIRED.
	SignalResolution(ctx context.Context, applicationID string, signal workflow.ResolutionSignal) error
}

// TemporalSignaler signals the deterministic disbursement workflow through
// the Temporal frontend. Decision signals start the workflow when it has no
// running execution yet (signal-with-start on the deterministic ID), so an
// application whose start was lost still advances; resolution signals never
// start a workflow, because a resolution is only meaningful to a workflow
// parked in RECONCILIATION_REQUIRED.
type TemporalSignaler struct {
	client    temporalclient.Client
	taskQueue string
}

// NewTemporalSignaler fails closed when the client or task queue is absent.
func NewTemporalSignaler(client temporalclient.Client, taskQueue string) (*TemporalSignaler, error) {
	if client == nil {
		return nil, errors.New("temporal client is required")
	}
	if taskQueue == "" || strings.TrimSpace(taskQueue) != taskQueue {
		return nil, errors.New("temporal task queue is required and must be canonical")
	}
	return &TemporalSignaler{client: client, taskQueue: taskQueue}, nil
}

func (signaler *TemporalSignaler) SignalDecision(ctx context.Context, applicationID, signalName string, signal workflow.DecisionSignal) error {
	if applicationID == "" || signalName == "" {
		return errors.New("application ID and signal name are required")
	}
	err := signaler.client.SignalWorkflow(ctx, WorkflowIDFor(applicationID), "", signalName, signal)
	if err == nil {
		return nil
	}
	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		return fmt.Errorf("signal disbursement workflow: %w", err)
	}
	// No running execution: start-and-signal on the deterministic ID. The
	// workflow begins at underwriting; the signal is buffered until the
	// chain reaches its decision point.
	_, err = signaler.client.SignalWithStartWorkflow(ctx,
		WorkflowIDFor(applicationID), signalName, signal,
		temporalclient.StartWorkflowOptions{ID: WorkflowIDFor(applicationID), TaskQueue: signaler.taskQueue},
		TemporalWorkflowName, workflow.DisbursementInput{ApplicationID: applicationID})
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			// Lost the start race: the winner's execution takes the signal.
			if err := signaler.client.SignalWorkflow(ctx, WorkflowIDFor(applicationID), "", signalName, signal); err != nil {
				return fmt.Errorf("signal disbursement workflow after start race: %w", err)
			}
			return nil
		}
		return fmt.Errorf("signal-with-start disbursement workflow: %w", err)
	}
	return nil
}

func (signaler *TemporalSignaler) SignalResolution(ctx context.Context, applicationID string, signal workflow.ResolutionSignal) error {
	if applicationID == "" {
		return errors.New("application ID is required")
	}
	err := signaler.client.SignalWorkflow(ctx, WorkflowIDFor(applicationID), "", workflow.SignalReconciliationResolution, signal)
	if err == nil {
		return nil
	}
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return ErrWorkflowNotRunning
	}
	return fmt.Errorf("signal reconciliation resolution: %w", err)
}
