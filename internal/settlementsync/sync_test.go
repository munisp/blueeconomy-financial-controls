package settlementsync

import (
	"context"
	"errors"
	"strings"
	"testing"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"

	"github.com/munisp/blueeconomy-financial-controls/internal/revenue"
)

type fakeQuerier struct {
	transfers []tigerbeetle.Transfer
	err       error
	last      tigerbeetle.QueryFilter
}

func (querier *fakeQuerier) QueryTransfers(filter tigerbeetle.QueryFilter) ([]tigerbeetle.Transfer, error) {
	querier.last = filter
	if querier.err != nil {
		return nil, querier.err
	}
	return querier.transfers, nil
}

type fakeMirrorStore struct {
	cursor    uint64
	recorded  []revenue.TBMirrorInput
	advanced  []uint64
	createSeq map[string]bool
}

func newFakeMirrorStore() *fakeMirrorStore {
	return &fakeMirrorStore{createSeq: map[string]bool{}}
}

func (store *fakeMirrorStore) SyncCursor(context.Context, string) (uint64, error) {
	return store.cursor, nil
}

func (store *fakeMirrorStore) AdvanceSyncCursor(_ context.Context, _ string, timestamp uint64) error {
	store.advanced = append(store.advanced, timestamp)
	if timestamp > store.cursor {
		store.cursor = timestamp
	}
	return nil
}

func (store *fakeMirrorStore) RecordTBSettlement(_ context.Context, input revenue.TBMirrorInput, _ string, observed uint64) (bool, error) {
	store.recorded = append(store.recorded, input)
	if observed > store.cursor {
		store.cursor = observed
	}
	if store.createSeq[input.TBTransferID] {
		return false, nil // replay no-op, mirrors the UNIQUE constraint
	}
	store.createSeq[input.TBTransferID] = true
	return true, nil
}

func transfer(id, amount uint64, timestamp uint64, pending, void bool) tigerbeetle.Transfer {
	var flags tigerbeetle.TransferFlags
	flags.Pending = pending
	flags.VoidPendingTransfer = void
	return tigerbeetle.Transfer{
		ID:        tigerbeetle.ToUint128(id),
		Amount:    tigerbeetle.ToUint128(amount),
		Ledger:    1,
		Code:      7,
		Timestamp: timestamp,
		Flags:     flags.ToUint16(),
	}
}

func testConfig() Config {
	return Config{Ledgers: map[uint32]string{1: "USD"}, Code: 7, Limit: 512}
}

func TestSyncMirrorsPostedTransfersOnly(t *testing.T) {
	querier := &fakeQuerier{transfers: []tigerbeetle.Transfer{
		transfer(3, 900, 300, false, false),
		transfer(1, 100, 100, false, false),
		transfer(2, 200, 200, true, false), // pending reservation: not money
		transfer(4, 400, 400, false, true), // void: not money
	}}
	store := newFakeMirrorStore()
	syncer, err := NewSyncer(querier, store, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	mirrored, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mirrored != 2 {
		t.Fatalf("mirrored %d, want 2 (posted only)", mirrored)
	}
	if len(store.recorded) != 2 {
		t.Fatalf("recorded %d rows", len(store.recorded))
	}
	// Ascending timestamp order regardless of query return order.
	if store.recorded[0].TBTransferID != "1" || store.recorded[1].TBTransferID != "3" {
		t.Fatalf("order: %+v", store.recorded)
	}
	first := store.recorded[0]
	if first.AmountMinor != 100 || first.Currency != "USD" ||
		first.BankReference != "tb:1" || first.PayerRef == "" || first.ValueDate == "" {
		t.Fatalf("mirror: %+v", first)
	}
	// Cursor advanced past the void too.
	if store.cursor != 400 {
		t.Fatalf("cursor %d, want 400", store.cursor)
	}
	// The poll resumed after the previous cursor with the configured
	// ledger/code — no unbounded scans.
	if querier.last.TimestampMin != 1 || querier.last.Ledger != 1 || querier.last.Code != 7 || querier.last.Limit != 512 {
		t.Fatalf("filter: %+v", querier.last)
	}

	// Replay: the same transfers mirror nothing new (idempotent).
	mirrored, err = syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mirrored != 0 {
		t.Fatalf("replay mirrored %d, want 0", mirrored)
	}
}

func TestSyncRefusesOversizedAmount(t *testing.T) {
	oversized := transfer(9, 100, 100, false, false)
	rawAmount := oversized.Amount.Bytes()
	rawAmount[15] = 1 // high limb set: exceeds int64 minor units
	oversized.Amount = tigerbeetle.Uint128(rawAmount)
	querier := &fakeQuerier{transfers: []tigerbeetle.Transfer{oversized}}
	syncer, err := NewSyncer(querier, newFakeMirrorStore(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, err = syncer.SyncOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds int64") {
		t.Fatalf("err = %v, want oversized-amount refusal", err)
	}
}

func TestSyncPropagatesQueryFailure(t *testing.T) {
	querier := &fakeQuerier{err: errors.New("cluster unreachable")}
	syncer, err := NewSyncer(querier, newFakeMirrorStore(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.SyncOnce(context.Background()); err == nil {
		t.Fatal("query failure swallowed")
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := NewSyncer(nil, newFakeMirrorStore(), testConfig()); err == nil {
		t.Fatal("nil querier accepted")
	}
	if _, err := NewSyncer(&fakeQuerier{}, nil, testConfig()); err == nil {
		t.Fatal("nil store accepted")
	}
	for name, mutate := range map[string]func(*Config){
		"no ledgers":    func(c *Config) { c.Ledgers = nil },
		"bad currency":  func(c *Config) { c.Ledgers = map[uint32]string{1: "EUR"} },
		"zero code":     func(c *Config) { c.Code = 0 },
		"zero ledger":   func(c *Config) { c.Ledgers = map[uint32]string{0: "USD"} },
		"limit too big": func(c *Config) { c.Limit = 9000 },
	} {
		config := testConfig()
		mutate(&config)
		if _, err := NewSyncer(&fakeQuerier{}, newFakeMirrorStore(), config); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestParseLedgerCurrencies(t *testing.T) {
	if _, err := ParseLedgerCurrencies(""); err == nil {
		t.Fatal("empty mapping accepted (fail-closed violation)")
	}
	if _, err := ParseLedgerCurrencies("1=EUR"); err == nil {
		t.Fatal("unsupported currency accepted")
	}
	if _, err := ParseLedgerCurrencies("abc"); err == nil {
		t.Fatal("malformed entry accepted")
	}
	ledgers, err := ParseLedgerCurrencies("1=USD, 2=NGN")
	if err != nil {
		t.Fatal(err)
	}
	if ledgers[1] != "USD" || ledgers[2] != "NGN" {
		t.Fatalf("ledgers: %+v", ledgers)
	}
}
