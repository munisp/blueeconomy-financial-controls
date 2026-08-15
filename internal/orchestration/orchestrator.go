package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

var ErrIntentNotReady = errors.New("financial intent is not approved for reservation")

type IntentStore interface {
	Get(context.Context, string) (intent.Intent, error)
	Transition(context.Context, string, int64, intent.State) (intent.Intent, error)
}

type Orchestrator struct {
	store          IntentStore
	ledger         *ledger.Service
	pendingTimeout uint32
}

func New(store IntentStore, ledgerService *ledger.Service, pendingTimeout uint32) (*Orchestrator, error) {
	if store == nil {
		return nil, errors.New("intent store is required")
	}
	if ledgerService == nil {
		return nil, errors.New("TigerBeetle ledger service is required")
	}
	if pendingTimeout == 0 {
		return nil, errors.New("pending timeout must be non-zero")
	}
	return &Orchestrator{store: store, ledger: ledgerService, pendingTimeout: pendingTimeout}, nil
}

func (orchestrator *Orchestrator) ReserveApproved(ctx context.Context, intentID string) (intent.Intent, error) {
	current, err := orchestrator.store.Get(ctx, intentID)
	if err != nil {
		return intent.Intent{}, err
	}
	if current.State == intent.StateReserved || current.State == intent.StatePosted {
		return current, nil
	}
	if current.State != intent.StateApproved {
		return intent.Intent{}, ErrIntentNotReady
	}
	requested, err := orchestrator.store.Transition(ctx, current.IntentID, current.Version, intent.StateReservationRequested)
	if err != nil {
		return intent.Intent{}, err
	}
	debitID, err := parseID(requested.DebitAccountID)
	if err != nil {
		return orchestrator.reconcile(ctx, requested, fmt.Errorf("debit account ID: %w", err))
	}
	creditID, err := parseID(requested.CreditAccountID)
	if err != nil {
		return orchestrator.reconcile(ctx, requested, fmt.Errorf("credit account ID: %w", err))
	}
	transferID := deterministicID("pending", requested.IntentID)
	if _, found, lookupErr := orchestrator.ledger.LookupTransfer(transferID); lookupErr != nil {
		return orchestrator.reconcile(ctx, requested, fmt.Errorf("lookup pending transfer: %w", lookupErr))
	} else if !found {
		if reserveErr := orchestrator.ledger.Reserve(transferID, debitID, creditID, requested.Amount, orchestrator.pendingTimeout); reserveErr != nil {
			return orchestrator.reconcile(ctx, requested, fmt.Errorf("reserve pending transfer: %w", reserveErr))
		}
	}
	reserved, err := orchestrator.store.Transition(ctx, requested.IntentID, requested.Version, intent.StateReserved)
	if err != nil {
		return orchestrator.reconcile(ctx, requested, fmt.Errorf("record reserved state: %w", err))
	}
	return reserved, nil
}

func (orchestrator *Orchestrator) reconcile(ctx context.Context, current intent.Intent, cause error) (intent.Intent, error) {
	updated, transitionErr := orchestrator.store.Transition(ctx, current.IntentID, current.Version, intent.StateReconciliationRequired)
	if transitionErr != nil {
		return intent.Intent{}, fmt.Errorf("%v; failed to record reconciliation state: %w", cause, transitionErr)
	}
	return updated, fmt.Errorf("%v; intent moved to reconciliation-required", cause)
}

func parseID(value string) (tigerbeetle.Uint128, error) {
	id, err := tigerbeetle.HexStringToUint128(value)
	if err != nil || id == (tigerbeetle.Uint128{}) {
		if err == nil {
			err = errors.New("zero TigerBeetle ID")
		}
		return tigerbeetle.Uint128{}, err
	}
	return id, nil
}

func deterministicID(namespace, intentID string) tigerbeetle.Uint128 {
	digest := sha256.Sum256([]byte(namespace + ":" + intentID))
	id, err := tigerbeetle.HexStringToUint128(hex.EncodeToString(digest[:16]))
	if err != nil {
		panic(fmt.Sprintf("deterministic TigerBeetle ID encoding failed: %v", err))
	}
	return id
}

func (orchestrator *Orchestrator) PendingTransferID(intentID string) tigerbeetle.Uint128 {
	return deterministicID("pending", intentID)
}

func (orchestrator *Orchestrator) Now() time.Time { return time.Now().UTC() }
