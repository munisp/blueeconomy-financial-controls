//go:build integration

package settlementsync

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

// TestRealTigerBeetleMirror mirrors a posted collection transfer from a REAL
// TigerBeetle cluster into settlement_records. The cluster address is
// opt-in: TIGERBEETLE_TEST_ADDRESS (plus TIGERBEETLE_TEST_CLUSTER_ID_HEX,
// default all-zero cluster id 0) — when unset the test SKIPS honestly
// rather than fabricating a ledger. DATABASE_URL + MIGRATION_PATH are still
// required (the mirror lands in PostgreSQL).
func TestRealTigerBeetleMirror(t *testing.T) {
	address := strings.TrimSpace(os.Getenv("TIGERBEETLE_TEST_ADDRESS"))
	if address == "" {
		t.Skip("TIGERBEETLE_TEST_ADDRESS unset — skipping the real-cluster mirror test honestly (no TigerBeetle is fabricated)")
	}
	store, pool := openRevenueStore(t)
	ctx := context.Background()

	clusterID := tigerbeetle.ToUint128(0)
	if hex := strings.TrimSpace(os.Getenv("TIGERBEETLE_TEST_CLUSTER_ID_HEX")); hex != "" {
		parsed, err := tigerbeetle.HexStringToUint128(hex)
		if err != nil {
			t.Fatalf("parse TIGERBEETLE_TEST_CLUSTER_ID_HEX: %v", err)
		}
		clusterID = parsed
	}
	client, err := tigerbeetle.NewClient(clusterID, []string{address})
	if err != nil {
		t.Fatalf("create TigerBeetle client: %v", err)
	}
	defer client.Close()

	// Seed one payer/payee pair and one POSTED collection transfer on the
	// configured ledger/code (ledger 1 = USD, code 7 per testConfig).
	payer, payee := tigerbeetle.ID(), tigerbeetle.ID()
	accounts := []tigerbeetle.Account{
		{ID: payer, Ledger: 1, Code: 7},
		{ID: payee, Ledger: 1, Code: 7},
	}
	if results, err := client.CreateAccounts(accounts); err != nil {
		t.Fatalf("create accounts: %v", err)
	} else {
		for _, result := range results {
			if result.Status != tigerbeetle.AccountCreated && result.Status != tigerbeetle.AccountExists {
				t.Fatalf("account status %v", result.Status)
			}
		}
	}
	transferID := tigerbeetle.ID()
	if results, err := client.CreateTransfers([]tigerbeetle.Transfer{{
		ID:              transferID,
		DebitAccountID:  payer,
		CreditAccountID: payee,
		Amount:          tigerbeetle.ToUint128(125000),
		Ledger:          1,
		Code:            7,
	}}); err != nil {
		t.Fatalf("create transfer: %v", err)
	} else if len(results) != 0 {
		t.Fatalf("transfer results: %+v", results)
	}

	syncer, err := NewSyncer(client, store, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Poll until the cluster timestamp lands (bounded).
	var mirrored int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mirrored, err = syncer.SyncOnce(ctx)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		if mirrored > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if mirrored != 1 {
		t.Fatalf("mirrored %d transfers, want 1", mirrored)
	}
	// Replay against the live cluster is a no-op.
	mirrored, err = syncer.SyncOnce(ctx)
	if err != nil || mirrored != 0 {
		t.Fatalf("replay mirrored %d, want 0 (err %v)", mirrored, err)
	}
	var count int
	var currency string
	var amount int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*), max(currency), max(amount_minor) FROM settlement_records
		 WHERE tb_transfer_id = $1`, transferID.String()).Scan(&count, &currency, &amount); err != nil {
		t.Fatal(err)
	}
	if count != 1 || currency != "USD" || amount != 125000 {
		t.Fatalf("mirrored row: count=%d %s %d", count, currency, amount)
	}
}
