package fx

import (
	"errors"
	"math"
	"testing"
	"time"
)

func date(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func TestNewRateValidation(t *testing.T) {
	if _, err := NewRate("rate-1", 1_550_250_000, date(2026, 8, 28), "kc-maker"); err != nil {
		t.Fatalf("valid rate rejected: %v", err)
	}
	if _, err := NewRate("rate-1", 0, date(2026, 8, 28), "kc-maker"); !errors.Is(err, ErrRateInvalid) {
		t.Fatalf("zero rate error = %v", err)
	}
	if _, err := NewRate("rate-1", maxNGNPerUSDMicro+1, date(2026, 8, 28), "kc-maker"); !errors.Is(err, ErrRateInvalid) {
		t.Fatalf("out-of-bounds rate error = %v", err)
	}
	if _, err := NewRate("rate-1", 1_550_250_000, date(2026, 8, 28).Add(time.Hour), "kc-maker"); err == nil {
		t.Fatal("non-midnight effective date accepted")
	}
	if _, err := NewRate("", 1_550_250_000, date(2026, 8, 28), "kc-maker"); err == nil {
		t.Fatal("empty rate ID accepted")
	}
	if _, err := NewRate("rate-1", 1_550_250_000, date(2026, 8, 28), ""); err == nil {
		t.Fatal("empty maker accepted")
	}
}

func TestConfirmRequiresDistinctChecker(t *testing.T) {
	rate, err := NewRate("rate-1", 1_550_250_000, date(2026, 8, 28), "kc-maker")
	if err != nil {
		t.Fatalf("new rate: %v", err)
	}
	if _, err := rate.Confirm(rate.Maker); !errors.Is(err, ErrDualControl) {
		t.Fatalf("self-confirm error = %v", err)
	}
	if _, err := rate.Confirm(""); !errors.Is(err, ErrDualControl) {
		t.Fatalf("empty checker error = %v", err)
	}
	confirmed, err := rate.Confirm("kc-checker")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !confirmed.Confirmed || confirmed.Checker == nil || *confirmed.Checker != "kc-checker" {
		t.Fatalf("unexpected confirmed rate: %+v", confirmed)
	}
	if _, err := confirmed.Confirm("kc-other"); !errors.Is(err, ErrRateConflict) {
		t.Fatalf("re-confirm error = %v", err)
	}
}

func confirmedRate(t *testing.T, micro uint64) Rate {
	t.Helper()
	rate, err := NewRate("rate-1", micro, date(2026, 8, 28), "kc-maker")
	if err != nil {
		t.Fatalf("new rate: %v", err)
	}
	confirmed, err := rate.Confirm("kc-checker")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	return confirmed
}

func TestConvertUSDToNGN(t *testing.T) {
	// 1 USD = 1,550.25 NGN; 100.00 USD (10,000 cents) = 155,025.00 NGN (15,502,500 kobo).
	rate := confirmedRate(t, 1_550_250_000)
	ngn, err := rate.ConvertUSDToNGN(10_000)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if ngn != 15_502_500 {
		t.Fatalf("converted = %d kobo, want 15502500", ngn)
	}
}

func TestConvertRoundsHalfUp(t *testing.T) {
	// 1 cent at 1.0000005 NGN/USD is not representable; use a rate producing a
	// fractional kobo: 1 cent at 0.015 NGN/USD => 0.015 kobo rounds... instead
	// verify with a rate whose product leaves exactly half a unit.
	rate := confirmedRate(t, MicroScale/2) // 0.5 NGN per USD
	ngn, err := rate.ConvertUSDToNGN(1)    // 0.5 kobo -> 1 kobo half-up
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if ngn != 1 {
		t.Fatalf("half-up rounding = %d, want 1", ngn)
	}
}

func TestConvertFailsClosed(t *testing.T) {
	rate, err := NewRate("rate-1", 1_550_250_000, date(2026, 8, 28), "kc-maker")
	if err != nil {
		t.Fatalf("new rate: %v", err)
	}
	if _, err := rate.ConvertUSDToNGN(10_000); err == nil {
		t.Fatal("unconfirmed rate converted")
	}
	confirmed := confirmedRate(t, 1_550_250_000)
	if _, err := confirmed.ConvertUSDToNGN(0); err == nil {
		t.Fatal("zero amount converted")
	}
	huge := confirmedRate(t, maxNGNPerUSDMicro)
	if _, err := huge.ConvertUSDToNGN(math.MaxUint64); !errors.Is(err, ErrAmountOverflow) {
		t.Fatalf("overflow error = %v", err)
	}
}
