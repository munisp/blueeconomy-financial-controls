package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"go.temporal.io/sdk/temporal"
)

// ApplicationStore is the durable control plane used by the activities.
type ApplicationStore interface {
	Get(ctx context.Context, applicationID string) (cvff.Application, error)
	RecordDecision(ctx context.Context, applicationID string, expectedVersion int64, principalID string, decision cvff.Decision) (cvff.Application, cvff.Approval, error)
	Transition(ctx context.Context, applicationID string, expectedVersion int64, move func(cvff.Application) (cvff.Application, error), eventType string) (cvff.Application, error)
	RecordEscalation(ctx context.Context, applicationID string, tier cvff.UnderwritingTier, deadline time.Time) error
}

// Disburser posts the FX-paired disbursement ledger entries. The production
// implementation pairs the CBN reference rate with TigerBeetle transfers.
type Disburser interface {
	Disburse(ctx context.Context, applicationID string) error
}

// NewActivities binds the workflow activities to the durable store and
// disbursement rail. It fails closed when either dependency is absent.
func NewActivities(store ApplicationStore, disburser Disburser) (*Activities, error) {
	if store == nil {
		return nil, errors.New("cvff application store is required")
	}
	if disburser == nil {
		return nil, errors.New("cvff disburser is required")
	}
	return &Activities{
		BeginUnderwriting: func(ctx context.Context, applicationID string) (cvff.State, error) {
			current, err := store.Get(ctx, applicationID)
			if err != nil {
				return "", err
			}
			updated, err := store.Transition(ctx, applicationID, current.Version, cvff.BeginUnderwriting, "cvff.underwriting_started")
			if err != nil {
				return "", activityError("begin underwriting", err)
			}
			return updated.State, nil
		},
		RecordDecision: func(ctx context.Context, applicationID, principalID string, decision cvff.Decision) (cvff.State, error) {
			current, err := store.Get(ctx, applicationID)
			if err != nil {
				return "", err
			}
			updated, _, err := store.RecordDecision(ctx, applicationID, current.Version, principalID, decision)
			if err != nil {
				return "", activityError("record decision", err)
			}
			return updated.State, nil
		},
		RecordEscalation: func(ctx context.Context, applicationID string, tier cvff.UnderwritingTier, deadline time.Time) error {
			return store.RecordEscalation(ctx, applicationID, tier, deadline)
		},
		Disburse: func(ctx context.Context, applicationID string) error {
			return disburser.Disburse(ctx, applicationID)
		},
		CommitAudit: func(ctx context.Context, applicationID string) error {
			current, err := store.Get(ctx, applicationID)
			if err != nil {
				return err
			}
			updated, err := store.Transition(ctx, applicationID, current.Version, cvff.MarkAudited, "cvff.audited")
			if err != nil {
				return activityError("commit audit", err)
			}
			if updated.State != cvff.StateAudited {
				return fmt.Errorf("audit close ended in %s", updated.State)
			}
			return nil
		},
	}, nil
}

// activityError marks state-machine rejections non-retryable so the workflow
// fails closed instead of retrying an invalid decision forever.
func activityError(operation string, err error) error {
	if errors.Is(err, cvff.ErrRoleNotAssigned) || errors.Is(err, cvff.ErrRoleSeparation) ||
		errors.Is(err, cvff.ErrSequenceViolation) || errors.Is(err, cvff.ErrTerminalState) ||
		errors.Is(err, cvff.ErrInvalidState) || errors.Is(err, cvff.ErrNotFound) {
		return temporal.NewNonRetryableApplicationError(operation, "cvff-control-rejection", err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
