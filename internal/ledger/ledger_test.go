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
	if client.lookups != nil {
		for index, transfer := range transfers {
			// Mirror the cluster: only transfers this call actually created
			// become visible to later lookups; pre-existing ones (Exists) are
			// seeded by the test itself.
			if index < len(client.transferResults) && client.transferResults[index].Status != tigerbeetle.TransferCreated {
				continue
			}
			if _, ok := client.lookups[transfer.ID]; !ok {
				client.lookups[transfer.ID] = transfer
			}
		}
	}
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
	// 100.00 USD at 1,550.25 NGN/USD = 15,502,500 kobo.
	equivalent, err := confirmed.ConvertUSDToNGN(10_000)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return DisbursementPairInput{
		ApplicationID:      "cvff-001",
		FeeNGNMinor:        1_000_000,
		CostUSDMinor:       10_000,
		CostNGNEquivalent:  equivalent,
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
	if !legs.RateEffectiveDate.Equal(input.Rate.EffectiveDate) {
		t.Fatalf("rate effective date = %s", legs.RateEffectiveDate)
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
	input = disbursementInput(t)
	input.CostNGNEquivalent = 0
	if _, _, err := BuildDisbursementPair(input); err == nil {
		t.Fatal("zero NGN equivalent accepted")
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

	errClient := &fakeClient{transferErr: errors.New("cluster unavailable")}
	if _, err := newService(t, errClient).CreateDisbursementPair(disbursementInput(t)); err == nil {
		t.Fatal("cluster error swallowed")
	}
}

// TestCreateDisbursementPairExistsIsIdempotentSuccess covers the Temporal
// activity retry after a TigerBeetle commit whose leg recording failed: the
// deterministic transfer IDs come back as TransferExists and, because the
// stored content matches the deterministic retry, the pair is a success.
func TestCreateDisbursementPairExistsIsIdempotentSuccess(t *testing.T) {
	input := disbursementInput(t)
	transfers, _, err := BuildDisbursementPair(input)
	if err != nil {
		t.Fatalf("build pair: %v", err)
	}
	client := &fakeClient{
		transferResults: []tigerbeetle.CreateTransferResult{
			{Status: tigerbeetle.TransferExists},
			{Status: tigerbeetle.TransferCreated},
		},
		lookups: map[tigerbeetle.Uint128]tigerbeetle.Transfer{transfers[0].ID: transfers[0]},
	}
	legs, err := newService(t, client).CreateDisbursementPair(input)
	if err != nil {
		t.Fatalf("idempotent replay rejected: %v", err)
	}
	if legs.FeeTransferID != transfers[0].ID || legs.CostTransferID != transfers[1].ID {
		t.Fatalf("legs after replay: %+v", legs)
	}
}

// TestCreateDisbursementPairExistsMismatchIsConflict: TransferExists with
// divergent stored content is a real conflict, never a silent success.
func TestCreateDisbursementPairExistsMismatchIsConflict(t *testing.T) {
	input := disbursementInput(t)
	transfers, _, err := BuildDisbursementPair(input)
	if err != nil {
		t.Fatalf("build pair: %v", err)
	}
	divergent := transfers[1]
	divergent.Amount = tigerbeetle.ToUint128(9_999_999)
	client := &fakeClient{
		transferResults: []tigerbeetle.CreateTransferResult{
			{Status: tigerbeetle.TransferCreated},
			{Status: tigerbeetle.TransferExists},
		},
		lookups: map[tigerbeetle.Uint128]tigerbeetle.Transfer{transfers[1].ID: divergent},
	}
	if _, err := newService(t, client).CreateDisbursementPair(input); !errors.Is(err, ErrTransferConflict) {
		t.Fatalf("divergent replay error = %v", err)
	}

	// An Exists report whose transfer lookup finds nothing is contradictory.
	ghost := &fakeClient{
		transferResults: []tigerbeetle.CreateTransferResult{
			{Status: tigerbeetle.TransferCreated},
			{Status: tigerbeetle.TransferExists},
		},
		lookups: map[tigerbeetle.Uint128]tigerbeetle.Transfer{},
	}
	if _, err := newService(t, ghost).CreateDisbursementPair(input); !errors.Is(err, ErrTransferConflict) {
		t.Fatalf("ghost transfer error = %v", err)
	}

	// Any other non-created status stays fatal.
	fatal := &fakeClient{transferResults: []tigerbeetle.CreateTransferResult{
		{Status: tigerbeetle.TransferCreated},
		{Status: tigerbeetle.TransferExceedsCredits},
	}}
	if _, err := newService(t, fatal).CreateDisbursementPair(input); err == nil || errors.Is(err, ErrTransferConflict) {
		t.Fatalf("non-created status error = %v", err)
	}
}

// TestTransferRetryExists covers the two-phase rail: a retried reserve, post
// or void whose deterministic transfer already committed with identical
// content is idempotent success; divergence is a conflict.
func TestTransferRetryExists(t *testing.T) {
	reserve := tigerbeetle.Transfer{
		ID:              id(1),
		DebitAccountID:  id(2),
		CreditAccountID: id(3),
		Amount:          tigerbeetle.ToUint128(100),
		Timeout:         60,
		Ledger:          1,
		Code:            1,
		Flags:           tigerbeetle.TransferFlags{Pending: true}.ToUint16(),
	}
	retry := &fakeClient{
		transferResults: []tigerbeetle.CreateTransferResult{{Status: tigerbeetle.TransferExists}},
		lookups:         map[tigerbeetle.Uint128]tigerbeetle.Transfer{id(1): reserve},
	}
	service := newService(t, retry)
	if err := service.Reserve(id(1), id(2), id(3), 100, 60); err != nil {
		t.Fatalf("reserve retry rejected: %v", err)
	}
	posted := tigerbeetle.Transfer{
		ID:        id(9),
		PendingID: id(1),
		Ledger:    1,
		Code:      1,
		Flags:     tigerbeetle.TransferFlags{PostPendingTransfer: true}.ToUint16(),
	}
	retryPost := &fakeClient{
		transferResults: []tigerbeetle.CreateTransferResult{{Status: tigerbeetle.TransferExists}},
		lookups:         map[tigerbeetle.Uint128]tigerbeetle.Transfer{id(9): posted},
	}
	if err := newService(t, retryPost).Post(id(9), id(1)); err != nil {
		t.Fatalf("post retry rejected: %v", err)
	}
	divergent := reserve
	divergent.Amount = tigerbeetle.ToUint128(50)
	conflict := &fakeClient{
		transferResults: []tigerbeetle.CreateTransferResult{{Status: tigerbeetle.TransferExists}},
		lookups:         map[tigerbeetle.Uint128]tigerbeetle.Transfer{id(1): divergent},
	}
	if err := newService(t, conflict).Reserve(id(1), id(2), id(3), 100, 60); !errors.Is(err, ErrTransferConflict) {
		t.Fatalf("divergent reserve retry error = %v", err)
	}
}
