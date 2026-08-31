// Package fx implements the CBN reference-rate pass-through for CVFF
// disbursements. Rates are admin-entered under dual control; no external rate
// API is ever called.
package fx

import (
	"errors"
	"math/big"
	"time"
)

// MicroScale is the fixed-point scale of NGNPerUSDMicro: a rate of
// 1,550.25 NGN per USD is stored as 1_550_250_000.
const MicroScale = 1_000_000

// maxNGNPerUSDMicro bounds admin entry mistakes (100,000,000 NGN per USD).
const maxNGNPerUSDMicro = 100_000_000 * MicroScale

var (
	ErrRateInvalid    = errors.New("fx rate must be a positive NGN-per-USD micro value within approved bounds")
	ErrDualControl    = errors.New("fx rate checker must differ from maker")
	ErrRateNotFound   = errors.New("no confirmed CBN reference rate for the disbursement date")
	ErrRateConflict   = errors.New("fx rate changed concurrently")
	ErrAmountOverflow = errors.New("fx conversion overflows ledger amount width")
	// ErrRateExpired marks a rate whose dual-control confirmation window
	// lapsed; an expired rate is unavailable, never a default rate.
	ErrRateExpired = errors.New("fx rate expired unconfirmed and is unavailable")
)

// Rate is one CBN reference rate entry. It becomes usable only after a second
// principal confirms it (dual control).
type Rate struct {
	RateID         string    `json:"rate_id"`
	NGNPerUSDMicro uint64    `json:"ngn_per_usd_micro"`
	EffectiveDate  time.Time `json:"effective_date"`
	Maker          string    `json:"maker"`
	Checker        *string   `json:"checker,omitempty"`
	Confirmed      bool      `json:"confirmed"`
	CreatedAt      time.Time `json:"created_at"`
}

// NewRate validates an admin-entered rate. effectiveDate must be a UTC date.
func NewRate(rateID string, ngnPerUSDMicro uint64, effectiveDate time.Time, maker string) (Rate, error) {
	if ngnPerUSDMicro == 0 || ngnPerUSDMicro > maxNGNPerUSDMicro {
		return Rate{}, ErrRateInvalid
	}
	if maker == "" {
		return Rate{}, errors.New("fx rate maker is required")
	}
	if effectiveDate.Location() != time.UTC || effectiveDate.Hour() != 0 || effectiveDate.Minute() != 0 || effectiveDate.Second() != 0 || effectiveDate.Nanosecond() != 0 {
		return Rate{}, errors.New("fx rate effective date must be a UTC calendar date")
	}
	if rateID == "" {
		return Rate{}, errors.New("fx rate ID is required")
	}
	return Rate{RateID: rateID, NGNPerUSDMicro: ngnPerUSDMicro, EffectiveDate: effectiveDate, Maker: maker}, nil
}

// Confirm applies dual control: the checker must be a different principal.
func (rate Rate) Confirm(checker string) (Rate, error) {
	if rate.Confirmed {
		return Rate{}, ErrRateConflict
	}
	if checker == "" || checker == rate.Maker {
		return Rate{}, ErrDualControl
	}
	rate.Checker = &checker
	rate.Confirmed = true
	return rate, nil
}

// ConvertUSDToNGN converts a USD minor-unit amount into NGN minor units at the
// reference rate with deterministic half-up rounding. It fails closed on
// overflow and on unconfirmed rates.
func (rate Rate) ConvertUSDToNGN(usdMinor uint64) (uint64, error) {
	if err := rate.usable(); err != nil {
		return 0, err
	}
	if usdMinor == 0 {
		return 0, errors.New("usd amount must be non-zero")
	}
	// USD minor (cents) to NGN minor (kobo): both are 1/100 of the major unit,
	// so the minor-to-minor ratio equals the major-to-major ratio.
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(usdMinor), new(big.Int).SetUint64(rate.NGNPerUSDMicro))
	// Round half-up: add scale/2 before dividing.
	numerator.Add(numerator, big.NewInt(MicroScale/2))
	quotient := numerator.Div(numerator, big.NewInt(MicroScale))
	if !quotient.IsUint64() || quotient.Sign() == 0 {
		return 0, ErrAmountOverflow
	}
	return quotient.Uint64(), nil
}

// ConvertNGNToUSD converts an NGN minor-unit amount into USD minor units at
// the reference rate with deterministic half-up rounding: the exact form
// floor((2*ngn*scale + rate) / (2*rate)) avoids any float math. It fails
// closed on overflow, on unconfirmed rates and on amounts that convert to
// zero (a disbursement leg must never silently vanish).
func (rate Rate) ConvertNGNToUSD(ngnMinor uint64) (uint64, error) {
	if err := rate.usable(); err != nil {
		return 0, err
	}
	if ngnMinor == 0 {
		return 0, errors.New("ngn amount must be non-zero")
	}
	// usd = ngn / (rate/scale); half-up: (2*ngn*scale + rate) / (2*rate).
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(ngnMinor), big.NewInt(2*MicroScale))
	numerator.Add(numerator, new(big.Int).SetUint64(rate.NGNPerUSDMicro))
	denominator := new(big.Int).Mul(big.NewInt(2), new(big.Int).SetUint64(rate.NGNPerUSDMicro))
	quotient := numerator.Div(numerator, denominator)
	if !quotient.IsUint64() || quotient.Sign() == 0 {
		return 0, ErrAmountOverflow
	}
	return quotient.Uint64(), nil
}

func (rate Rate) usable() error {
	if !rate.Confirmed {
		return errors.New("fx rate is not confirmed under dual control")
	}
	if rate.NGNPerUSDMicro == 0 {
		return ErrRateInvalid
	}
	return nil
}
