package tariff

import (
	"fmt"
	"time"
)

// instrumentOrder is the canonical statutory order of assessment lines;
// output is byte-stable for identical inputs.
var instrumentOrder = []string{
	InstrumentNPAShipDues,
	InstrumentNPALevyAct,
	InstrumentSPL2012,
	InstrumentCabotageSurcharge,
	InstrumentNimasaLNGDue,
	InstrumentNIWAInlandCharge,
}

var instrumentAgency = map[string]string{
	InstrumentNPAShipDues:       AgencyNPA,
	InstrumentNPALevyAct:        AgencyNIMASA,
	InstrumentSPL2012:           AgencyNIMASA,
	InstrumentCabotageSurcharge: AgencyFMMBE,
	InstrumentNimasaLNGDue:      AgencyNIMASA,
	InstrumentNIWAInlandCharge:  AgencyNIWA,
}

// applicability documents which declarations each instrument touches
// (engine logic from the statute; the rate rows carry no applicability
// policy of their own).
func applicability(instrument string, request AssessRequest) (bool, string) {
	switch instrument {
	case InstrumentNPAShipDues:
		if request.NigeriaPortCall {
			return true, "Nigerian port call: NPA ship dues on vessel GRT"
		}
		return false, "no Nigerian port call"
	case InstrumentNPALevyAct:
		if request.NigeriaPortCall {
			return true, "in/out cargo ship calling a Nigerian port: NIMASA Act s.15 levy on gross freight"
		}
		return false, "no Nigerian port call"
	case InstrumentSPL2012:
		if request.NigeriaPortCall {
			return true, "in/out cargo ship calling a Nigerian port: Sea Protection Levy 2012 on gross freight"
		}
		return false, "no Nigerian port call"
	case InstrumentCabotageSurcharge:
		if request.VoyageType == VoyageCabotage {
			return true, "cabotage (coastal) voyage: 2% surcharge accruing to CVFF"
		}
		return false, "international voyage — cabotage surcharge applies to coastal trade only"
	case InstrumentNimasaLNGDue:
		if request.VesselClass == VesselLNGCarrier && request.NigeriaPortCall {
			return true, "LNG carrier calling a Nigerian port: NIMASA US$0.30/GRT due"
		}
		return false, "not an LNG carrier on a Nigerian port call"
	case InstrumentNIWAInlandCharge:
		if request.RouteKind == RouteInlandWaterway {
			return true, "inland-waterways voyage: NIWA s.28 charge"
		}
		return false, "sea voyage — NIWA charges apply to inland waterways"
	}
	return false, "unknown instrument"
}

// windowContains reports whether asOf falls inside [from, to).
func windowContains(asOf, from time.Time, to *time.Time) bool {
	asOfDay := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	if asOfDay.Before(fromDay) {
		return false
	}
	if to == nil {
		return true
	}
	toDay := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return !asOfDay.After(toDay)
}

// bandMatches selects PER_GRT_BAND rows by [floor, ceiling) on vessel GRT.
func bandMatches(rate RateRow, grt int64) bool {
	if rate.BandLogic != BandPerGRT {
		return true
	}
	if grt < rate.BandFloor {
		return false
	}
	return rate.BandCeiling == nil || grt < *rate.BandCeiling
}

// rateDescription renders the applied rate deterministically for audit.
func rateDescription(rate RateRow) string {
	if rate.BandLogic == BandPerGRT {
		band := fmt.Sprintf("GRT >= %d", rate.BandFloor)
		if rate.BandCeiling != nil {
			band = fmt.Sprintf("%d <= GRT < %d", rate.BandFloor, *rate.BandCeiling)
		}
		return fmt.Sprintf("%d %s minor/GRT (%s)", rate.RateMinorPerUnit, rate.Currency, band)
	}
	return fmt.Sprintf("%d bps of gross freight", rate.RateBps)
}

// exemptionMatches evaluates one rule against the declaration.
func exemptionMatches(exemption ExemptionRow, request AssessRequest) bool {
	switch exemption.MatchKind {
	case MatchEntity:
		return exemption.MatchValue == request.EntityRef
	case MatchCargoCategory:
		return exemption.MatchValue == request.CargoCategory
	case MatchVoyageFlag:
		for _, flag := range request.VoyageFlags {
			if flag == exemption.MatchValue {
				return true
			}
		}
		return false
	case MatchCabotageTrade:
		return request.VoyageType == VoyageCabotage
	}
	return false
}

// Computation is the pure engine output: line items plus applied exemptions.
type Computation struct {
	Lines      []AssessmentLine
	Exemptions []ExemptionRow // rules actually applied, in line order
}

// Compute evaluates one declaration against the given ACTIVE-window rate and
// exemption rows. It is pure (no I/O, no clock — asOf is an input), which is
// what makes assessments reproducible for audit.
func Compute(request AssessRequest, rates []RateRow, exemptions []ExemptionRow, asOf time.Time) Computation {
	computation := Computation{Lines: make([]AssessmentLine, 0, len(instrumentOrder))}
	for index, instrument := range instrumentOrder {
		line := AssessmentLine{
			LineNo:      index + 1,
			Instrument:  instrument,
			Agency:      instrumentAgency[instrument],
			Currency:    "USD",
			AmountMinor: 0,
		}
		applies, basis := applicability(instrument, request)
		line.Basis = basis
		if !applies {
			line.Applicability = LineNotApplicable
			computation.Lines = append(computation.Lines, line)
			continue
		}
		// Select the effective ACTIVE rate row (band-aware).
		var selected *RateRow
		for i := range rates {
			rate := &rates[i]
			if rate.Instrument != instrument || rate.State != "ACTIVE" {
				continue
			}
			if !windowContains(asOf, rate.EffectiveFrom, rate.EffectiveTo) || !bandMatches(*rate, request.VesselGRT) {
				continue
			}
			selected = rate
			break
		}
		if selected == nil {
			line.Applicability = LineUnrated
			line.Basis = basis + " — no ACTIVE effective rate row configured"
			computation.Lines = append(computation.Lines, line)
			continue
		}
		line.StatutoryReference = selected.StatutoryReference
		line.RateDescription = rateDescription(*selected)
		line.Provisional = selected.Provisional
		line.Currency = selected.Currency
		// Exemptions are checked BEFORE charging: deterministic,
		// machine-checkable, evidence-bound.
		for _, exemption := range exemptions {
			if exemption.Instrument != instrument || exemption.State != "ACTIVE" {
				continue
			}
			if !windowContains(asOf, exemption.EffectiveFrom, exemption.EffectiveTo) {
				continue
			}
			if exemptionMatches(exemption, request) {
				line.Applicability = LineExempt
				line.ExemptionID = exemption.ExemptionID
				computation.Exemptions = append(computation.Exemptions, exemption)
				break
			}
		}
		if line.Applicability == LineExempt {
			computation.Lines = append(computation.Lines, line)
			continue
		}
		switch selected.BandLogic {
		case BandPerGRT:
			line.AmountMinor = request.VesselGRT * selected.RateMinorPerUnit
		case BandGrossFreight:
			line.AmountMinor = roundHalfUp(request.GrossFreightUSDMinor*selected.RateBps, 10000)
		}
		line.Applicability = LineCharged
		computation.Lines = append(computation.Lines, line)
	}
	return computation
}

// Totals sums CHARGED lines per currency.
func (computation Computation) Totals() (usdMinor, ngnMinor int64) {
	for _, line := range computation.Lines {
		if line.Applicability != LineCharged {
			continue
		}
		switch line.Currency {
		case "USD":
			usdMinor += line.AmountMinor
		case "NGN":
			ngnMinor += line.AmountMinor
		}
	}
	return usdMinor, ngnMinor
}
