package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/fx"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
)

type stubRateSource struct {
	rate fx.Rate
	err  error
}

func (source stubRateSource) ConfirmedForDate(context.Context, time.Time) (fx.Rate, error) {
	return source.rate, source.err
}

type stubPairCreator struct {
	legs ledger.DisbursementLegs
	err  error
	call int
}

func (creator *stubPairCreator) CreateDisbursementPair(ledger.DisbursementPairInput) (ledger.DisbursementLegs, error) {
	creator.call++
	return creator.legs, creator.err
}

// stubLegsStore extends stubStore with disbursement leg persistence.
type stubLegsStore struct {
	*stubStore
	legs map[string]cvff.DisbursementLegs
}

func (store *stubLegsStore) DisbursementLegs(_ context.Context, applicationID string) (cvff.DisbursementLegs, error) {
	legs, ok := store.legs[applicationID]
	if !ok {
		return cvff.DisbursementLegs{}, cvff.ErrNotFound
	}
	return legs, nil
}

func (store *stubLegsStore) RecordDisbursementLegs(_ context.Context, legs cvff.DisbursementLegs) error {
	if legs.ApplicationID == "" || legs.FeeNGNMinor == 0 || legs.CostUSDMinor == 0 || legs.CostNGNEquivalent == 0 {
		return errors.New("invalid legs")
	}
	if _, exists := store.legs[legs.ApplicationID]; !exists {
		store.legs[legs.ApplicationID] = legs
	}
	return nil
}

func railFixture(t *testing.T) (*DisbursementRail, *stubLegsStore, *stubPairCreator) {
	t.Helper()
	store := &stubLegsStore{stubStore: newStubStore(), legs: map[string]cvff.DisbursementLegs{}}
	store.application.State = cvff.StateDisbursementPending
	rate, err := fx.NewRate("rate-1", 1_550_250_000, time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC), "kc-maker")
	if err != nil {
		t.Fatalf("new rate: %v", err)
	}
	confirmed, err := rate.Confirm("kc-checker")
	if err != nil {
		t.Fatalf("confirm rate: %v", err)
	}
	creator := &stubPairCreator{legs: ledger.DisbursementLegs{
		ApplicationID:     "cvff-001",
		RateID:            "rate-1",
		NGNPerUSDMicro:    1_550_250_000,
		RateEffectiveDate: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
		FeeTransferID:     tigerbeetle.ToUint128(101),
		CostTransferID:    tigerbeetle.ToUint128(102),
		FeeNGNMinor:       2_712_937_500,
		CostUSDMinor:      700_000_000,
		CostNGNEquivalent: 1_085_175_000_000,
	}}
	config := DisbursementRailConfig{
		NGNDebitAccountID:  tigerbeetle.ToUint128(11),
		NGNCreditAccountID: tigerbeetle.ToUint128(12),
		USDDebitAccountID:  tigerbeetle.ToUint128(21),
		USDCreditAccountID: tigerbeetle.ToUint128(22),
		NGNLedger:          1,
		USDLedger:          2,
		FeeCode:            10,
		CostCode:           20,
		FeeBasisPoints:     25,
	}
	rail, err := NewDisbursementRail(store, stubRateSource{rate: confirmed}, creator, config)
	if err != nil {
		t.Fatalf("new rail: %v", err)
	}
	return rail, store, creator
}

func TestRailConfigFailsClosed(t *testing.T) {
	store := &stubLegsStore{stubStore: newStubStore(), legs: map[string]cvff.DisbursementLegs{}}
	valid := DisbursementRailConfig{
		NGNDebitAccountID:  tigerbeetle.ToUint128(11),
		NGNCreditAccountID: tigerbeetle.ToUint128(12),
		USDDebitAccountID:  tigerbeetle.ToUint128(21),
		USDCreditAccountID: tigerbeetle.ToUint128(22),
		NGNLedger:          1,
		USDLedger:          2,
		FeeCode:            10,
		CostCode:           20,
		FeeBasisPoints:     25,
	}
	if _, err := NewDisbursementRail(nil, stubRateSource{}, &stubPairCreator{}, valid); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewDisbursementRail(store, nil, &stubPairCreator{}, valid); err == nil {
		t.Fatal("nil rate source accepted")
	}
	if _, err := NewDisbursementRail(store, stubRateSource{}, nil, valid); err == nil {
		t.Fatal("nil pair creator accepted")
	}
	zeroAccount := valid
	zeroAccount.USDCreditAccountID = tigerbeetle.Uint128{}
	if _, err := NewDisbursementRail(store, stubRateSource{}, &stubPairCreator{}, zeroAccount); err == nil {
		t.Fatal("zero account accepted")
	}
	zeroBPS := valid
	zeroBPS.FeeBasisPoints = 0
	if _, err := NewDisbursementRail(store, stubRateSource{}, &stubPairCreator{}, zeroBPS); err == nil {
		t.Fatal("zero fee basis points accepted")
	}
	overBPS := valid
	overBPS.FeeBasisPoints = 10_001
	if _, err := NewDisbursementRail(store, stubRateSource{}, &stubPairCreator{}, overBPS); err == nil {
		t.Fatal("over-100% fee basis points accepted")
	}
}

func TestRailDisbursesAndRecordsLegs(t *testing.T) {
	rail, store, creator := railFixture(t)
	if err := rail.Disburse(context.Background(), "cvff-001"); err != nil {
		t.Fatalf("disburse: %v", err)
	}
	if creator.call != 1 {
		t.Fatalf("pair creator calls = %d", creator.call)
	}
	if _, ok := store.legs["cvff-001"]; !ok {
		t.Fatal("disbursement legs not recorded")
	}
	// Idempotent replay: legs exist, no second TigerBeetle write.
	if err := rail.Disburse(context.Background(), "cvff-001"); err != nil {
		t.Fatalf("replay disburse: %v", err)
	}
	if creator.call != 1 {
		t.Fatalf("replay posted again: %d calls", creator.call)
	}
}

func TestRailFeeComputation(t *testing.T) {
	rail, store, _ := railFixture(t)
	var captured ledger.DisbursementPairInput
	capturing := &capturePairCreator{target: &captured}
	rail.pairs = capturing
	// 700,000,000 USD cents at 1,550.25 NGN/USD = 1,085,175,000,000 kobo; 25 bps fee = 2,712,937,500 kobo.
	if err := rail.Disburse(context.Background(), "cvff-001"); err != nil {
		t.Fatalf("disburse: %v", err)
	}
	if captured.CostUSDMinor != 700_000_000 {
		t.Fatalf("cost = %d", captured.CostUSDMinor)
	}
	if captured.FeeNGNMinor != 2_712_937_500 {
		t.Fatalf("fee = %d kobo, want 2712937500", captured.FeeNGNMinor)
	}
	if _, ok := store.legs["cvff-001"]; !ok {
		t.Fatal("legs not recorded")
	}
}

type capturePairCreator struct{ target *ledger.DisbursementPairInput }

func (creator *capturePairCreator) CreateDisbursementPair(input ledger.DisbursementPairInput) (ledger.DisbursementLegs, error) {
	*creator.target = input
	return ledger.DisbursementLegs{
		ApplicationID:     input.ApplicationID,
		RateID:            input.Rate.RateID,
		NGNPerUSDMicro:    input.Rate.NGNPerUSDMicro,
		RateEffectiveDate: input.Rate.EffectiveDate,
		FeeTransferID:     tigerbeetle.ToUint128(101),
		CostTransferID:    tigerbeetle.ToUint128(102),
		FeeNGNMinor:       input.FeeNGNMinor,
		CostUSDMinor:      input.CostUSDMinor,
		CostNGNEquivalent: input.CostNGNEquivalent,
	}, nil
}

func TestRailFailsClosedOnMissingRate(t *testing.T) {
	rail, store, creator := railFixture(t)
	rail.rates = stubRateSource{err: fx.ErrRateNotFound}
	err := rail.Disburse(context.Background(), "cvff-001")
	if err == nil {
		t.Fatal("missing rate disbursed")
	}
	if creator.call != 0 {
		t.Fatal("TigerBeetle write attempted without a confirmed rate")
	}
	if store.application.State != cvff.StateReconciliationRequired {
		t.Fatalf("state = %s, want RECONCILIATION_REQUIRED", store.application.State)
	}
}

func TestRailFailsClosedOnPairError(t *testing.T) {
	rail, store, creator := railFixture(t)
	creator.err = errors.New("cluster unavailable")
	if err := rail.Disburse(context.Background(), "cvff-001"); err == nil {
		t.Fatal("pair error swallowed")
	}
	if store.application.State != cvff.StateReconciliationRequired {
		t.Fatalf("state = %s, want RECONCILIATION_REQUIRED", store.application.State)
	}
}

func TestRailRejectsWrongStateAndUnsupportedCurrency(t *testing.T) {
	rail, store, _ := railFixture(t)
	store.application.State = cvff.StateBankConfirmation
	if err := rail.Disburse(context.Background(), "cvff-001"); err == nil {
		t.Fatal("non-pending state disbursed")
	}
	rail, store, creator := railFixture(t)
	store.application.Currency = "EUR"
	if err := rail.Disburse(context.Background(), "cvff-001"); err == nil {
		t.Fatal("unsupported currency disbursed")
	}
	if creator.call != 0 {
		t.Fatal("TigerBeetle write attempted for unsupported currency")
	}
	if store.application.State != cvff.StateReconciliationRequired {
		t.Fatalf("state = %s, want RECONCILIATION_REQUIRED", store.application.State)
	}
}

// TestRailDisbursesNGNWithCBNConversion: an NGN-denominated intake
// application disburses with the USD cost leg converted at the captured CBN
// reference rate; the conversion (rate ID, micro value, effective date) is
// persisted with the legs for audit.
func TestRailDisbursesNGNWithCBNConversion(t *testing.T) {
	rail, store, _ := railFixture(t)
	store.application.Currency = "NGN"
	store.application.Amount = 1_085_175_000_000 // 7,000,000.00 USD at 1,550.25
	var captured ledger.DisbursementPairInput
	capturing := &capturePairCreator{target: &captured}
	rail.pairs = capturing
	if err := rail.Disburse(context.Background(), "cvff-001"); err != nil {
		t.Fatalf("disburse NGN application: %v", err)
	}
	if captured.CostUSDMinor != 700_000_000 {
		t.Fatalf("usd cost = %d cents, want 700000000", captured.CostUSDMinor)
	}
	if captured.CostNGNEquivalent != 1_085_175_000_000 {
		t.Fatalf("ngn equivalent = %d, want the NGN principal", captured.CostNGNEquivalent)
	}
	// 25 bps of the NGN principal.
	if captured.FeeNGNMinor != 2_712_937_500 {
		t.Fatalf("fee = %d kobo, want 2712937500", captured.FeeNGNMinor)
	}
	legs, ok := store.legs["cvff-001"]
	if !ok {
		t.Fatal("legs not recorded")
	}
	if !legs.RateEffectiveDate.Equal(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("rate effective date = %s", legs.RateEffectiveDate)
	}
}
