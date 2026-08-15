package main

import (
	"strings"
	"testing"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

func TestValidateProductionQuorum(t *testing.T) {
	clusterID, err := tigerbeetle.HexStringToUint128("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	replicas := []string{
		"10.10.0.10:3001", "10.10.0.11:3002", "10.10.1.10:3003",
		"10.10.1.11:3004", "10.10.2.10:3005", "10.10.2.11:3006",
	}
	if err := validateProductionQuorum(clusterID, replicas); err != nil {
		t.Fatalf("valid quorum configuration rejected: %v", err)
	}
	if err := validateProductionQuorum(tigerbeetle.Uint128{}, replicas); err == nil {
		t.Fatal("zero test cluster was accepted")
	}
	if err := validateProductionQuorum(clusterID, replicas[:5]); err == nil {
		t.Fatal("five-replica configuration was accepted")
	}
	duplicate := append([]string{}, replicas...)
	duplicate[5] = duplicate[0]
	if err := validateProductionQuorum(clusterID, duplicate); err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("duplicate configuration returned %v", err)
	}
	malformed := append([]string{}, replicas...)
	malformed[2] = "not-a-host-port"
	if err := validateProductionQuorum(clusterID, malformed); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("malformed configuration returned %v", err)
	}
}
