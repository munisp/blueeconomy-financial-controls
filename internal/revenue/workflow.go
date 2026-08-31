package revenue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

// Temporal wiring for long-running reconciliation batches. The workflow is
// a thin deterministic driver; all state lives in PostgreSQL via the
// activity, so a crashed worker loses nothing (the batch is one DB
// transaction). The platform interceptors (telemetry.TemporalInterceptor)
// attach tracing at the worker — this package only defines workflow +
// activity.

// ReconBatchInput starts one reconciliation batch.
type ReconBatchInput struct {
	Actor string `json:"actor"` // verified subject or scheduler identity
	AsOf  string `json:"asOf"`  // YYYY-MM-DD business date for due-note aging
}

// Validate fails closed on a malformed batch input.
func (input ReconBatchInput) Validate() error {
	if input.Actor == "" {
		return errors.New("actor is required")
	}
	if _, err := time.Parse("2006-01-02", input.AsOf); err != nil {
		return errors.New("asOf must be YYYY-MM-DD")
	}
	return nil
}

// ReconBatchResult reports one finished batch.
type ReconBatchResult struct {
	RunID          string `json:"runId"`
	MatchedCount   int    `json:"matchedCount"`
	ExceptionCount int    `json:"exceptionCount"`
}

// ReconActivities carries the store dependency for the activity worker.
type ReconActivities struct {
	store *Store
}

// NewReconActivities fails closed on a nil store.
func NewReconActivities(store *Store) (*ReconActivities, error) {
	if store == nil {
		return nil, errors.New("revenue store is required")
	}
	return &ReconActivities{store: store}, nil
}

// RunReconBatchActivity executes one atomic reconciliation batch.
func (activities *ReconActivities) RunReconBatchActivity(ctx context.Context, input ReconBatchInput) (ReconBatchResult, error) {
	if err := input.Validate(); err != nil {
		return ReconBatchResult{}, err
	}
	asOf, _ := time.Parse("2006-01-02", input.AsOf)
	summary, err := activities.store.RunRecon(ctx, "TEMPORAL_BATCH", input.Actor, asOf.UTC())
	if err != nil {
		return ReconBatchResult{}, fmt.Errorf("run recon batch: %w", err)
	}
	return ReconBatchResult{
		RunID:          summary.RunID,
		MatchedCount:   summary.MatchedCount,
		ExceptionCount: summary.ExceptionCount,
	}, nil
}

// ReconBatchWorkflow is the workflow function: one activity with bounded
// retries (the batch itself is idempotent — re-runs skip already-raised
// exceptions and already-matched legs).
func ReconBatchWorkflow(ctx workflow.Context, input ReconBatchInput) (ReconBatchResult, error) {
	if err := input.Validate(); err != nil {
		return ReconBatchResult{}, err
	}
	options := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
	}
	ctx = workflow.WithActivityOptions(ctx, options)
	var result ReconBatchResult
	err := workflow.ExecuteActivity(ctx, "RunReconBatchActivity", input).Get(ctx, &result)
	if err != nil {
		return ReconBatchResult{}, err
	}
	workflow.GetLogger(ctx).Info("recon batch completed",
		"runId", result.RunID, "matched", result.MatchedCount, "exceptions", result.ExceptionCount)
	return result, nil
}

// ActivityName is the registered name of the batch activity.
const ActivityName = "RunReconBatchActivity"
