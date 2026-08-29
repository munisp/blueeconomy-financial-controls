package workflow

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/fx"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
	"go.temporal.io/sdk/temporal"
)

// fxRateSource loads the dual-control-confirmed CBN reference rate.
type fxRateSource interface {
	ConfirmedForDate(ctx context.Context, date time.Time) (fx.Rate, error)
}

// pairCreator posts the FX-paired disbursement transfers to TigerBeetle. The
// traced variant carries the activity context so the ledger client spans join
// the workflow trace (OTEL_DESIGN §3: TigerBeetle coverage is client-side).
type pairCreator interface {
	CreateDisbursementPairTraced(ctx context.Context, input ledger.DisbursementPairInput) (ledger.DisbursementLegs, error)
}

// disbursementStore is the CVFF persistence boundary used by the rail.
type disbursementStore interface {
	Get(ctx context.Context, applicationID string) (cvff.Application, error)
	DisbursementLegs(ctx context.Context, applicationID string) (cvff.DisbursementLegs, error)
	RecordDisbursementLegs(ctx context.Context, legs cvff.DisbursementLegs) error
	Transition(ctx context.Context, applicationID string, expectedVersion int64, move func(cvff.Application) (cvff.Application, error), eventType string) (cvff.Application, error)
}

// DisbursementRailConfig carries the approved account/ledger topology. Every
// value is operator-supplied configuration; there are no defaults.
type DisbursementRailConfig struct {
	NGNDebitAccountID  tigerbeetle.Uint128
	NGNCreditAccountID tigerbeetle.Uint128
	USDDebitAccountID  tigerbeetle.Uint128
	USDCreditAccountID tigerbeetle.Uint128
	NGNLedger          uint32
	USDLedger          uint32
	FeeCode            uint16
	CostCode           uint16
	// FeeBasisPoints is the approved NGN custodial fee in basis points of the
	// FX-adjusted NGN equivalent of the USD cost.
	FeeBasisPoints uint64
}

// DisbursementRail implements Disburser: it captures the CBN reference rate at
// the disbursement date, posts the paired NGN fee and USD cost transfers, and
// records the dual-ledger legs. Any failure moves the application into the
// fail-closed RECONCILIATION_REQUIRED branch.
type DisbursementRail struct {
	store  disbursementStore
	rates  fxRateSource
	pairs  pairCreator
	config DisbursementRailConfig
	now    func() time.Time
}

// NewDisbursementRail validates the topology and fails closed on any gap.
func NewDisbursementRail(store disbursementStore, rates fxRateSource, pairs pairCreator, config DisbursementRailConfig) (*DisbursementRail, error) {
	if store == nil || rates == nil || pairs == nil {
		return nil, errors.New("disbursement rail requires application store, fx rate source and pair creator")
	}
	zero := tigerbeetle.Uint128{}
	if config.NGNDebitAccountID == zero || config.NGNCreditAccountID == zero ||
		config.USDDebitAccountID == zero || config.USDCreditAccountID == zero {
		return nil, errors.New("disbursement rail account IDs must be non-zero")
	}
	if config.NGNDebitAccountID == config.NGNCreditAccountID || config.USDDebitAccountID == config.USDCreditAccountID {
		return nil, errors.New("disbursement rail debit and credit accounts must differ")
	}
	if config.NGNLedger == 0 || config.USDLedger == 0 || config.FeeCode == 0 || config.CostCode == 0 {
		return nil, errors.New("disbursement rail ledgers and codes must be non-zero")
	}
	if config.FeeBasisPoints == 0 || config.FeeBasisPoints > 10_000 {
		return nil, errors.New("custodial fee basis points must be within (0, 10000]")
	}
	return &DisbursementRail{store: store, rates: rates, pairs: pairs, config: config, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Disburse executes the FX pass-through disbursement for one application.
func (rail *DisbursementRail) Disburse(ctx context.Context, applicationID string) error {
	application, err := rail.store.Get(ctx, applicationID)
	if err != nil {
		return fmt.Errorf("load application: %w", err)
	}
	if application.State != cvff.StateDisbursementPending {
		return temporal.NewNonRetryableApplicationError("application is not disbursement-pending", "cvff-control-rejection", nil)
	}
	// Idempotent replay: legs already recorded means the pair already posted.
	if _, err := rail.store.DisbursementLegs(ctx, applicationID); err == nil {
		return nil
	} else if !errors.Is(err, cvff.ErrNotFound) {
		return fmt.Errorf("check existing disbursement legs: %w", err)
	}
	disbursementDate := time.Date(rail.now().Year(), rail.now().Month(), rail.now().Day(), 0, 0, 0, 0, time.UTC)
	rate, err := rail.rates.ConfirmedForDate(ctx, disbursementDate)
	if err != nil {
		return rail.failClosed(ctx, application, fmt.Errorf("capture CBN reference rate: %w", err))
	}
	// Both currencies are carried explicitly: the NGN custodial fee leg and
	// the USD cost leg, with the CBN-rate conversion between them computed at
	// disbursement time and persisted (rate ID, micro rate, effective date and
	// timestamp) for audit. No silent defaults, no float math.
	var fee, costUSDMinor, costNGNEquivalent uint64
	switch application.Currency {
	case "NGN":
		// NGN-denominated intake (beneficiary portal): the fee is charged on
		// the NGN principal and the USD cost leg is the principal converted at
		// the captured CBN reference rate.
		fee, err = custodialFee(application.Amount, rail.config.FeeBasisPoints)
		if err != nil {
			return rail.failClosed(ctx, application, fmt.Errorf("compute NGN custodial fee: %w", err))
		}
		costUSDMinor, err = rate.ConvertNGNToUSD(application.Amount)
		if err != nil {
			return rail.failClosed(ctx, application, fmt.Errorf("fx-convert NGN principal to USD cost: %w", err))
		}
		costNGNEquivalent = application.Amount
	case "USD":
		// USD-denominated applications (recovery and reconciliation tooling):
		// the USD cost leg is the principal itself and the fee is charged on
		// its NGN equivalent at the captured rate.
		costNGNEquivalent, err = rate.ConvertUSDToNGN(application.Amount)
		if err != nil {
			return rail.failClosed(ctx, application, fmt.Errorf("fx-adjust USD cost: %w", err))
		}
		fee, err = custodialFee(costNGNEquivalent, rail.config.FeeBasisPoints)
		if err != nil {
			return rail.failClosed(ctx, application, fmt.Errorf("compute NGN custodial fee: %w", err))
		}
		costUSDMinor = application.Amount
	default:
		return rail.failClosed(ctx, application, fmt.Errorf("application currency %q is not an approved ledger currency (NGN, USD)", application.Currency))
	}
	legs, err := rail.pairs.CreateDisbursementPairTraced(ctx, ledger.DisbursementPairInput{
		ApplicationID:      applicationID,
		FeeNGNMinor:        fee,
		CostUSDMinor:       costUSDMinor,
		CostNGNEquivalent:  costNGNEquivalent,
		Rate:               rate,
		NGNDebitAccountID:  rail.config.NGNDebitAccountID,
		NGNCreditAccountID: rail.config.NGNCreditAccountID,
		USDDebitAccountID:  rail.config.USDDebitAccountID,
		USDCreditAccountID: rail.config.USDCreditAccountID,
		NGNLedger:          rail.config.NGNLedger,
		USDLedger:          rail.config.USDLedger,
		FeeCode:            rail.config.FeeCode,
		CostCode:           rail.config.CostCode,
	})
	if err != nil {
		return rail.failClosed(ctx, application, fmt.Errorf("post disbursement pair: %w", err))
	}
	if err := rail.store.RecordDisbursementLegs(ctx, cvff.DisbursementLegs{
		ApplicationID:     legs.ApplicationID,
		RateID:            legs.RateID,
		NGNPerUSDMicro:    legs.NGNPerUSDMicro,
		RateEffectiveDate: legs.RateEffectiveDate,
		FeeTransferID:     legs.FeeTransferID.String(),
		CostTransferID:    legs.CostTransferID.String(),
		FeeNGNMinor:       legs.FeeNGNMinor,
		CostUSDMinor:      legs.CostUSDMinor,
		CostNGNEquivalent: legs.CostNGNEquivalent,
	}); err != nil {
		return rail.failClosed(ctx, application, fmt.Errorf("record disbursement legs: %w", err))
	}
	return nil
}

// custodialFee computes base * basisPoints / 10_000 in integer math with an
// explicit big.Int overflow guard on the multiply; a fee rounding to zero is
// a hard error because a disbursement must never post an empty fee leg.
func custodialFee(base uint64, basisPoints uint64) (uint64, error) {
	if base == 0 {
		return 0, errors.New("fee base must be non-zero")
	}
	product := new(big.Int).Mul(new(big.Int).SetUint64(base), new(big.Int).SetUint64(basisPoints))
	quotient := product.Div(product, big.NewInt(10_000))
	if !quotient.IsUint64() {
		return 0, errors.New("custodial fee overflows ledger amount width")
	}
	if quotient.Sign() == 0 {
		return 0, errors.New("custodial fee rounds to zero")
	}
	return quotient.Uint64(), nil
}

// failClosed moves the application to RECONCILIATION_REQUIRED and returns the
// cause as a non-retryable error so Temporal does not retry a contradictory
// disbursement.
func (rail *DisbursementRail) failClosed(ctx context.Context, application cvff.Application, cause error) error {
	if _, err := rail.store.Transition(ctx, application.ApplicationID, application.Version, cvff.RequireReconciliation, "cvff.reconciliation_required"); err != nil {
		return fmt.Errorf("%v; failed to record reconciliation state: %w", cause, err)
	}
	return temporal.NewNonRetryableApplicationError(cause.Error(), "cvff-disbursement-failure", cause)
}
