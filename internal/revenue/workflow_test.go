package revenue

import (
	"context"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// stubBatchActivity stands in for the DB-gated activity inside the Temporal
// test environment (activity logic itself is covered by the integration
// suite; here only the workflow driver is under test).
func stubBatchActivity(_ context.Context, input ReconBatchInput) (ReconBatchResult, error) {
	if err := input.Validate(); err != nil {
		return ReconBatchResult{}, err
	}
	return ReconBatchResult{RunID: "run-1", MatchedCount: 3, ExceptionCount: 2}, nil
}

// TestReconBatchWorkflow drives the workflow in the Temporal test
// environment with the activity mocked at the environment boundary (the
// activity itself is covered DB-gated; here we prove the workflow driver:
// input validation, activity invocation, result propagation).
func TestReconBatchWorkflow(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	input := ReconBatchInput{Actor: "scheduler:nightly", AsOf: "2026-08-30"}
	expected := ReconBatchResult{RunID: "run-1", MatchedCount: 3, ExceptionCount: 2}
	env.RegisterActivityWithOptions(stubBatchActivity, activity.RegisterOptions{Name: ActivityName})
	env.ExecuteWorkflow(ReconBatchWorkflow, input)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result ReconBatchResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result != expected {
		t.Fatalf("result %+v, want %+v", result, expected)
	}
}

// TestReconBatchWorkflowFailsClosedOnBadInput refuses a malformed batch.
func TestReconBatchWorkflowFailsClosedOnBadInput(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(ReconBatchWorkflow, ReconBatchInput{Actor: "", AsOf: "not-a-date"})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if env.GetWorkflowError() == nil {
		t.Fatal("invalid input must fail the workflow before any activity")
	}
}
