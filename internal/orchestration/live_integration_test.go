//go:build liveintegration

package orchestration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

func TestReserveApprovedAgainstLiveTigerBeetle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := intent.Open(ctx, requiredEnv(t, "DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, path := range strings.Split(requiredEnv(t, "MIGRATION_PATH"), ",") {
		migration, readErr := os.ReadFile(filepath.Clean(strings.TrimSpace(path)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if execErr := store.Exec(ctx, string(migration)); execErr != nil {
			t.Fatal(execErr)
		}
	}
	cluster, err := tigerbeetle.HexStringToUint128(requiredEnv(t, "TIGERBEETLE_CLUSTER_ID_HEX"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := tigerbeetle.NewClient(cluster, strings.Split(requiredEnv(t, "TIGERBEETLE_REPLICA_ADDRESSES"), ","))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ledgerService, err := ledger.New(client, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	seed := fmt.Sprintf("%032x", time.Now().UnixNano())
	debit := seed
	credit := fmt.Sprintf("%032x", time.Now().UnixNano()+1)
	debitID, err := tigerbeetle.HexStringToUint128(debit)
	if err != nil {
		t.Fatal(err)
	}
	creditID, err := tigerbeetle.HexStringToUint128(credit)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgerService.CreateAccount(debitID, true); err != nil {
		t.Fatal(err)
	}
	if err := ledgerService.CreateAccount(creditID, true); err != nil {
		t.Fatal(err)
	}
	intentID := "live-tb-" + fmt.Sprintf("%d", time.Now().UnixNano())
	created, err := store.Create(ctx, intent.CreateRequest{IntentID: intentID, ExternalRef: intentID + "-external", DebitAccountID: debit, CreditAccountID: credit, Amount: 100, Ledger: 1, Code: 1, Currency: "NGN", Maker: "live-maker"})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.Approve(ctx, created.IntentID, created.Version, "live-checker")
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(store, ledgerService, 60)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := orchestrator.ReserveApproved(ctx, approved.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.State != intent.StateReserved {
		t.Fatalf("want RESERVED, got %s", reserved.State)
	}
	transfer, found, err := ledgerService.LookupTransfer(orchestrator.PendingTransferID(intentID))
	if err != nil || !found {
		t.Fatalf("pending transfer not found: found=%v err=%v", found, err)
	}
	if transfer.Amount != tigerbeetle.ToUint128(100) {
		t.Fatalf("unexpected pending amount: %v", transfer.Amount)
	}
	posted, err := orchestrator.PostReserved(ctx, intentID)
	if err != nil {
		t.Fatal(err)
	}
	if posted.State != intent.StatePosted {
		t.Fatalf("want POSTED, got %s", posted.State)
	}
	if _, found, err := ledgerService.LookupTransfer(orchestrator.PostTransferID(intentID)); err != nil || !found {
		t.Fatalf("post transfer not found: found=%v err=%v", found, err)
	}
	postedReplay, err := orchestrator.PostReserved(ctx, intentID)
	if err != nil {
		t.Fatalf("posted replay failed: %v", err)
	}
	if postedReplay.Version != posted.Version {
		t.Fatalf("posted replay changed version: got %d want %d", postedReplay.Version, posted.Version)
	}
}

func TestVoidReservedAgainstLiveTigerBeetle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := intent.Open(ctx, requiredEnv(t, "DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cluster, err := tigerbeetle.HexStringToUint128(requiredEnv(t, "TIGERBEETLE_CLUSTER_ID_HEX"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := tigerbeetle.NewClient(cluster, strings.Split(requiredEnv(t, "TIGERBEETLE_REPLICA_ADDRESSES"), ","))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ledgerService, err := ledger.New(client, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	seed := fmt.Sprintf("%032x", time.Now().UnixNano())
	debit := seed
	credit := fmt.Sprintf("%032x", time.Now().UnixNano()+1)
	debitID, err := tigerbeetle.HexStringToUint128(debit)
	if err != nil {
		t.Fatal(err)
	}
	creditID, err := tigerbeetle.HexStringToUint128(credit)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgerService.CreateAccount(debitID, true); err != nil {
		t.Fatal(err)
	}
	if err := ledgerService.CreateAccount(creditID, true); err != nil {
		t.Fatal(err)
	}
	intentID := "live-tb-void-" + fmt.Sprintf("%d", time.Now().UnixNano())
	created, err := store.Create(ctx, intent.CreateRequest{IntentID: intentID, ExternalRef: intentID + "-external", DebitAccountID: debit, CreditAccountID: credit, Amount: 100, Ledger: 1, Code: 1, Currency: "NGN", Maker: "live-maker-void"})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.Approve(ctx, created.IntentID, created.Version, "live-checker-void")
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := New(store, ledgerService, 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.ReserveApproved(ctx, approved.IntentID); err != nil {
		t.Fatal(err)
	}
	voided, err := orchestrator.VoidReserved(ctx, intentID)
	if err != nil {
		t.Fatal(err)
	}
	if voided.State != intent.StateVoided {
		t.Fatalf("want VOIDED, got %s", voided.State)
	}
	if _, found, err := ledgerService.LookupTransfer(orchestrator.VoidTransferID(intentID)); err != nil || !found {
		t.Fatalf("void transfer not found: found=%v err=%v", found, err)
	}
	voidedReplay, err := orchestrator.VoidReserved(ctx, intentID)
	if err != nil {
		t.Fatalf("void replay failed: %v", err)
	}
	if voidedReplay.Version != voided.Version {
		t.Fatalf("void replay changed version: got %d want %d", voidedReplay.Version, voided.Version)
	}
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func TestReconcileObservedAgainstLiveTigerBeetle(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		action func(context.Context, *Orchestrator, *ledger.Service, string) error
		want   intent.State
	}{
		{"pending", func(context.Context, *Orchestrator, *ledger.Service, string) error { return nil }, intent.StateReserved},
		{"post", func(ctx context.Context, o *Orchestrator, l *ledger.Service, id string) error {
			return l.Post(o.PostTransferID(id), o.PendingTransferID(id))
		}, intent.StatePosted},
		{"void", func(ctx context.Context, o *Orchestrator, l *ledger.Service, id string) error {
			return l.Void(o.VoidTransferID(id), o.PendingTransferID(id))
		}, intent.StateVoided},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			store, err := intent.Open(ctx, requiredEnv(t, "DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cluster, err := tigerbeetle.HexStringToUint128(requiredEnv(t, "TIGERBEETLE_CLUSTER_ID_HEX"))
			if err != nil {
				t.Fatal(err)
			}
			client, err := tigerbeetle.NewClient(cluster, strings.Split(requiredEnv(t, "TIGERBEETLE_REPLICA_ADDRESSES"), ","))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			service, err := ledger.New(client, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			seed := time.Now().UnixNano()
			debitText := fmt.Sprintf("%032x", seed)
			creditText := fmt.Sprintf("%032x", seed+1)
			debit, _ := tigerbeetle.HexStringToUint128(debitText)
			credit, _ := tigerbeetle.HexStringToUint128(creditText)
			if err := service.CreateAccount(debit, true); err != nil {
				t.Fatal(err)
			}
			if err := service.CreateAccount(credit, true); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("live-reconcile-%s-%d", scenario.name, seed)
			created, err := store.Create(ctx, intent.CreateRequest{IntentID: id, ExternalRef: id + "-external", DebitAccountID: debitText, CreditAccountID: creditText, Amount: 100, Ledger: 1, Code: 1, Currency: "NGN", Maker: "reconcile-maker"})
			if err != nil {
				t.Fatal(err)
			}
			approved, err := store.Approve(ctx, created.IntentID, created.Version, "reconcile-checker")
			if err != nil {
				t.Fatal(err)
			}
			o, err := New(store, service, 60)
			if err != nil {
				t.Fatal(err)
			}
			reserved, err := o.ReserveApproved(ctx, approved.IntentID)
			if err != nil {
				t.Fatal(err)
			}
			requiring, err := store.Transition(ctx, reserved.IntentID, reserved.Version, intent.StateReconciliationRequired)
			if err != nil {
				t.Fatal(err)
			}
			if err := scenario.action(ctx, o, service, id); err != nil {
				t.Fatal(err)
			}
			resolved, err := o.ReconcileObserved(ctx, requiring.IntentID)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.State != scenario.want {
				t.Fatalf("want %s got %s", scenario.want, resolved.State)
			}
		})
	}
}
