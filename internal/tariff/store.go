package tariff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the PostgreSQL persistence boundary. Assessment creation is one
// atomic transaction: assessment + line items + exemption audit + outbox
// events. Admin mutations enforce maker/checker at SQL level
// (decided_by <> requested_by), mirroring the financial-intents doctrine.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore fails closed on a nil pool.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("postgres pool is required")
	}
	return &Store{pool: pool}, nil
}

// canonicalRequest renders the request deterministically for the replay hash.
func canonicalRequest(request AssessRequest) ([]byte, string, error) {
	canonical := struct {
		VesselGRT            int64    `json:"vesselGrt"`
		VesselClass          string   `json:"vesselClass"`
		EntityRef            string   `json:"entityRef"`
		CargoCategory        string   `json:"cargoCategory"`
		VoyageType           string   `json:"voyageType"`
		RouteKind            string   `json:"routeKind"`
		NigeriaPortCall      bool     `json:"nigeriaPortCall"`
		GrossFreightUSDMinor int64    `json:"grossFreightUsdMinor"`
		VoyageFlags          []string `json:"voyageFlags"`
		AsOf                 string   `json:"asOf"`
	}{request.VesselGRT, request.VesselClass, request.EntityRef, request.CargoCategory,
		request.VoyageType, request.RouteKind, request.NigeriaPortCall,
		request.GrossFreightUSDMinor, request.VoyageFlags, request.AsOf}
	if canonical.VoyageFlags == nil {
		canonical.VoyageFlags = []string{}
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return nil, "", fmt.Errorf("encode canonical request: %w", err)
	}
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), nil
}

// Assess runs the deterministic computation over the current ACTIVE rows and
// persists the immutable assessment atomically with its audit trail and
// outbox events. Replay of the same idempotency key returns the stored
// assessment; the same key against a different request conflicts.
func (store *Store) Assess(ctx context.Context, request AssessRequest, idempotencyKey, requester, correlationID string, now time.Time) (Assessment, error) {
	if err := request.Validate(); err != nil {
		return Assessment{}, err
	}
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return Assessment{}, errors.New("idempotency key is required")
	}
	if requester == "" || correlationID == "" {
		return Assessment{}, errors.New("requester and correlation id are required")
	}
	asOf := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if request.AsOf != "" {
		parsed, err := time.Parse("2006-01-02", request.AsOf)
		if err != nil {
			return Assessment{}, errors.New("asOf must be YYYY-MM-DD")
		}
		asOf = parsed.UTC()
		request.AsOf = asOf.Format("2006-01-02")
	} else {
		request.AsOf = asOf.Format("2006-01-02")
	}
	rawRequest, requestHash, err := canonicalRequest(request)
	if err != nil {
		return Assessment{}, err
	}
	// Idempotent replay resolves BEFORE any computation is persisted.
	if existing, err := store.lookupByIdempotency(ctx, idempotencyKey); err == nil {
		if existing.hash != requestHash {
			return Assessment{}, ErrIdempotencyConflict
		}
		return store.GetAssessment(ctx, existing.assessmentID)
	} else if !errors.Is(err, ErrNotFound) {
		return Assessment{}, err
	}
	rates, err := store.listActiveRates(ctx, asOf)
	if err != nil {
		return Assessment{}, err
	}
	exemptions, err := store.listActiveExemptions(ctx, asOf)
	if err != nil {
		return Assessment{}, err
	}
	computation := Compute(request, rates, exemptions, asOf)
	usdTotal, ngnTotal := computation.Totals()
	assessment := Assessment{
		AssessmentID:  uuid.NewString(),
		Request:       request,
		AsOf:          asOf.Format("2006-01-02"),
		Lines:         computation.Lines,
		TotalUSDMinor: usdTotal,
		TotalNGNMinor: ngnTotal,
		Requester:     requester,
		CorrelationID: correlationID,
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Assessment{}, fmt.Errorf("begin assessment transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tariff_assessments (assessment_id, idempotency_key, request_hash, request, as_of,
		     total_usd_minor, total_ngn_minor, requester, correlation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		assessment.AssessmentID, idempotencyKey, requestHash, rawRequest, asOf,
		usdTotal, ngnTotal, requester, correlationID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Concurrent twin won the key: replay its stored assessment.
			_ = tx.Rollback(ctx)
			if existing, lookupErr := store.lookupByIdempotency(ctx, idempotencyKey); lookupErr == nil {
				if existing.hash != requestHash {
					return Assessment{}, ErrIdempotencyConflict
				}
				return store.GetAssessment(ctx, existing.assessmentID)
			}
			return Assessment{}, ErrIdempotencyConflict
		}
		return Assessment{}, fmt.Errorf("insert assessment: %w", err)
	}
	for _, line := range computation.Lines {
		var exemptionID *string
		if line.ExemptionID != "" {
			exemptionID = &line.ExemptionID
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO tariff_assessment_lines (assessment_id, line_no, instrument, agency, applicability,
			     basis, statutory_reference, rate_description, amount_minor, currency, exemption_id, provisional)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			assessment.AssessmentID, line.LineNo, line.Instrument, line.Agency, line.Applicability,
			line.Basis, line.StatutoryReference, line.RateDescription, line.AmountMinor, line.Currency,
			exemptionID, line.Provisional); err != nil {
			return Assessment{}, fmt.Errorf("insert assessment line: %w", err)
		}
	}
	if err := insertOutboxTx(ctx, tx, assessment.AssessmentID, "tariff.assessment.created", map[string]any{
		"assessment_id":   assessment.AssessmentID,
		"as_of":           assessment.AsOf,
		"total_usd_minor": usdTotal,
		"total_ngn_minor": ngnTotal,
		"requester":       requester,
		"correlation_id":  correlationID,
	}); err != nil {
		return Assessment{}, err
	}
	// One audit row + one outbox event per applied exemption: who/what/why/
	// statutory basis, forever (exemptions are where the money leaked).
	for _, exemption := range computation.Exemptions {
		auditID := uuid.NewString()
		if _, err := tx.Exec(ctx,
			`INSERT INTO tariff_exemption_audit (audit_id, assessment_id, exemption_id, instrument, match_kind,
			     match_value, statutory_basis, evidence_requirement, requester)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			auditID, assessment.AssessmentID, exemption.ExemptionID, exemption.Instrument, exemption.MatchKind,
			exemption.MatchValue, exemption.StatutoryBasis, exemption.EvidenceRequirement, requester); err != nil {
			return Assessment{}, fmt.Errorf("insert exemption audit: %w", err)
		}
		if err := insertOutboxTx(ctx, tx, auditID, "tariff.exemption.applied", map[string]any{
			"audit_id":             auditID,
			"assessment_id":        assessment.AssessmentID,
			"exemption_id":         exemption.ExemptionID,
			"instrument":           exemption.Instrument,
			"match_kind":           exemption.MatchKind,
			"match_value":          exemption.MatchValue,
			"statutory_basis":      exemption.StatutoryBasis,
			"evidence_requirement": exemption.EvidenceRequirement,
			"requester":            requester,
			"correlation_id":       correlationID,
		}); err != nil {
			return Assessment{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Assessment{}, fmt.Errorf("commit assessment: %w", err)
	}
	return store.GetAssessment(ctx, assessment.AssessmentID)
}

// insertOutboxTx appends one outbox event inside the domain transaction.
func insertOutboxTx(ctx context.Context, tx pgx.Tx, subjectID, eventType string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbox payload: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tariff_outbox (event_id, subject_id, event_type, payload, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		uuid.New(), subjectID, eventType, raw, time.Now().UTC()); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// idemLookup is the replay-resolution record.
type idemLookup struct {
	assessmentID string
	hash         string
}

func (store *Store) lookupByIdempotency(ctx context.Context, key string) (idemLookup, error) {
	var lookup idemLookup
	err := store.pool.QueryRow(ctx,
		`SELECT assessment_id, request_hash FROM tariff_assessments WHERE idempotency_key = $1`, key).
		Scan(&lookup.assessmentID, &lookup.hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return idemLookup{}, ErrNotFound
		}
		return idemLookup{}, fmt.Errorf("resolve idempotency key: %w", err)
	}
	return lookup, nil
}

// GetAssessment loads one immutable assessment with its lines.
func (store *Store) GetAssessment(ctx context.Context, assessmentID string) (Assessment, error) {
	var assessment Assessment
	var rawRequest []byte
	var asOf time.Time
	err := store.pool.QueryRow(ctx,
		`SELECT assessment_id, request, as_of, total_usd_minor, total_ngn_minor, requester, correlation_id, created_at
		 FROM tariff_assessments WHERE assessment_id = $1`, assessmentID).
		Scan(&assessment.AssessmentID, &rawRequest, &asOf, &assessment.TotalUSDMinor,
			&assessment.TotalNGNMinor, &assessment.Requester, &assessment.CorrelationID, &assessment.CreatedAt)
	assessment.AsOf = asOf.Format("2006-01-02")
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Assessment{}, ErrNotFound
		}
		return Assessment{}, fmt.Errorf("load assessment: %w", err)
	}
	if err := json.Unmarshal(rawRequest, &assessment.Request); err != nil {
		return Assessment{}, fmt.Errorf("decode stored request: %w", err)
	}
	rows, err := store.pool.Query(ctx,
		`SELECT line_no, instrument, agency, applicability, basis, statutory_reference, rate_description,
		        amount_minor, currency, exemption_id, provisional
		 FROM tariff_assessment_lines WHERE assessment_id = $1 ORDER BY line_no`, assessmentID)
	if err != nil {
		return Assessment{}, fmt.Errorf("load assessment lines: %w", err)
	}
	defer rows.Close()
	assessment.Lines = make([]AssessmentLine, 0)
	for rows.Next() {
		var line AssessmentLine
		var exemptionID *string
		if err := rows.Scan(&line.LineNo, &line.Instrument, &line.Agency, &line.Applicability, &line.Basis,
			&line.StatutoryReference, &line.RateDescription, &line.AmountMinor, &line.Currency,
			&exemptionID, &line.Provisional); err != nil {
			return Assessment{}, fmt.Errorf("scan assessment line: %w", err)
		}
		if exemptionID != nil {
			line.ExemptionID = *exemptionID
		}
		assessment.Lines = append(assessment.Lines, line)
	}
	return assessment, rows.Err()
}

// ListExemptionAudits returns the audit trail of one assessment.
func (store *Store) ListExemptionAudits(ctx context.Context, assessmentID string) ([]ExemptionAudit, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT audit_id, assessment_id, exemption_id, instrument, match_kind, match_value,
		        statutory_basis, evidence_requirement, requester, created_at
		 FROM tariff_exemption_audit WHERE assessment_id = $1 ORDER BY created_at`, assessmentID)
	if err != nil {
		return nil, fmt.Errorf("list exemption audits: %w", err)
	}
	defer rows.Close()
	audits := make([]ExemptionAudit, 0)
	for rows.Next() {
		var audit ExemptionAudit
		if err := rows.Scan(&audit.AuditID, &audit.AssessmentID, &audit.ExemptionID, &audit.Instrument,
			&audit.MatchKind, &audit.MatchValue, &audit.StatutoryBasis, &audit.EvidenceRequirement,
			&audit.Requester, &audit.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan exemption audit: %w", err)
		}
		audits = append(audits, audit)
	}
	return audits, rows.Err()
}

// ---------------------------------------------------------------------------
// Rate/exemption row loading for the computation window.
// ---------------------------------------------------------------------------

func (store *Store) listActiveRates(ctx context.Context, asOf time.Time) ([]RateRow, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT rate_id, instrument, agency, band_logic, currency, rate_minor_per_unit, rate_bps,
		        band_floor, band_ceiling, statutory_reference, provisional, effective_from, effective_to,
		        state, maker, checker
		 FROM tariff_rates WHERE state = 'ACTIVE' AND effective_from <= $1
		   AND (effective_to IS NULL OR effective_to >= $1)
		 ORDER BY instrument, band_floor`, asOf)
	if err != nil {
		return nil, fmt.Errorf("list active rates: %w", err)
	}
	defer rows.Close()
	rates := make([]RateRow, 0)
	for rows.Next() {
		var rate RateRow
		var minorPerUnit, bps *int64
		var checker *string
		if err := rows.Scan(&rate.RateID, &rate.Instrument, &rate.Agency, &rate.BandLogic, &rate.Currency,
			&minorPerUnit, &bps, &rate.BandFloor, &rate.BandCeiling, &rate.StatutoryReference,
			&rate.Provisional, &rate.EffectiveFrom, &rate.EffectiveTo, &rate.State, &rate.Maker, &checker); err != nil {
			return nil, fmt.Errorf("scan rate: %w", err)
		}
		if minorPerUnit != nil {
			rate.RateMinorPerUnit = *minorPerUnit
		}
		if bps != nil {
			rate.RateBps = *bps
		}
		if checker != nil {
			rate.Checker = *checker
		}
		rates = append(rates, rate)
	}
	return rates, rows.Err()
}

func (store *Store) listActiveExemptions(ctx context.Context, asOf time.Time) ([]ExemptionRow, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT exemption_id, instrument, match_kind, match_value, statutory_basis, evidence_requirement,
		        effective_from, effective_to, state, maker, checker
		 FROM tariff_exemptions WHERE state = 'ACTIVE' AND effective_from <= $1
		   AND (effective_to IS NULL OR effective_to >= $1)
		 ORDER BY exemption_id`, asOf)
	if err != nil {
		return nil, fmt.Errorf("list active exemptions: %w", err)
	}
	defer rows.Close()
	exemptions := make([]ExemptionRow, 0)
	for rows.Next() {
		var exemption ExemptionRow
		var checker *string
		if err := rows.Scan(&exemption.ExemptionID, &exemption.Instrument, &exemption.MatchKind,
			&exemption.MatchValue, &exemption.StatutoryBasis, &exemption.EvidenceRequirement,
			&exemption.EffectiveFrom, &exemption.EffectiveTo, &exemption.State, &exemption.Maker, &checker); err != nil {
			return nil, fmt.Errorf("scan exemption: %w", err)
		}
		if checker != nil {
			exemption.Checker = *checker
		}
		exemptions = append(exemptions, exemption)
	}
	return exemptions, rows.Err()
}

// ---------------------------------------------------------------------------
// Admin: maker/checker with SQL-level decided_by <> requested_by.
// ---------------------------------------------------------------------------

// CreateRate inserts one DRAFT rate (maker), refusing window overlap against
// existing non-RETIRED rows of the same instrument and band floor.
func (store *Store) CreateRate(ctx context.Context, rate RateRow, maker string) (RateRow, error) {
	if maker == "" || rate.RateID == "" || rate.StatutoryReference == "" {
		return RateRow{}, errors.New("rate id, statutory reference and maker are required")
	}
	if rate.EffectiveFrom.IsZero() {
		return RateRow{}, errors.New("effective_from is required")
	}
	if rate.BandLogic != BandPerGRT && rate.BandLogic != BandGrossFreight {
		return RateRow{}, errors.New("band logic must be PER_GRT_BAND or PERCENT_GROSS_FREIGHT")
	}
	if rate.BandLogic == BandPerGRT && rate.RateMinorPerUnit <= 0 {
		return RateRow{}, errors.New("per-GRT rates require a positive minor-per-unit")
	}
	if rate.BandLogic == BandGrossFreight && rate.RateBps <= 0 {
		return RateRow{}, errors.New("freight rates require positive basis points")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return RateRow{}, fmt.Errorf("begin rate transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var conflict bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM tariff_rates
		     WHERE instrument = $1 AND band_floor = $2 AND state <> 'RETIRED'
		       AND effective_from <= COALESCE($3, DATE '9999-12-31')
		       AND (effective_to IS NULL OR effective_to >= $4))`,
		rate.Instrument, rate.BandFloor, rate.EffectiveTo, rate.EffectiveFrom).Scan(&conflict); err != nil {
		return RateRow{}, fmt.Errorf("check rate window overlap: %w", err)
	}
	if conflict {
		return RateRow{}, fmt.Errorf("rate window overlaps an existing %s band-floor-%d row: %w",
			rate.Instrument, rate.BandFloor, ErrInvalidTransition)
	}
	var minorPerUnit, bps *int64
	if rate.BandLogic == BandPerGRT {
		minorPerUnit = &rate.RateMinorPerUnit
	} else {
		bps = &rate.RateBps
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tariff_rates (rate_id, instrument, agency, band_logic, currency, rate_minor_per_unit,
		     rate_bps, band_floor, band_ceiling, statutory_reference, provisional, effective_from,
		     effective_to, state, maker)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'DRAFT', $14)`,
		rate.RateID, rate.Instrument, rate.Agency, rate.BandLogic, rate.Currency, minorPerUnit,
		bps, rate.BandFloor, rate.BandCeiling, rate.StatutoryReference, rate.Provisional,
		rate.EffectiveFrom, rate.EffectiveTo, maker); err != nil {
		return RateRow{}, fmt.Errorf("insert rate: %w", err)
	}
	if err := insertOutboxTx(ctx, tx, rate.RateID, "tariff.rate.created", map[string]any{
		"rate_id": rate.RateID, "instrument": rate.Instrument, "maker": maker,
	}); err != nil {
		return RateRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RateRow{}, fmt.Errorf("commit rate: %w", err)
	}
	rate.State = "DRAFT"
	rate.Maker = maker
	return rate, nil
}

// ActivateRate approves one DRAFT rate with a checker DISTINCT from the
// maker, enforced in SQL: the UPDATE refuses maker == checker.
func (store *Store) ActivateRate(ctx context.Context, rateID, checker string) (RateRow, error) {
	if checker == "" {
		return RateRow{}, errors.New("checker is required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return RateRow{}, fmt.Errorf("begin rate activation: %w", err)
	}
	defer tx.Rollback(ctx)
	var maker string
	err = tx.QueryRow(ctx,
		`UPDATE tariff_rates SET state = 'ACTIVE', checker = $2
		 WHERE rate_id = $1 AND state = 'DRAFT' AND maker <> $2
		 RETURNING maker`, rateID, checker).Scan(&maker)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RateRow{}, fmt.Errorf("rate %s not DRAFT or maker equals checker: %w", rateID, ErrMakerChecker)
		}
		return RateRow{}, fmt.Errorf("activate rate: %w", err)
	}
	if err := insertOutboxTx(ctx, tx, rateID, "tariff.rate.activated", map[string]any{
		"rate_id": rateID, "maker": maker, "checker": checker,
	}); err != nil {
		return RateRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RateRow{}, fmt.Errorf("commit rate activation: %w", err)
	}
	return store.getRate(ctx, rateID)
}

// GetRate loads one rate row.
func (store *Store) getRate(ctx context.Context, rateID string) (RateRow, error) {
	rates, err := store.listRatesWhere(ctx, `rate_id = $1`, rateID)
	if err != nil {
		return RateRow{}, err
	}
	if len(rates) == 0 {
		return RateRow{}, ErrNotFound
	}
	return rates[0], nil
}

// ListRates returns every rate row (admin view).
func (store *Store) ListRates(ctx context.Context) ([]RateRow, error) {
	return store.listRatesWhere(ctx, `TRUE`)
}

func (store *Store) listRatesWhere(ctx context.Context, where string, args ...any) ([]RateRow, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT rate_id, instrument, agency, band_logic, currency, rate_minor_per_unit, rate_bps,
		        band_floor, band_ceiling, statutory_reference, provisional, effective_from, effective_to,
		        state, maker, checker
		 FROM tariff_rates WHERE `+where+` ORDER BY instrument, band_floor, effective_from`, args...)
	if err != nil {
		return nil, fmt.Errorf("list rates: %w", err)
	}
	defer rows.Close()
	rates := make([]RateRow, 0)
	for rows.Next() {
		var rate RateRow
		var minorPerUnit, bps *int64
		var checker *string
		if err := rows.Scan(&rate.RateID, &rate.Instrument, &rate.Agency, &rate.BandLogic, &rate.Currency,
			&minorPerUnit, &bps, &rate.BandFloor, &rate.BandCeiling, &rate.StatutoryReference,
			&rate.Provisional, &rate.EffectiveFrom, &rate.EffectiveTo, &rate.State, &rate.Maker, &checker); err != nil {
			return nil, fmt.Errorf("scan rate: %w", err)
		}
		if minorPerUnit != nil {
			rate.RateMinorPerUnit = *minorPerUnit
		}
		if bps != nil {
			rate.RateBps = *bps
		}
		if checker != nil {
			rate.Checker = *checker
		}
		rates = append(rates, rate)
	}
	return rates, rows.Err()
}

// CreateExemption inserts one DRAFT exemption rule (maker).
func (store *Store) CreateExemption(ctx context.Context, exemption ExemptionRow, maker string) (ExemptionRow, error) {
	if maker == "" || exemption.ExemptionID == "" || exemption.StatutoryBasis == "" || exemption.EvidenceRequirement == "" {
		return ExemptionRow{}, errors.New("exemption id, statutory basis, evidence requirement and maker are required")
	}
	switch exemption.MatchKind {
	case MatchEntity, MatchCargoCategory, MatchVoyageFlag, MatchCabotageTrade:
	default:
		return ExemptionRow{}, fmt.Errorf("match kind %q is not supported", exemption.MatchKind)
	}
	if exemption.MatchValue == "" || exemption.Instrument == "" {
		return ExemptionRow{}, errors.New("instrument and match value are required")
	}
	if exemption.EffectiveFrom.IsZero() {
		return ExemptionRow{}, errors.New("effective_from is required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ExemptionRow{}, fmt.Errorf("begin exemption transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO tariff_exemptions (exemption_id, instrument, match_kind, match_value, statutory_basis,
		     evidence_requirement, effective_from, effective_to, state, maker)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'DRAFT', $9)`,
		exemption.ExemptionID, exemption.Instrument, exemption.MatchKind, exemption.MatchValue,
		exemption.StatutoryBasis, exemption.EvidenceRequirement, exemption.EffectiveFrom,
		exemption.EffectiveTo, maker); err != nil {
		return ExemptionRow{}, fmt.Errorf("insert exemption: %w", err)
	}
	if err := insertOutboxTx(ctx, tx, exemption.ExemptionID, "tariff.exemption.created", map[string]any{
		"exemption_id": exemption.ExemptionID, "instrument": exemption.Instrument, "maker": maker,
	}); err != nil {
		return ExemptionRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ExemptionRow{}, fmt.Errorf("commit exemption: %w", err)
	}
	exemption.State = "DRAFT"
	exemption.Maker = maker
	return exemption, nil
}

// ActivateExemption approves one DRAFT exemption with SQL-enforced
// maker/checker separation.
func (store *Store) ActivateExemption(ctx context.Context, exemptionID, checker string) (ExemptionRow, error) {
	if checker == "" {
		return ExemptionRow{}, errors.New("checker is required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ExemptionRow{}, fmt.Errorf("begin exemption activation: %w", err)
	}
	defer tx.Rollback(ctx)
	var maker string
	err = tx.QueryRow(ctx,
		`UPDATE tariff_exemptions SET state = 'ACTIVE', checker = $2
		 WHERE exemption_id = $1 AND state = 'DRAFT' AND maker <> $2
		 RETURNING maker`, exemptionID, checker).Scan(&maker)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExemptionRow{}, fmt.Errorf("exemption %s not DRAFT or maker equals checker: %w", exemptionID, ErrMakerChecker)
		}
		return ExemptionRow{}, fmt.Errorf("activate exemption: %w", err)
	}
	if err := insertOutboxTx(ctx, tx, exemptionID, "tariff.exemption.activated", map[string]any{
		"exemption_id": exemptionID, "maker": maker, "checker": checker,
	}); err != nil {
		return ExemptionRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ExemptionRow{}, fmt.Errorf("commit exemption activation: %w", err)
	}
	return store.getExemption(ctx, exemptionID)
}

func (store *Store) getExemption(ctx context.Context, exemptionID string) (ExemptionRow, error) {
	var exemption ExemptionRow
	var checker *string
	err := store.pool.QueryRow(ctx,
		`SELECT exemption_id, instrument, match_kind, match_value, statutory_basis, evidence_requirement,
		        effective_from, effective_to, state, maker, checker
		 FROM tariff_exemptions WHERE exemption_id = $1`, exemptionID).
		Scan(&exemption.ExemptionID, &exemption.Instrument, &exemption.MatchKind, &exemption.MatchValue,
			&exemption.StatutoryBasis, &exemption.EvidenceRequirement, &exemption.EffectiveFrom,
			&exemption.EffectiveTo, &exemption.State, &exemption.Maker, &checker)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ExemptionRow{}, ErrNotFound
		}
		return ExemptionRow{}, fmt.Errorf("load exemption: %w", err)
	}
	if checker != nil {
		exemption.Checker = *checker
	}
	return exemption, nil
}
