package orchestration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

// fakeStore drives intent state in memory.
type fakeStore struct {
	current       intent.Intent
	transitionErr error
	calls         []intent.State
}

func (store *fakeStore) Get(context.Context, string) (intent.Intent, error) {
	return store.current, nil
}

func (store *fakeStore) Transition(_ context.Context, _ string, _ int64, next intent.State) (intent.Intent, error) {
	if store.transitionErr != nil {
		return intent.Intent{}, store.transitionErr
	}
	if !intent.ValidOperationalTransition(store.current.State, next) {
		return intent.Intent{}, intent.ErrConflict
	}
	store.current.State = next
	store.current.Version++
	store.calls = append(store.calls, next)
	return store.current, nil
}

// fakeLedger records TigerBeetle operations.
type fakeLedger struct {
	transfers map[tigerbeetle.Uint128]tigerbeetle.Transfer
	createErr error
	lookupErr error
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{transfers: map[tigerbeetle.Uint128]tigerbeetle.Transfer{}}
}

func (client *fakeLedger) CreateAccounts([]tigerbeetle.Account) ([]tigerbeetle.CreateAccountResult, error) {
	return nil, errors.New("not implemented")
}

func (client *fakeLedger) CreateTransfers(transfers []tigerbeetle.Transfer) ([]tigerbeetle.CreateTransferResult, error) {
	if client.createErr != nil {
		return nil, client.createErr
	}
	results := make([]tigerbeetle.CreateTransferResult, 0, len(transfers))
	for _, transfer := range transfers {
		if _, exists := client.transfers[transfer.ID]; exists {
			results = append(results, tigerbeetle.CreateTransferResult{Status: tigerbeetle.TransferExists})
			continue
		}
		client.transfers[transfer.ID] = transfer
		results = append(results, tigerbeetle.CreateTransferResult{Status: tigerbeetle.TransferCreated})
	}
	return results, nil
}

func (client *fakeLedger) LookupTransfers(ids []tigerbeetle.Uint128) ([]tigerbeetle.Transfer, error) {
	if client.lookupErr != nil {
		return nil, client.lookupErr
	}
	found := make([]tigerbeetle.Transfer, 0, len(ids))
	for _, id := range ids {
		if transfer, ok := client.transfers[id]; ok {
			found = append(found, transfer)
		}
	}
	return found, nil
}

func approvedIntent() intent.Intent {
	return intent.Intent{
		CreateRequest: intent.CreateRequest{
			IntentID:        "intent-001",
			ExternalRef:     "ref-001",
			DebitAccountID:  fmt.Sprintf("%032x", 11),
			CreditAccountID: fmt.Sprintf("%032x", 22),
			Amount:          1_000,
			Ledger:          1,
			Code:            1,
			Currency:        "NGN",
			Maker:           "maker-001",
		},
		State:   intent.StateApproved,
		Version: 2,
	}
}

func newOrchestrator(t *testing.T, store *fakeStore, client *fakeLedger) *Orchestrator {
	t.Helper()
	ledgerService, err := ledger.New(client, 1, 1)
	if err != nil {
		t.Fatalf("ledger service: %v", err)
	}
	orchestrator, err := New(store, ledgerService, 60)
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	return orchestrator
}

func TestNewFailsClosed(t *testing.T) {
	client := newFakeLedger()
	ledgerService, _ := ledger.New(client, 1, 1)
	if _, err := New(nil, ledgerService, 60); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := New(&fakeStore{}, nil, 60); err == nil {
		t.Fatal("nil ledger accepted")
	}
	if _, err := New(&fakeStore{}, ledgerService, 0); err == nil {
		t.Fatal("zero timeout accepted")
	}
}

func TestReserveApprovedHappyPath(t *testing.T) {
	store := &fakeStore{current: approvedIntent()}
	orchestrator := newOrchestrator(t, store, newFakeLedger())
	updated, err := orchestrator.ReserveApproved(context.Background(), "intent-001")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if updated.State != intent.StateReserved {
		t.Fatalf("state = %s", updated.State)
	}
	// Idempotent: a second call returns the reserved intent without error.
	again, err := orchestrator.ReserveApproved(context.Background(), "intent-001")
	if err != nil || again.State != intent.StateReserved {
		t.Fatalf("replay reserve = %s, %v", again.State, err)
	}
}

func TestReserveApprovedRequiresApproval(t *testing.T) {
	store := &fakeStore{current: approvedIntent()}
	store.current.State = intent.StateDraft
	orchestrator := newOrchestrator(t, store, newFakeLedger())
	if _, err := orchestrator.ReserveApproved(context.Background(), "intent-001"); !errors.Is(err, ErrIntentNotReady) {
		t.Fatalf("draft reserve error = %v", err)
	}
}

func TestReserveLedgerFailureMovesToReconciliation(t *testing.T) {
	store := &fakeStore{current: approvedIntent()}
	client := newFakeLedger()
	client.createErr = errors.New("cluster unavailable")
	orchestrator := newOrchestrator(t, store, client)
	if _, err := orchestrator.ReserveApproved(context.Background(), "intent-001"); err == nil {
		t.Fatal("ledger failure swallowed")
	}
	if store.current.State != intent.StateReconciliationRequired {
		t.Fatalf("state = %s, want RECONCILIATION_REQUIRED", store.current.State)
	}
}

func TestPostReservedHappyPath(t *testing.T) {
	store := &fakeStore{current: approvedIntent()}
	client := newFakeLedger()
	orchestrator := newOrchestrator(t, store, client)
	if _, err := orchestrator.ReserveApproved(context.Background(), "intent-001"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	posted, err := orchestrator.PostReserved(context.Background(), "intent-001")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if posted.State != intent.StatePosted {
		t.Fatalf("state = %s", posted.State)
	}
	// Idempotent replay.
	again, err := orchestrator.PostReserved(context.Background(), "intent-001")
	if err != nil || again.State != intent.StatePosted {
		t.Fatalf("replay post = %s, %v", again.State, err)
	}
	// Posting twice must not void.
	if _, err := orchestrator.VoidReserved(context.Background(), "intent-001"); !errors.Is(err, ErrIntentNotReady) {
		t.Fatalf("void after post error = %v", err)
	}
}

func TestVoidReservedHappyPath(t *testing.T) {
	store := &fakeStore{current: approvedIntent()}
	orchestrator := newOrchestrator(t, store, newFakeLedger())
	if _, err := orchestrator.ReserveApproved(context.Background(), "intent-001"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	voided, err := orchestrator.VoidReserved(context.Background(), "intent-001")
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if voided.State != intent.StateVoided {
		t.Fatalf("state = %s", voided.State)
	}
}

func TestReconcileObserved(t *testing.T) {
	store := &fakeStore{current: approvedIntent()}
	client := newFakeLedger()
	client.createErr = errors.New("cluster unavailable")
	orchestrator := newOrchestrator(t, store, client)
	if _, err := orchestrator.ReserveApproved(context.Background(), "intent-001"); err == nil {
		t.Fatal("ledger failure swallowed")
	}
	// No evidence exists: the intent becomes AMBIGUOUS, not guessed.
	repaired, err := orchestrator.ReconcileObserved(context.Background(), "intent-001")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if repaired.State != intent.StateAmbiguous {
		t.Fatalf("state = %s, want AMBIGUOUS", repaired.State)
	}

	// With a posted transfer observed, reconciliation repairs to POSTED.
	store = &fakeStore{current: approvedIntent()}
	client = newFakeLedger()
	orchestrator = newOrchestrator(t, store, client)
	if _, err := orchestrator.ReserveApproved(context.Background(), "intent-001"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := orchestrator.PostReserved(context.Background(), "intent-001"); err != nil {
		t.Fatalf("post: %v", err)
	}
	store.current.State = intent.StateReconciliationRequired
	repaired, err = orchestrator.ReconcileObserved(context.Background(), "intent-001")
	if err != nil {
		t.Fatalf("reconcile posted: %v", err)
	}
	if repaired.State != intent.StatePosted {
		t.Fatalf("state = %s, want POSTED", repaired.State)
	}
}

func TestReconcileObservedLookupFailureStaysFailClosed(t *testing.T) {
	store := &fakeStore{current: approvedIntent()}
	store.current.State = intent.StateReconciliationRequired
	client := newFakeLedger()
	client.lookupErr = errors.New("cluster unavailable")
	orchestrator := newOrchestrator(t, store, client)
	if _, err := orchestrator.ReconcileObserved(context.Background(), "intent-001"); err == nil {
		t.Fatal("lookup failure swallowed")
	}
	if store.current.State != intent.StateReconciliationRequired {
		t.Fatalf("state = %s, want RECONCILIATION_REQUIRED", store.current.State)
	}
}

func TestDeterministicTransferIDs(t *testing.T) {
	orchestrator := newOrchestrator(t, &fakeStore{current: approvedIntent()}, newFakeLedger())
	pending := orchestrator.PendingTransferID("intent-001")
	post := orchestrator.PostTransferID("intent-001")
	void := orchestrator.VoidTransferID("intent-001")
	zero := tigerbeetle.Uint128{}
	if pending == zero || post == zero || void == zero {
		t.Fatal("zero deterministic ID")
	}
	if pending == post || pending == void || post == void {
		t.Fatal("deterministic IDs collide")
	}
	other := newOrchestrator(t, &fakeStore{current: approvedIntent()}, newFakeLedger())
	if other.PendingTransferID("intent-001") != pending {
		t.Fatal("pending ID is not deterministic")
	}
	if other.PendingTransferID("intent-002") == pending {
		t.Fatal("pending ID ignores intent ID")
	}
}
