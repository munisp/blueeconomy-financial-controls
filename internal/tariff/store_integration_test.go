//go:build integration

package tariff

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration harness: real PostgreSQL, real migration (DATABASE_URL +
// MIGRATION_PATH, repo convention). Each test gets a clean public schema.
func openStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration tests")
	}
	migrationPath := os.Getenv("MIGRATION_PATH")
	if migrationPath == "" {
		t.Fatal("MIGRATION_PATH is required for integration tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	statement, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(statement)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return store, pool
}

func seededAssessRequest() AssessRequest {
	return AssessRequest{
		VesselGRT: 25000, VesselClass: VesselTanker, EntityRef: "ACME-SHIPPING",
		CargoCategory: "PETROLEUM", VoyageType: VoyageInternational, RouteKind: RouteSea,
		NigeriaPortCall: true, GrossFreightUSDMinor: 500000000, AsOf: "2026-01-15",
	}
}

// TestSeededAssessmentAgainstResearchRates exercises the seeded statutory
// rates end-to-end (research-verified numbers, SPL 2012 provisional).
func TestSeededAssessmentAgainstResearchRates(t *testing.T) {
	store, pool := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	assessment, err := store.Assess(ctx, seededAssessRequest(), "idem-seeded-1", "officer:assessor", "corr-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if len(assessment.Lines) != 6 {
		t.Fatalf("expected 6 itemized lines, got %d", len(assessment.Lines))
	}
	byInstrument := map[string]AssessmentLine{}
	for _, line := range assessment.Lines {
		byInstrument[line.Instrument] = line
	}
	// NPA ship dues: 25000 GRT x US$1.47 = US$36,750.00 -> 3675000 minor.
	if line := byInstrument[InstrumentNPAShipDues]; line.Applicability != LineCharged || line.AmountMinor != 3675000 {
		t.Fatalf("ship dues = %+v", line)
	}
	// s.15 3% levy: window closed in 2026 -> UNRATED (visible, not silently dropped).
	if line := byInstrument[InstrumentNPALevyAct]; line.Applicability != LineUnrated {
		t.Fatalf("s.15 in 2026 = %+v, want UNRATED", line)
	}
	// SPL 2012: 0.2% of US$5,000,000.00 freight = US$10,000.00 -> 1000000 minor, provisional.
	if line := byInstrument[InstrumentSPL2012]; line.Applicability != LineCharged || line.AmountMinor != 1000000 || !line.Provisional {
		t.Fatalf("SPL 2012 = %+v, want charged 1000000 provisional", line)
	}
	// International voyage: no cabotage surcharge; sea route: NIWA N/A; tanker: no LNG due.
	if line := byInstrument[InstrumentCabotageSurcharge]; line.Applicability != LineNotApplicable {
		t.Fatalf("cabotage = %+v", line)
	}
	if line := byInstrument[InstrumentNIWAInlandCharge]; line.Applicability != LineNotApplicable {
		t.Fatalf("NIWA = %+v", line)
	}
	if line := byInstrument[InstrumentNimasaLNGDue]; line.Applicability != LineNotApplicable {
		t.Fatalf("LNG due = %+v", line)
	}
	if assessment.TotalUSDMinor != 3675000+1000000 || assessment.TotalNGNMinor != 0 {
		t.Fatalf("totals = %d/%d", assessment.TotalUSDMinor, assessment.TotalNGNMinor)
	}
	// Outbox: exactly one assessment event, no exemption events.
	var assessmentEvents, exemptionEvents int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tariff_outbox WHERE event_type = 'tariff.assessment.created'`).Scan(&assessmentEvents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tariff_outbox WHERE event_type = 'tariff.exemption.applied'`).Scan(&exemptionEvents); err != nil {
		t.Fatal(err)
	}
	if assessmentEvents != 1 || exemptionEvents != 0 {
		t.Fatalf("outbox = %d assessment / %d exemption events", assessmentEvents, exemptionEvents)
	}
}

// TestNLNGExemptionEndToEnd is the NLNG-pattern scenario from the research:
// an NLNG LNG carrier must NOT be charged the NIMASA US$0.30/GRT due, and
// every applied exemption emits a full audit trail.
func TestNLNGExemptionEndToEnd(t *testing.T) {
	store, pool := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	request := seededAssessRequest()
	request.VesselClass = VesselLNGCarrier
	request.EntityRef = "NLNG"
	request.CargoCategory = "LNG_EXPORT"
	assessment, err := store.Assess(ctx, request, "idem-nlng-1", "officer:assessor", "corr-nlng", time.Now().UTC())
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	var lngLine AssessmentLine
	for _, line := range assessment.Lines {
		if line.Instrument == InstrumentNimasaLNGDue {
			lngLine = line
		}
	}
	if lngLine.Applicability != LineExempt || lngLine.ExemptionID != "exemption-nlng-lng-due" || lngLine.AmountMinor != 0 {
		t.Fatalf("NLNG LNG due line = %+v, want EXEMPT", lngLine)
	}
	// The exemption would otherwise have been 25000 x 30 = 750000 minor.
	if assessment.TotalUSDMinor != 3675000+1000000 {
		t.Fatalf("NLNG total = %d, want ship dues + SPL only", assessment.TotalUSDMinor)
	}
	// Exemption audit: exactly one row with who/what/why/statutory basis.
	audits, err := store.ListExemptionAudits(ctx, assessment.AssessmentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 {
		t.Fatalf("audits = %+v", audits)
	}
	audit := audits[0]
	if audit.ExemptionID != "exemption-nlng-lng-due" || audit.MatchKind != MatchEntity || audit.MatchValue != "NLNG" ||
		audit.Requester != "officer:assessor" || audit.StatutoryBasis == "" || audit.EvidenceRequirement == "" {
		t.Fatalf("audit = %+v", audit)
	}
	// And the outbox carried the same event.
	var events int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tariff_outbox WHERE event_type = 'tariff.exemption.applied' AND subject_id = $1`,
		audit.AuditID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("exemption outbox events = %d, want 1", events)
	}
	// A non-NLNG LNG carrier on the same voyage pays the due in full.
	request.EntityRef = "BONNY-GAS-TRANSPORT"
	charged, err := store.Assess(ctx, request, "idem-nlng-2", "officer:assessor", "corr-nlng-2", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range charged.Lines {
		if line.Instrument == InstrumentNimasaLNGDue {
			if line.Applicability != LineCharged || line.AmountMinor != 750000 {
				t.Fatalf("non-NLNG LNG due = %+v, want charged 750000", line)
			}
		}
	}
}

// TestAssessmentReplayIdempotency verifies replay returns the stored,
// immutable assessment and conflicting reuse of a key is refused.
func TestAssessmentReplayIdempotency(t *testing.T) {
	store, pool := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	first, err := store.Assess(ctx, seededAssessRequest(), "idem-replay", "officer:a", "corr-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Assess(ctx, seededAssessRequest(), "idem-replay", "officer:a", "corr-2", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if replayed.AssessmentID != first.AssessmentID {
		t.Fatalf("replay minted a new assessment: %s != %s", replayed.AssessmentID, first.AssessmentID)
	}
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tariff_assessments WHERE idempotency_key = 'idem-replay'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("assessments for key = %d, want 1", count)
	}
	// Same key, different declaration: conflict, fail closed.
	different := seededAssessRequest()
	different.VesselGRT = 30000
	if _, err := store.Assess(ctx, different, "idem-replay", "officer:a", "corr-3", time.Now().UTC()); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
	// The immutable record is retrievable by id.
	stored, err := store.GetAssessment(ctx, first.AssessmentID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TotalUSDMinor != first.TotalUSDMinor || len(stored.Lines) != len(first.Lines) {
		t.Fatalf("stored assessment = %+v", stored)
	}
}

// TestMakerCheckerSQLRefusal verifies decided_by <> requested_by is enforced
// in SQL for both rates and exemptions.
func TestMakerCheckerSQLRefusal(t *testing.T) {
	store, pool := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	rate := RateRow{
		RateID: "rate-test-new", Instrument: InstrumentNIWAInlandCharge, Agency: AgencyNIWA,
		BandLogic: BandPerGRT, Currency: "NGN", RateMinorPerUnit: 150,
		StatutoryReference: "NIWA s.28 schedule (test)", EffectiveFrom: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := store.CreateRate(ctx, rate, "officer:maker"); err != nil {
		t.Fatalf("create rate: %v", err)
	}
	// Self-approval refused at SQL level.
	if _, err := store.ActivateRate(ctx, "rate-test-new", "officer:maker"); !errors.Is(err, ErrMakerChecker) {
		t.Fatalf("self-approval must fail, got %v", err)
	}
	// The row is untouched (still DRAFT, no checker).
	var state string
	var checker *string
	if err := pool.QueryRow(ctx,
		`SELECT state, checker FROM tariff_rates WHERE rate_id = 'rate-test-new'`).Scan(&state, &checker); err != nil {
		t.Fatal(err)
	}
	if state != "DRAFT" || checker != nil {
		t.Fatalf("rate after refused activation = %s/%v", state, checker)
	}
	// Distinct checker activates.
	activated, err := store.ActivateRate(ctx, "rate-test-new", "officer:checker")
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if activated.State != "ACTIVE" || activated.Checker != "officer:checker" {
		t.Fatalf("activated rate = %+v", activated)
	}
	// An already-ACTIVE row cannot be re-activated.
	if _, err := store.ActivateRate(ctx, "rate-test-new", "officer:other"); err == nil {
		t.Fatal("re-activation must fail")
	}
	// Overlap guard: a second row for the same instrument + band floor with
	// an overlapping window is refused (even as DRAFT).
	overlapping := rate
	overlapping.RateID = "rate-test-overlap"
	if _, err := store.CreateRate(ctx, overlapping, "officer:maker"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("overlapping window must fail, got %v", err)
	}
	// Exemption maker/checker follows the same rule.
	exemption := ExemptionRow{
		ExemptionID: "exemption-test", Instrument: InstrumentCabotageSurcharge, MatchKind: MatchCabotageTrade,
		MatchValue: "CABOTAGE_TRADE", StatutoryBasis: "test basis", EvidenceRequirement: "test evidence",
		EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := store.CreateExemption(ctx, exemption, "officer:maker"); err != nil {
		t.Fatalf("create exemption: %v", err)
	}
	if _, err := store.ActivateExemption(ctx, "exemption-test", "officer:maker"); !errors.Is(err, ErrMakerChecker) {
		t.Fatalf("exemption self-approval must fail, got %v", err)
	}
	activatedExemption, err := store.ActivateExemption(ctx, "exemption-test", "officer:checker")
	if err != nil {
		t.Fatalf("activate exemption: %v", err)
	}
	if activatedExemption.State != "ACTIVE" {
		t.Fatalf("activated exemption = %+v", activatedExemption)
	}
}

// TestNewRateAndExemptionDriveAssessments proves rate/exemption admin is
// live configuration, not decoration: an activated rule changes the next
// assessment deterministically.
func TestNewRateAndExemptionDriveAssessments(t *testing.T) {
	store, pool := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	// Maker/checker a cabotage-trade exemption for the cabotage surcharge.
	exemption := ExemptionRow{
		ExemptionID: "exemption-cabotage-test", Instrument: InstrumentCabotageSurcharge,
		MatchKind: MatchCabotageTrade, MatchValue: "CABOTAGE_TRADE",
		StatutoryBasis:      "Cabotage Act implementation regulation (test)",
		EvidenceRequirement: "coastal trade license",
		EffectiveFrom:       time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := store.CreateExemption(ctx, exemption, "officer:maker"); err != nil {
		t.Fatal(err)
	}
	request := seededAssessRequest()
	request.VoyageType = VoyageCabotage
	// Before activation (DRAFT): the surcharge is charged.
	before, err := store.Assess(ctx, request, "idem-cab-1", "officer:a", "corr-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range before.Lines {
		if line.Instrument == InstrumentCabotageSurcharge {
			if line.Applicability != LineCharged || line.AmountMinor != 10000000 {
				t.Fatalf("pre-activation cabotage = %+v, want charged 10000000", line)
			}
		}
	}
	if _, err := store.ActivateExemption(ctx, "exemption-cabotage-test", "officer:checker"); err != nil {
		t.Fatal(err)
	}
	// After activation: exempt, audited, totals drop.
	after, err := store.Assess(ctx, request, "idem-cab-2", "officer:a", "corr-2", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range after.Lines {
		if line.Instrument == InstrumentCabotageSurcharge {
			if line.Applicability != LineExempt || line.ExemptionID != "exemption-cabotage-test" {
				t.Fatalf("post-activation cabotage = %+v, want EXEMPT", line)
			}
		}
	}
	if after.TotalUSDMinor != before.TotalUSDMinor-10000000 {
		t.Fatalf("totals before/after = %d/%d", before.TotalUSDMinor, after.TotalUSDMinor)
	}
}

// TestHistoricalAsOfReproducesWindows pins the effective-dated behavior:
// the same declaration assessed at different statutory windows prices
// differently, reproducibly.
func TestHistoricalAsOfReproducesWindows(t *testing.T) {
	store, pool := openStore(t)
	defer pool.Close()
	ctx := context.Background()

	request := seededAssessRequest()
	request.AsOf = "2010-06-01"
	assessment, err := store.Assess(ctx, request, "idem-hist-1", "officer:a", "corr-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	byInstrument := map[string]AssessmentLine{}
	for _, line := range assessment.Lines {
		byInstrument[line.Instrument] = line
	}
	// 2010: s.15 3% is live (3% of US$5m = US$150,000), SPL 2012 not yet.
	if line := byInstrument[InstrumentNPALevyAct]; line.Applicability != LineCharged || line.AmountMinor != 15000000 {
		t.Fatalf("2010 s.15 = %+v, want charged 15000000", line)
	}
	if line := byInstrument[InstrumentSPL2012]; line.Applicability != LineUnrated {
		t.Fatalf("2010 SPL = %+v, want UNRATED", line)
	}
	if assessment.TotalUSDMinor != 3675000+15000000 {
		t.Fatalf("2010 total = %d", assessment.TotalUSDMinor)
	}
}
