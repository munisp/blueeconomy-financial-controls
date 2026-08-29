package orchestration

import (
	"context"
	"errors"
	"fmt"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
)

// ResolutionStore is the persistence boundary for officer resolution of
// AMBIGUOUS intents.
type ResolutionStore interface {
	Get(context.Context, string) (intent.Intent, error)
	ResolveAmbiguous(ctx context.Context, intentID string, expectedVersion int64, officer string, resolution intent.Resolution) (intent.Intent, error)
}

// Resolver applies officer dispositions to AMBIGUOUS intents with the
// compensating ledger handling the dispositions require. AMBIGUOUS means the
// evidence-driven reconciler found contradictory or missing TigerBeetle
// evidence, so a human financial controller decides; the resolver never
// guesses and never voids money that observably moved.
type Resolver struct {
	store  ResolutionStore
	ledger *ledger.Service
}

// NewResolver fails closed when any dependency is absent.
func NewResolver(store ResolutionStore, ledgerService *ledger.Service) (*Resolver, error) {
	if store == nil {
		return nil, errors.New("intent store is required")
	}
	if ledgerService == nil {
		return nil, errors.New("TigerBeetle ledger service is required")
	}
	return &Resolver{store: store, ledger: ledgerService}, nil
}

// ResolveAmbiguous applies the officer disposition:
//
//   - RECONCILE returns the intent to RECONCILIATION_REQUIRED so the
//     evidence-driven reconciler re-examines the TigerBeetle records.
//   - VOID performs the compensating ledger handling before recording
//     VOIDED: an observed post transfer means the money moved and voiding is
//     refused (the officer must reconcile instead); an outstanding pending
//     reservation is voided idempotently with the deterministic void
//     transfer so no funds stay locked.
//
// The officer identity is the verified token subject and is recorded in the
// financial_intent.officer_resolved audit event by the store.
func (resolver *Resolver) ResolveAmbiguous(ctx context.Context, intentID string, expectedVersion int64, officer string, resolution intent.Resolution) (intent.Intent, error) {
	if resolution != intent.ResolutionReconcile && resolution != intent.ResolutionVoid {
		return intent.Intent{}, intent.ErrInvalidResolution
	}
	current, err := resolver.store.Get(ctx, intentID)
	if err != nil {
		return intent.Intent{}, err
	}
	if current.State != intent.StateAmbiguous {
		return intent.Intent{}, intent.ErrInvalidState
	}
	if resolution == intent.ResolutionReconcile {
		return resolver.store.ResolveAmbiguous(ctx, intentID, expectedVersion, officer, resolution)
	}
	postID := deterministicID("post", intentID)
	if _, postFound, lookupErr := resolver.ledger.LookupTransfer(postID); lookupErr != nil {
		return intent.Intent{}, fmt.Errorf("lookup post transfer: %w", lookupErr)
	} else if postFound {
		// The money observably moved; voiding would erase a real movement.
		return intent.Intent{}, intent.ErrResolutionRejected
	}
	pendingID := deterministicID("pending", intentID)
	voidID := deterministicID("void", intentID)
	_, pendingFound, err := resolver.ledger.LookupTransfer(pendingID)
	if err != nil {
		return intent.Intent{}, fmt.Errorf("lookup pending transfer: %w", err)
	}
	_, voidFound, err := resolver.ledger.LookupTransfer(voidID)
	if err != nil {
		return intent.Intent{}, fmt.Errorf("lookup void transfer: %w", err)
	}
	if pendingFound && !voidFound {
		// Compensating handling: release the locked reservation before the
		// intent is recorded VOIDED.
		if voidErr := resolver.ledger.Void(voidID, pendingID); voidErr != nil {
			return intent.Intent{}, fmt.Errorf("void pending reservation: %w", voidErr)
		}
	}
	return resolver.store.ResolveAmbiguous(ctx, intentID, expectedVersion, officer, resolution)
}
