package tariff

import (
	"reflect"
	"testing"
	"time"
)

func day(year int, month time.Month, date int) time.Time {
	return time.Date(year, month, date, 0, 0, 0, 0, time.UTC)
}

func perGRTRate(id, instrument string, minorPerUnit, floor int64, ceiling *int64, from time.Time, to *time.Time) RateRow {
	return RateRow{
		RateID: id, Instrument: instrument, Agency: instrumentAgency[instrument],
		BandLogic: BandPerGRT, Currency: "USD", RateMinorPerUnit: minorPerUnit,
		BandFloor: floor, BandCeiling: ceiling, StatutoryReference: "test-ref",
		EffectiveFrom: from, EffectiveTo: to, State: "ACTIVE",
	}
}

func freightRate(id, instrument string, bps int64, provisional bool, from time.Time, to *time.Time) RateRow {
	return RateRow{
		RateID: id, Instrument: instrument, Agency: instrumentAgency[instrument],
		BandLogic: BandGrossFreight, Currency: "USD", RateBps: bps,
		StatutoryReference: "test-ref", Provisional: provisional,
		EffectiveFrom: from, EffectiveTo: to, State: "ACTIVE",
	}
}

func baseRequest() AssessRequest {
	return AssessRequest{
		VesselGRT: 10000, VesselClass: VesselTanker, EntityRef: "ACME-SHIPPING",
		CargoCategory: "PETROLEUM", VoyageType: VoyageInternational, RouteKind: RouteSea,
		NigeriaPortCall: true, GrossFreightUSDMinor: 100000000,
	}
}

func lineFor(computation Computation, instrument string) AssessmentLine {
	for _, line := range computation.Lines {
		if line.Instrument == instrument {
			return line
		}
	}
	return AssessmentLine{}
}

func TestRoundHalfUp(t *testing.T) {
	cases := []struct {
		numerator, denominator, want int64
	}{
		{150, 100, 2}, // 1.5 -> 2 (half up)
		{149, 100, 1}, // 1.49 -> 1
		{125, 100, 1}, // 1.25 -> 1
		{5, 10, 1},    // 0.5 -> 1
		{447, 100, 4}, // 4.47 -> 4
		{450, 100, 5}, // 4.5 -> 5 (half up)
		{3, 100, 0},   // 0.03 -> 0
		{0, 100, 0},
	}
	for _, tc := range cases {
		if got := roundHalfUp(tc.numerator, tc.denominator); got != tc.want {
			t.Fatalf("roundHalfUp(%d, %d) = %d, want %d", tc.numerator, tc.denominator, got, tc.want)
		}
	}
}

func TestPerGRTBandMath(t *testing.T) {
	rates := []RateRow{perGRTRate("r1", InstrumentNPAShipDues, 147, 0, nil, day(2007, 1, 1), nil)}
	computation := Compute(baseRequest(), rates, nil, day(2026, 1, 15))
	line := lineFor(computation, InstrumentNPAShipDues)
	if line.Applicability != LineCharged || line.AmountMinor != 10000*147 {
		t.Fatalf("ship dues line = %+v, want charged %d", line, 10000*147)
	}
}

func TestPerGRTBoundaryBands(t *testing.T) {
	lowCeiling := int64(5000)
	highCeiling := int64(20000)
	rates := []RateRow{
		perGRTRate("low", InstrumentNPAShipDues, 100, 0, &lowCeiling, day(2007, 1, 1), nil),
		perGRTRate("mid", InstrumentNPAShipDues, 147, 5000, &highCeiling, day(2007, 1, 1), nil),
		perGRTRate("high", InstrumentNPAShipDues, 180, 20000, nil, day(2007, 1, 1), nil),
	}
	cases := []struct {
		grt, wantMinor int64
	}{
		{4999, 4999 * 100},
		{5000, 5000 * 147}, // floor inclusive
		{19999, 19999 * 147},
		{20000, 20000 * 180}, // ceiling exclusive
		{1, 100},
	}
	for _, tc := range cases {
		request := baseRequest()
		request.VesselGRT = tc.grt
		line := lineFor(Compute(request, rates, nil, day(2026, 1, 15)), InstrumentNPAShipDues)
		if line.Applicability != LineCharged || line.AmountMinor != tc.wantMinor {
			t.Fatalf("grt %d: line = %+v, want %d", tc.grt, line, tc.wantMinor)
		}
	}
}

func TestPercentGrossFreightMath(t *testing.T) {
	rates := []RateRow{freightRate("f1", InstrumentNPALevyAct, 300, false, day(2007, 1, 1), nil)}
	request := baseRequest()
	request.GrossFreightUSDMinor = 999999
	computation := Compute(request, rates, nil, day(2010, 6, 1))
	line := lineFor(computation, InstrumentNPALevyAct)
	// 3% of 999999 = 29999.97 -> 30000 half-up.
	if line.Applicability != LineCharged || line.AmountMinor != 30000 {
		t.Fatalf("levy line = %+v, want charged 30000", line)
	}
}

func TestEffectiveWindowsSelectStatute(t *testing.T) {
	spl2012from := day(2013, 1, 1)
	rates := []RateRow{
		freightRate("s15", InstrumentNPALevyAct, 300, false, day(2007, 1, 1), &spl2012from),
		freightRate("spl2012", InstrumentSPL2012, 20, true, day(2013, 1, 1), nil),
	}
	request := baseRequest()
	request.GrossFreightUSDMinor = 1000000
	// 2010: the old s.15 3% levy is effective; the 2012 levy is not yet.
	historical := Compute(request, rates, nil, day(2010, 6, 1))
	if line := lineFor(historical, InstrumentNPALevyAct); line.Applicability != LineCharged || line.AmountMinor != 30000 {
		t.Fatalf("2010 s.15 line = %+v, want 30000", line)
	}
	if line := lineFor(historical, InstrumentSPL2012); line.Applicability != LineUnrated {
		t.Fatalf("2010 SPL2012 line = %+v, want UNRATED (not yet effective)", line)
	}
	// 2020: the 2012 levy (provisional) is effective; s.15's window closed.
	current := Compute(request, rates, nil, day(2020, 6, 1))
	if line := lineFor(current, InstrumentNPALevyAct); line.Applicability != LineUnrated {
		t.Fatalf("2020 s.15 line = %+v, want UNRATED (window closed)", line)
	}
	if line := lineFor(current, InstrumentSPL2012); line.Applicability != LineCharged || line.AmountMinor != 2000 || !line.Provisional {
		t.Fatalf("2020 SPL2012 line = %+v, want charged 2000 provisional", line)
	}
}

func TestExemptionWindowsAndMatching(t *testing.T) {
	rates := []RateRow{perGRTRate("lng", InstrumentNimasaLNGDue, 30, 0, nil, day(2008, 1, 1), nil)}
	exemptions := []ExemptionRow{{
		ExemptionID: "ex-nlng", Instrument: InstrumentNimasaLNGDue, MatchKind: MatchEntity, MatchValue: "NLNG",
		StatutoryBasis: "NIMASA Act s.19", EvidenceRequirement: "registration cert",
		EffectiveFrom: day(2008, 1, 1), State: "ACTIVE",
	}}
	request := baseRequest()
	request.VesselClass = VesselLNGCarrier
	request.EntityRef = "NLNG"
	computation := Compute(request, rates, exemptions, day(2026, 1, 20))
	line := lineFor(computation, InstrumentNimasaLNGDue)
	if line.Applicability != LineExempt || line.ExemptionID != "ex-nlng" || line.AmountMinor != 0 {
		t.Fatalf("NLNG line = %+v, want EXEMPT", line)
	}
	if len(computation.Exemptions) != 1 {
		t.Fatalf("applied exemptions = %+v", computation.Exemptions)
	}
	// A non-NLNG entity pays the due.
	request.EntityRef = "OTHER-LNG"
	computation = Compute(request, rates, exemptions, day(2026, 1, 20))
	if line := lineFor(computation, InstrumentNimasaLNGDue); line.Applicability != LineCharged || line.AmountMinor != 10000*30 {
		t.Fatalf("non-NLNG line = %+v, want charged %d", line, 10000*30)
	}
	// Before the exemption's window the same entity pays.
	computation = Compute(request, rates, []ExemptionRow{{
		ExemptionID: "ex-nlng", Instrument: InstrumentNimasaLNGDue, MatchKind: MatchEntity, MatchValue: "NLNG",
		StatutoryBasis: "b", EvidenceRequirement: "e",
		EffectiveFrom: day(2008, 1, 1), EffectiveTo: &[]time.Time{day(2009, 1, 1)}[0], State: "ACTIVE",
	}}, day(2026, 1, 20))
	request.EntityRef = "NLNG"
	computation = Compute(request, rates, []ExemptionRow{{
		ExemptionID: "ex-nlng", Instrument: InstrumentNimasaLNGDue, MatchKind: MatchEntity, MatchValue: "NLNG",
		StatutoryBasis: "b", EvidenceRequirement: "e",
		EffectiveFrom: day(2008, 1, 1), EffectiveTo: &[]time.Time{day(2009, 1, 1)}[0], State: "ACTIVE",
	}}, day(2026, 1, 20))
	if line := lineFor(computation, InstrumentNimasaLNGDue); line.Applicability != LineCharged {
		t.Fatalf("expired-window exemption must not apply: %+v", line)
	}
	// DRAFT rules never apply.
	computation = Compute(request, rates, []ExemptionRow{{
		ExemptionID: "ex-draft", Instrument: InstrumentNimasaLNGDue, MatchKind: MatchEntity, MatchValue: "NLNG",
		StatutoryBasis: "b", EvidenceRequirement: "e", EffectiveFrom: day(2008, 1, 1), State: "DRAFT",
	}}, day(2026, 1, 20))
	if line := lineFor(computation, InstrumentNimasaLNGDue); line.Applicability != LineCharged {
		t.Fatalf("DRAFT exemption must not apply: %+v", line)
	}
}

func TestApplicabilityMatrix(t *testing.T) {
	rates := []RateRow{
		perGRTRate("dues", InstrumentNPAShipDues, 147, 0, nil, day(2007, 1, 1), nil),
		freightRate("spl", InstrumentSPL2012, 20, true, day(2013, 1, 1), nil),
		freightRate("cabotage", InstrumentCabotageSurcharge, 200, false, day(2004, 1, 1), nil),
		perGRTRate("lng", InstrumentNimasaLNGDue, 30, 0, nil, day(2008, 1, 1), nil),
	}
	// International sea voyage: cabotage + NIWA not applicable.
	computation := Compute(baseRequest(), rates, nil, day(2026, 1, 15))
	if line := lineFor(computation, InstrumentCabotageSurcharge); line.Applicability != LineNotApplicable {
		t.Fatalf("cabotage on international voyage = %+v", line)
	}
	if line := lineFor(computation, InstrumentNIWAInlandCharge); line.Applicability != LineNotApplicable {
		t.Fatalf("NIWA on sea voyage = %+v", line)
	}
	if line := lineFor(computation, InstrumentNimasaLNGDue); line.Applicability != LineNotApplicable {
		t.Fatalf("LNG due on tanker = %+v", line)
	}
	// Cabotage voyage: 2% surcharge applies.
	request := baseRequest()
	request.VoyageType = VoyageCabotage
	computation = Compute(request, rates, nil, day(2026, 1, 15))
	if line := lineFor(computation, InstrumentCabotageSurcharge); line.Applicability != LineCharged || line.AmountMinor != 2000000 {
		t.Fatalf("cabotage line = %+v, want 2000000", line)
	}
	// Inland voyage: NIWA applies but has no rate row -> UNRATED, visible.
	request.RouteKind = RouteInlandWaterway
	computation = Compute(request, rates, nil, day(2026, 1, 15))
	if line := lineFor(computation, InstrumentNIWAInlandCharge); line.Applicability != LineUnrated {
		t.Fatalf("NIWA unrated line = %+v, want UNRATED", line)
	}
	// No Nigerian port call: ship dues do not apply.
	request = baseRequest()
	request.NigeriaPortCall = false
	computation = Compute(request, rates, nil, day(2026, 1, 15))
	if line := lineFor(computation, InstrumentNPAShipDues); line.Applicability != LineNotApplicable {
		t.Fatalf("ship dues without port call = %+v", line)
	}
}

func TestDeterministicOutput(t *testing.T) {
	rates := []RateRow{
		perGRTRate("dues", InstrumentNPAShipDues, 147, 0, nil, day(2007, 1, 1), nil),
		freightRate("spl", InstrumentSPL2012, 20, true, day(2013, 1, 1), nil),
	}
	first := Compute(baseRequest(), rates, nil, day(2026, 1, 15))
	second := Compute(baseRequest(), rates, nil, day(2026, 1, 15))
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical inputs must produce identical computations")
	}
	usd, ngn := first.Totals()
	if usd != 10000*147+roundHalfUp(100000000*20, 10000) || ngn != 0 {
		t.Fatalf("totals = %d/%d", usd, ngn)
	}
}

func TestValidateFailsClosed(t *testing.T) {
	bad := baseRequest()
	bad.VesselGRT = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("zero GRT must reject")
	}
	bad = baseRequest()
	bad.VesselClass = "HOVERCRAFT"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown vessel class must reject")
	}
	bad = baseRequest()
	bad.VoyageType = "NEARBY"
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown voyage type must reject")
	}
	bad = baseRequest()
	bad.AsOf = "15-01-2026"
	if err := bad.Validate(); err == nil {
		t.Fatal("malformed asOf must reject")
	}
}
