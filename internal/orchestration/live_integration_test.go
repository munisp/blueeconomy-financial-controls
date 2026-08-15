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
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}
