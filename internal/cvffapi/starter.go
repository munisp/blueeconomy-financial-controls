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

func (starter *TemporalStarter) StartDisbursement(ctx context.Context, applicationID string) error {
	if applicationID == "" || strings.TrimSpace(applicationID) != applicationID {
		return errors.New("application ID is required and must be canonical")
	}
	_, err := starter.client.ExecuteWorkflow(ctx, temporalclient.StartWorkflowOptions{
		ID:        "cvff-disbursement-" + applicationID,
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
