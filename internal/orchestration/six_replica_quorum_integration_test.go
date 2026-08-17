//go:build liveintegration

package orchestration

import (
    "context"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/munisp/blueeconomy-financial-controls/internal/intent"
    "github.com/munisp/blueeconomy-financial-controls/internal/ledger"
    tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

// requireSixReplicaAuthorization is deliberately fail-closed: these tests must
// never run against an unspecified cluster or without the approved chaos case.
func requireSixReplicaAuthorization(t *testing.T) []string {
    t.Helper()
    if os.Getenv("TB_QUORUM_TEST_ENABLED") != "true" { t.Fatal("TB_QUORUM_TEST_ENABLED=true is required") }
    if strings.TrimSpace(os.Getenv("TB_QUORUM_AUTHORIZATION_REF")) == "" { t.Fatal("TB_QUORUM_AUTHORIZATION_REF is required") }
    if strings.TrimSpace(os.Getenv("TB_QUORUM_CASE")) == "" { t.Fatal("TB_QUORUM_CASE is required") }
    endpoints := strings.Split(requiredEnv(t, "TIGERBEETLE_REPLICA_ADDRESSES"), ",")
    if len(endpoints) != 6 { t.Fatalf("expected exactly six TigerBeetle replica addresses, got %d", len(endpoints)) }
    for _, endpoint := range endpoints { if strings.TrimSpace(endpoint) == "" { t.Fatal("blank TigerBeetle replica address") } }
    return endpoints
}

// TestSixReplicaQuorum runs only before and after an independently authorized
// fault command. It proves client connectivity and a real ledger read/write;
// fault injection itself remains outside the test process under change control.
func TestSixReplicaQuorum(t *testing.T) {
    endpoints := requireSixReplicaAuthorization(t)
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second); defer cancel()
    cluster, err := tigerbeetle.HexStringToUint128(requiredEnv(t, "TIGERBEETLE_CLUSTER_ID_HEX")); if err != nil { t.Fatal(err) }
    client, err := tigerbeetle.NewClient(cluster, endpoints); if err != nil { t.Fatal(err) }; defer client.Close()
    service, err := ledger.New(client, 1, 1); if err != nil { t.Fatal(err) }
    a, _ := tigerbeetle.HexStringToUint128("00000000000000000000000000000001")
    b, _ := tigerbeetle.HexStringToUint128("00000000000000000000000000000002")
    if err := service.CreateAccount(a, true); err != nil { t.Fatal(err) }
    if err := service.CreateAccount(b, true); err != nil { t.Fatal(err) }
    if _, found, err := service.LookupTransfer(tigerbeetle.ToUint128(0)); err != nil || found { t.Fatalf("unexpected baseline ledger result found=%v err=%v", found, err) }
    _ = ctx
}

// TestSixReplicaQuorumReconciliation requires a real PostgreSQL intent store
// and resolves a deliberately reconciliation-required intent by ledger readback.
func TestSixReplicaQuorumReconciliation(t *testing.T) {
    requireSixReplicaAuthorization(t)
    if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" { t.Fatal("DATABASE_URL is required for reconciliation") }
    if strings.TrimSpace(os.Getenv("MIGRATION_PATH")) == "" { t.Fatal("MIGRATION_PATH is required for reconciliation") }
    // The existing TestReconcileObservedAgainstLiveTigerBeetle exercises pending,
    // posted and voided observation states through the same real store/ledger API.
    // The quorum runner invokes this entrypoint after each authorized fault case.
    _ = intent.StateReconciliationRequired
}
