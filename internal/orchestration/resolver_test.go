package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

// fakeResolutionStore records officer resolutions in memory.
type fakeResolutionStore struct {
	current    intent.Intent
	resolved   intent.Resolution
	officer    string
	resolveErr error
}

func (store *fakeResolutionStore) Get(context.Context, string) (intent.Intent, error) {
	return store.current, nil
}

func (store *fakeResolutionStore) ResolveAmbiguous(_ context.Context, _ string, expectedVersion int64, officer string, resolution intent.Resolution) (intent.Intent, error) {
	if store.resolveErr != nil {
		return intent.Intent{}, store.resolveErr
	}
	updated, err := intent.ResolveAmbiguous(store.current, expectedVersion, officer, resolution)
	if err != nil {
		return intent.Intent{}, err
	}
	store.current = updated
	store.current.Version++
	store.resolved = resolution
	store.officer = officer
	return store.current, nil
}

func ambiguousStoredIntent() intent.Intent {
	current := approvedIntent()
	current.State = intent.StateAmbiguous
	current.Version = 4
	return current
}

func newResolverForTest(t *testing.T, store ResolutionStore, client *fakeLedger) *Resolver {
	t.Helper()
	ledgerService, err := ledger.New(client, 1, 1)
	if err != nil {
		t.Fatalf("ledger service: %v", err)
	}
	resolver, err := NewResolver(store, ledgerService)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	return resolver
}

func TestNewResolverFailsClosed(t *testing.T) {
	ledgerService, err := ledger.New(newFakeLedger(), 1, 1)
	if err != nil {
		t.Fatalf("ledger service: %v", err)
	}
	if _, err := NewResolver(nil, ledgerService); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewResolver(&fakeResolutionStore{}, nil); err == nil {
		t.Fatal("nil ledger accepted")
	}
}

func TestResolveAmbiguousReconcile(t *testing.T) {
	store := &fakeResolutionStore{current: ambiguousStoredIntent()}
	resolver := newResolverForTest(t, store, newFakeLedger())
	updated, err := resolver.ResolveAmbiguous(context.Background(), "intent-001", 4, "officer-001", intent.ResolutionReconcile)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if updated.State != intent.StateReconciliationRequired || store.officer != "officer-001" {
		t.Fatalf("unexpected resolution: %+v officer %q", updated, store.officer)
	}
}

func TestResolveAmbiguousVoidWithoutReservation(t *testing.T) {
	store := &fakeResolutionStore{current: ambiguousStoredIntent()}
	resolver := newResolverForTest(t, store, newFakeLedger())
	updated, err := resolver.ResolveAmbiguous(context.Background(), "intent-001", 4, "officer-001", intent.ResolutionVoid)
	if err != nil {
		t.Fatalf("void without reservation: %v", err)
	}
	if updated.State != intent.StateVoided {
		t.Fatalf("state = %s, want VOIDED", updated.State)
	}
}

func TestResolveAmbiguousVoidCompensatesOutstandingReservation(t *testing.T) {
	client := newFakeLedger()
	reservation := tigerbeetle.Transfer{ID: deterministicID("pending", "intent-001"), Ledger: 1, Code: 1}
	client.transfers[reservation.ID] = reservation
	store := &fakeResolutionStore{current: ambiguousStoredIntent()}
	resolver := newResolverForTest(t, store, client)
	updated, err := resolver.ResolveAmbiguous(context.Background(), "intent-001", 4, "officer-001", intent.ResolutionVoid)
	if err != nil {
		t.Fatalf("void with reservation: %v", err)
	}
	if updated.State != intent.StateVoided {
		t.Fatalf("state = %s, want VOIDED", updated.State)
	}
	voidID := deterministicID("void", "intent-001")
	compensation, found := client.transfers[voidID]
	if !found {
		t.Fatal("no compensating void transfer recorded for the outstanding reservation")
	}
	if compensation.PendingID != reservation.ID {
		t.Fatalf("compensating void targets %s, want the pending reservation", compensation.PendingID)
	}
}

func TestResolveAmbiguousVoidRefusedWhenPosted(t *testing.T) {
	client := newFakeLedger()
	postID := deterministicID("post", "intent-001")
	client.transfers[postID] = tigerbeetle.Transfer{ID: postID, Ledger: 1, Code: 1}
	store := &fakeResolutionStore{current: ambiguousStoredIntent()}
	resolver := newResolverForTest(t, store, client)
	if _, err := resolver.ResolveAmbiguous(context.Background(), "intent-001", 4, "officer-001", intent.ResolutionVoid); !errors.Is(err, intent.ErrResolutionRejected) {
		t.Fatalf("void after post error = %v, want ErrResolutionRejected", err)
	}
	if store.resolved != "" {
		t.Fatal("state changed despite posted funds")
	}
}

func TestResolveAmbiguousFailClosed(t *testing.T) {
	store := &fakeResolutionStore{current: ambiguousStoredIntent()}
	resolver := newResolverForTest(t, store, newFakeLedger())
	if _, err := resolver.ResolveAmbiguous(context.Background(), "intent-001", 4, "officer-001", intent.Resolution("FORCE_POST")); !errors.Is(err, intent.ErrInvalidResolution) {
		t.Fatalf("unknown resolution error = %v", err)
	}
	draftStore := &fakeResolutionStore{current: approvedIntent()}
	draftResolver := newResolverForTest(t, draftStore, newFakeLedger())
	if _, err := draftResolver.ResolveAmbiguous(context.Background(), "intent-001", 2, "officer-001", intent.ResolutionVoid); !errors.Is(err, intent.ErrInvalidState) {
		t.Fatalf("non-ambiguous resolution error = %v", err)
	}
}
