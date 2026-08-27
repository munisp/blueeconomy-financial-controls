package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/fx"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

// fakeClient records calls and serves programmed results.
type fakeClient struct {
	transferResults []tigerbeetle.CreateTransferResult
	transferErr     error
	created         []tigerbeetle.Transfer
	lookups         map[tigerbeetle.Uint128]tigerbeetle.Transfer
	lookupErr       error
}

func (client *fakeClient) CreateAccounts(accounts []tigerbeetle.Account) ([]tigerbeetle.CreateAccountResult, error) {
	return nil, errors.New("not implemented")
}

func (client *fakeClient) CreateTransfers(transfers []tigerbeetle.Transfer) ([]tigerbeetle.CreateTransferResult, error) {
	if client.transferErr != nil {
		return nil, client.transferErr
	}
	client.created = append(client.created, transfers...)
	return client.transferResults, nil
}

func (client *fakeClient) LookupTransfers(ids []tigerbeetle.Uint128) ([]tigerbeetle.Transfer, error) {
	if client.lookupErr != nil {
		return nil, client.lookupErr
	}
	found := make([]tigerbeetle.Transfer, 0, len(ids))
	for _, id := range ids {
		if transfer, ok := client.lookups[id]; ok {
			found = append(found, transfer)
		}
	}
	return found, nil
}

func newService(t *testing.T, client *fakeClient) *Service {
	t.Helper()
	service, err := New(client, 1, 1)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func id(value byte) tigerbeetle.Uint128 {
	return tigerbeetle.ToUint128(uint64(value))
}

func okResults(count int) []tigerbeetle.CreateTransferResult {
	results := make([]tigerbeetle.CreateTransferResult, count)
	for index := range results {
		results[index] = tigerbeetle.CreateTransferResult{Status: tigerbeetle.TransferCreated}
	}
	return results
}

func TestNewFailsClosed(t *testing.T) {
	if _, err := New(nil, 1, 1); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := New(&fakeClient{}, 0, 1); err == nil {
		t.Fatal("zero ledger accepted")
	}
	if _, err := New(&fakeClient{}, 1, 0); err == nil {
		t.Fatal("zero code accepted")
	}
}

func TestReserveValidatesAndPosts(t *testing.T) {
	client := &fakeClient{transferResults: okResults(1)}
	service := newService(t, client)
	if err := service.Reserve(tigerbeetle.Uint128{}, id(2), id(3), 100, 60); err == nil {
		t.Fatal("zero transfer ID accepted")
	}
	if err := service.Reserve(id(1), id(2), id(2), 100, 60); err == nil {
		t.Fatal("same debit/credit accepted")
	}
	if err := service.Reserve(id(1), id(2), id(3), 0, 60); err == nil {
		t.Fatal("zero amount accepted")
	}
	if err := service.Reserve(id(1), id(2), id(3), 100, 0); err == nil {
		t.Fatal("zero timeout accepted")
	}
	if err := service.Reserve(id(1), id(2), id(3), 100, 60); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if len(client.created) != 1 || client.created[0].Flags&(tigerbeetle.TransferFlags{Pending: true}.ToUint16()) == 0 {
		t.Fatalf("created transfers: %+v", client.created)
	}
}

func TestPostAndVoid(t *testing.T) {
	client := &fakeClient{transferResults: okResults(1)}
	service := newService(t, client)
	if err := service.Post(id(2), id(1)); err != nil {
		t.Fatalf("post: %v", err)
	}
	if client.created[0].Flags&(tigerbeetle.TransferFlags{PostPendingTransfer: true}.ToUint16()) == 0 {
		t.Fatalf("post flags: %+v", client.created[0])
	}
	if err := service.Void(id(3), id(1)); err != nil {
		t.Fatalf("void: %v", err)
	}
	if client.created[1].Flags&(tigerbeetle.TransferFlags{VoidPendingTransfer: true}.ToUint16()) == 0 {
		t.Fatalf("void flags: %+v", client.created[1])
	}
	if err := service.Post(tigerbeetle.Uint128{}, id(1)); err == nil {
		t.Fatal("zero post ID accepted")
	}
	if err := service.Void(tigerbeetle.Uint128{}, id(1)); err == nil {
		t.Fatal("zero void ID accepted")
	}
}

func TestLookupTransfer(t *testing.T) {
	client := &fakeClient{lookups: map[tigerbeetle.Uint128]tigerbeetle.Transfer{id(7): {ID: id(7)}}}
	service := newService(t, client)
	transfer, found, err := service.LookupTransfer(id(7))
	if err != nil || !found || transfer.ID != id(7) {
		t.Fatalf("lookup = %v, %v, %v", transfer, found, err)
	}
	if _, found, err := service.LookupTransfer(id(8)); err != nil || found {
		t.Fatalf("missing lookup = %v, %v", found, err)
	}
	if _, _, err := service.LookupTransfer(tigerbeetle.Uint128{}); err == nil {
		t.Fatal("zero lookup ID accepted")
	}
}

func disbursementInput(t *testing.T) DisbursementPairInput {
	t.Helper()
	rate, err := fx.NewRate("rate-1", 1_550_250_000, time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC), "kc-maker")
	if err != nil {
		t.Fatalf("new rate: %v", err)
	}
	confirmed, err := rate.Confirm("kc-checker")
	if err != nil {
		t.Fatalf("confirm rate: %v", err)
	}
	return DisbursementPairInput{
		ApplicationID:      "cvff-001",
		FeeNGNMinor:        1_000_000,
		CostUSDMinor:       10_000,
		Rate:               confirmed,
		NGNDebitAccountID:  id(11),
		NGNCreditAccountID: id(12),
		USDDebitAccountID:  id(21),
		USDCreditAccountID: id(22),
		NGNLedger:          1,
		USDLedger:          2,
		FeeCode:            10,
		CostCode:           20,
	}
}

func TestBuildDisbursementPair(t *testing.T) {
	input := disbursementInput(t)
	transfers, legs, err := BuildDisbursementPair(input)
	if err != nil {
		t.Fatalf("build pair: %v", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("transfers = %d, want 2", len(transfers))
	}
	fee, cost := transfers[0], transfers[1]
	if fee.Amount != tigerbeetle.ToUint128(1_000_000) || fee.Ledger != 1 || fee.Code != 10 {
		t.Fatalf("fee leg: %+v", fee)
	}
	if cost.Amount != tigerbeetle.ToUint128(10_000) || cost.Ledger != 2 || cost.Code != 20 {
		t.Fatalf("cost leg: %+v", cost)
	}
	// 100.00 USD at 1,550.25 NGN/USD = 15,502,500 kobo.
	if legs.CostNGNEquivalent != 15_502_500 {
		t.Fatalf("ngn equivalent = %d", legs.CostNGNEquivalent)
	}
	// Deterministic idempotent IDs.
	again, againLegs, err := BuildDisbursementPair(input)
	if err != nil {
		t.Fatalf("rebuild pair: %v", err)
	}
	if again[0].ID != transfers[0].ID || again[1].ID != transfers[1].ID {
		t.Fatal("deterministic transfer IDs differ")
	}
	if againLegs.FeeTransferID != legs.FeeTransferID || againLegs.CostTransferID != legs.CostTransferID {
		t.Fatal("leg transfer IDs differ")
	}
	if transfers[0].ID == transfers[1].ID {
		t.Fatal("fee and cost transfer IDs collide")
	}
}

func TestBuildDisbursementPairFailsClosed(t *testing.T) {
	input := disbursementInput(t)
	input.Rate.Confirmed = false
	if _, _, err := BuildDisbursementPair(input); err == nil {
		t.Fatal("unconfirmed rate accepted")
	}
	input = disbursementInput(t)
	input.FeeNGNMinor = 0
	if _, _, err := BuildDisbursementPair(input); err == nil {
		t.Fatal("zero fee accepted")
	}
	input = disbursementInput(t)
	input.USDLedger = 0
	if _, _, err := BuildDisbursementPair(input); err == nil {
		t.Fatal("zero USD ledger accepted")
	}
	input = disbursementInput(t)
	input.ApplicationID = ""
	if _, _, err := BuildDisbursementPair(input); err == nil {
		t.Fatal("empty application ID accepted")
	}
	input = disbursementInput(t)
	input.NGNCreditAccountID = input.NGNDebitAccountID
	if _, _, err := BuildDisbursementPair(input); err == nil {
		t.Fatal("same NGN debit/credit accepted")
	}
}

func TestCreateDisbursementPair(t *testing.T) {
	client := &fakeClient{transferResults: okResults(2)}
	service := newService(t, client)
	legs, err := service.CreateDisbursementPair(disbursementInput(t))
	if err != nil {
		t.Fatalf("create pair: %v", err)
	}
	if len(client.created) != 2 {
		t.Fatalf("created = %d transfers", len(client.created))
	}
	if legs.ApplicationID != "cvff-001" || legs.RateID != "rate-1" {
		t.Fatalf("legs: %+v", legs)
	}

	failing := &fakeClient{transferResults: []tigerbeetle.CreateTransferResult{
		{Status: tigerbeetle.TransferCreated},
		{Status: tigerbeetle.TransferExists},
	}}
	if _, err := newService(t, failing).CreateDisbursementPair(disbursementInput(t)); err == nil {
		t.Fatal("non-created status accepted")
	}

	errClient := &fakeClient{transferErr: errors.New("cluster unavailable")}
	if _, err := newService(t, errClient).CreateDisbursementPair(disbursementInput(t)); err == nil {
		t.Fatal("cluster error swallowed")
	}
}
