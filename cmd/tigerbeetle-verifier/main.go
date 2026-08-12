package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

type configuration struct {
	clusterID        tigerbeetle.Uint128
	replicaAddresses []string
	accountID        tigerbeetle.Uint128
}

type evidence struct {
	SchemaVersion          string    `json:"schema_version"`
	CheckedAt              time.Time `json:"checked_at"`
	AccountReferenceSHA256 string    `json:"account_reference_sha256"`
	Found                  bool      `json:"found"`
	Ledger                 uint32    `json:"ledger,omitempty"`
	Code                   uint16    `json:"code,omitempty"`
	Timestamp              uint64    `json:"timestamp,omitempty"`
	HistoryEnabled         bool      `json:"history_enabled,omitempty"`
	Closed                 bool      `json:"closed,omitempty"`
}

func main() {
	var accountIDHex string
	var evidencePath string
	flag.StringVar(&accountIDHex, "account-id-hex", "", "approved TigerBeetle account ID as hexadecimal Uint128")
	flag.StringVar(&evidencePath, "evidence", "", "approved non-secret evidence output path")
	flag.Parse()

	if accountIDHex == "" || evidencePath == "" {
		fail(errors.New("--account-id-hex and --evidence are required"))
	}
	config, err := loadConfiguration(accountIDHex)
	if err != nil {
		fail(err)
	}

	client, err := tigerbeetle.NewClient(config.clusterID, config.replicaAddresses)
	if err != nil {
		fail(fmt.Errorf("create TigerBeetle client: %w", err))
	}
	defer client.Close()

	accounts, err := client.LookupAccounts([]tigerbeetle.Uint128{config.accountID})
	if err != nil {
		fail(fmt.Errorf("look up approved account: %w", err))
	}
	if len(accounts) > 1 {
		fail(errors.New("unexpected multiple account records returned for one account identifier"))
	}

	report := evidence{
		SchemaVersion:          "blueeconomy.financial.tigerbeetle-verification.v1",
		CheckedAt:              time.Now().UTC(),
		AccountReferenceSHA256: digestText(config.accountID.String()),
		Found:                  len(accounts) == 1,
	}
	if len(accounts) == 1 {
		account := accounts[0]
		flags := account.AccountFlags()
		report.Ledger = account.Ledger
		report.Code = account.Code
		report.Timestamp = account.Timestamp
		report.HistoryEnabled = flags.History
		report.Closed = flags.Closed
	}
	if err := writeEvidence(filepath.Clean(evidencePath), report); err != nil {
		fail(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		fail(fmt.Errorf("encode report: %w", err))
	}
	fmt.Println(string(encoded))
}

func loadConfiguration(accountIDHex string) (configuration, error) {
	clusterIDHex := strings.TrimSpace(os.Getenv("TIGERBEETLE_CLUSTER_ID_HEX"))
	if clusterIDHex == "" {
		return configuration{}, errors.New("TIGERBEETLE_CLUSTER_ID_HEX must be injected by the approved environment configuration")
	}
	clusterID, err := tigerbeetle.HexStringToUint128(clusterIDHex)
	if err != nil {
		return configuration{}, fmt.Errorf("parse TIGERBEETLE_CLUSTER_ID_HEX: %w", err)
	}
	accountID, err := tigerbeetle.HexStringToUint128(strings.TrimSpace(accountIDHex))
	if err != nil {
		return configuration{}, fmt.Errorf("parse --account-id-hex: %w", err)
	}
	replicas := strings.Split(strings.TrimSpace(os.Getenv("TIGERBEETLE_REPLICA_ADDRESSES")), ",")
	if len(replicas) == 0 || replicas[0] == "" {
		return configuration{}, errors.New("TIGERBEETLE_REPLICA_ADDRESSES must contain approved replica address values")
	}
	for index, address := range replicas {
		replicas[index] = strings.TrimSpace(address)
		if replicas[index] == "" {
			return configuration{}, errors.New("TIGERBEETLE_REPLICA_ADDRESSES contains an empty address")
		}
	}
	return configuration{clusterID: clusterID, replicaAddresses: replicas, accountID: accountID}, nil
}

func writeEvidence(path string, report evidence) error {
	if path == "." || path == string(filepath.Separator) {
		return errors.New("evidence output must name a file")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create evidence directory: %w", err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode evidence: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o640); err != nil {
		return fmt.Errorf("write temporary evidence: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish evidence: %w", err)
	}
	return nil
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tigerbeetle-verifier:", err)
	os.Exit(1)
}
