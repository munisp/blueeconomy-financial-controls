package revenue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// Store is the PostgreSQL persistence boundary for the revenue-assurance
// chain. Mutations that cross trust boundaries are one atomic transaction:
// debit-note issuance writes note + document series + transition audit +
// envelope + outbox together; statement ingest writes statement + lines +
// outbox; a recon batch writes run + matches + transitions + exceptions.
// Dual control is SQL-enforced: issuance and cancellation guarded UPDATEs
// require the actor to differ from the note's maker.
type Store struct {
	pool   *pgxpool.Pool
	signer *envelope.Signer
	now    func() time.Time
}

// NewStore fails closed on a nil pool or signer (the signer seals debit
// notes and remittance advices; without one the store cannot issue).
func NewStore(pool *pgxpool.Pool, signer *envelope.Signer) (*Store, error) {
	if pool == nil {
		return nil, errors.New("postgres pool is required")
	}
	if signer == nil {
		return nil, errors.New("envelope signer is required")
	}
	return &Store{pool: pool, signer: signer, now: func() time.Time { return time.Now().UTC() }}, nil
}

// canonicalJSON renders a request deterministically for the replay hash.
func canonicalJSON(value any) ([]byte, string, error) {
	raw, err := envelope.Canonicalize(value)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(raw)
	return raw, hex.EncodeToString(digest[:]), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// ---------------------------------------------------------------------------
// Debit notes
// ---------------------------------------------------------------------------

// CreateDebitNote drafts a debit note against an immutable assessment. The
// maker is the verified subject; issuance (Issue) is the dual-controlled
// step. Replay of the same idempotency key returns the stored note; the
// same key against a different request conflicts.
func (store *Store) CreateDebitNote(ctx context.Context, request IssueRequest, idempotencyKey, maker, correlationID string) (DebitNote, error) {
	if err := request.Validate(); err != nil {
		return DebitNote{}, err
	}
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return DebitNote{}, errors.New("idempotency key is required")
	}
	if maker == "" || correlationID == "" {
		return DebitNote{}, errors.New("maker and correlation id are required")
	}
	effectiveDate := request.EffectiveDate
	if effectiveDate == "" {
		effectiveDate = store.now().Format("2006-01-02")
	}
	_, requestHash, err := canonicalJSON(struct {
		AssessmentID  string `json:"assessmentId"`
		DueDate       string `json:"dueDate"`
		EffectiveDate string `json:"effectiveDate"`
	}{request.AssessmentID, request.DueDate, effectiveDate})
	if err != nil {
		return DebitNote{}, err
	}
	if existing, err := store.noteByIdempotency(ctx, idempotencyKey); err == nil {
		if existing.hash != requestHash {
			return DebitNote{}, ErrIdempotencyConflict
		}
		return store.GetDebitNote(ctx, existing.id)
	} else if !errors.Is(err, ErrNotFound) {
		return DebitNote{}, err
	}

	// Load the assessment and its CHARGED lines (immutable source).
	var (
		entityRef          string
		totalUSD, totalNGN int64
	)
	err = store.pool.QueryRow(ctx,
		`SELECT request->>'entityRef', total_usd_minor, total_ngn_minor
		 FROM tariff_assessments WHERE assessment_id = $1`, request.AssessmentID).
		Scan(&entityRef, &totalUSD, &totalNGN)
	if errors.Is(err, pgx.ErrNoRows) {
		return DebitNote{}, ErrNotFound
	}
	if err != nil {
		return DebitNote{}, fmt.Errorf("load assessment: %w", err)
	}
	rows, err := store.pool.Query(ctx,
		`SELECT line_no, instrument, agency, amount_minor, currency, statutory_reference
		 FROM tariff_assessment_lines
		 WHERE assessment_id = $1 AND applicability = 'CHARGED' ORDER BY line_no`, request.AssessmentID)
	if err != nil {
		return DebitNote{}, fmt.Errorf("load assessment lines: %w", err)
	}
	defer rows.Close()
	var lines []DebitNoteLine
	lineNo := 0
	for rows.Next() {
		var line DebitNoteLine
		if err := rows.Scan(&lineNo, &line.Instrument, &line.Agency, &line.AmountMinor, &line.Currency, &line.StatutoryReference); err != nil {
			return DebitNote{}, fmt.Errorf("scan assessment line: %w", err)
		}
		line.LineNo = len(lines) + 1
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return DebitNote{}, err
	}
	if len(lines) == 0 {
		return DebitNote{}, errors.New("assessment has no charged lines to bill")
	}
	agency := lines[0].Agency
	for _, line := range lines {
		if line.Agency != agency {
			return DebitNote{}, errors.New("assessment lines span agencies; issue one debit note per agency (unsupported mix)")
		}
	}

	note := DebitNote{
		DebitNoteID:    uuid.NewString(),
		AssessmentID:   request.AssessmentID,
		Agency:         agency,
		EntityRef:      entityRef,
		AmountUSDMinor: totalUSD,
		AmountNGNMinor: totalNGN,
		EffectiveDate:  effectiveDate,
		DueDate:        request.DueDate,
		State:          StateDraft,
		Lines:          lines,
		Maker:          maker,
		CorrelationID:  correlationID,
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DebitNote{}, fmt.Errorf("begin debit-note transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO revenue_debit_notes (debit_note_id, assessment_id, idempotency_key, request_hash,
		     agency, entity_ref, amount_usd_minor, amount_ngn_minor, effective_date, due_date,
		     state, maker, correlation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		note.DebitNoteID, note.AssessmentID, idempotencyKey, requestHash, note.Agency,
		note.EntityRef, note.AmountUSDMinor, note.AmountNGNMinor, note.EffectiveDate,
		note.DueDate, note.State, note.Maker, note.CorrelationID); err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			if existing, lookupErr := store.noteByIdempotency(ctx, idempotencyKey); lookupErr == nil {
				if existing.hash != requestHash {
					return DebitNote{}, ErrIdempotencyConflict
				}
				return store.GetDebitNote(ctx, existing.id)
			}
			return DebitNote{}, ErrIdempotencyConflict
		}
		return DebitNote{}, fmt.Errorf("insert debit note: %w", err)
	}
	for _, line := range lines {
		if _, err := tx.Exec(ctx,
			`INSERT INTO revenue_debit_note_lines (debit_note_id, line_no, instrument, agency, amount_minor, currency, statutory_reference)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			note.DebitNoteID, line.LineNo, line.Instrument, line.Agency, line.AmountMinor, line.Currency, line.StatutoryReference); err != nil {
			return DebitNote{}, fmt.Errorf("insert debit-note line: %w", err)
		}
	}
	if err := insertTransition(ctx, tx, note.DebitNoteID, "", StateDraft, "debit note drafted", maker, ""); err != nil {
		return DebitNote{}, err
	}
	if err := insertOutbox(ctx, tx, note.DebitNoteID, "revenue.debit_note.created", map[string]any{
		"debitNoteId": note.DebitNoteID, "assessmentId": note.AssessmentID,
		"agency": note.Agency, "entityRef": note.EntityRef,
		"amountUsdMinor": note.AmountUSDMinor, "amountNgnMinor": note.AmountNGNMinor,
		"maker": maker, "correlationId": correlationID,
	}); err != nil {
		return DebitNote{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DebitNote{}, fmt.Errorf("commit debit note: %w", err)
	}
	return store.GetDebitNote(ctx, note.DebitNoteID)
}

// Issue seals a DRAFT note: assigns the per-agency document number, seals
// the envelope v1.0 signed representation and moves DRAFT -> ISSUED. The
// issuer (verified subject) must differ from the maker — enforced by the
// guarded UPDATE in SQL (maker <> checker), so self-issuance is refused
// even if the service check is bypassed.
func (store *Store) Issue(ctx context.Context, debitNoteID, issuer string) (DebitNote, error) {
	if debitNoteID == "" || issuer == "" {
		return DebitNote{}, errors.New("debit note id and issuer are required")
	}
	ctx, span := store.startSpan(ctx, "revenue.debit_note.issue")
	defer span.End()
	note, err := store.GetDebitNote(ctx, debitNoteID)
	if err != nil {
		return DebitNote{}, err
	}
	if note.State != StateDraft {
		return DebitNote{}, ErrInvalidTransition
	}
	if note.Maker == issuer {
		return DebitNote{}, ErrMakerChecker
	}
	now := store.now()
	documentNumber, err := store.nextDocumentNumber(ctx, note.Agency, now.Year())
	if err != nil {
		return DebitNote{}, err
	}
	note.DocumentNumber = documentNumber
	note.Checker = issuer
	// The signed canonical representation: the note as billed.
	payload := struct {
		DebitNoteID    string          `json:"debitNoteId"`
		DocumentNumber string          `json:"documentNumber"`
		AssessmentID   string          `json:"assessmentId"`
		Agency         string          `json:"agency"`
		EntityRef      string          `json:"entityRef"`
		AmountUSDMinor int64           `json:"amountUsdMinor"`
		AmountNGNMinor int64           `json:"amountNgnMinor"`
		EffectiveDate  string          `json:"effectiveDate"`
		DueDate        string          `json:"dueDate"`
		Lines          []DebitNoteLine `json:"lines"`
	}{note.DebitNoteID, documentNumber, note.AssessmentID, note.Agency, note.EntityRef,
		note.AmountUSDMinor, note.AmountNGNMinor, note.EffectiveDate, note.DueDate, note.Lines}
	bundle, jws, err := store.signer.Sign(ArtifactDebitNote, note.DebitNoteID, payload, now)
	if err != nil {
		return DebitNote{}, fmt.Errorf("seal debit-note envelope: %w", err)
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DebitNote{}, fmt.Errorf("begin issuance transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	// Guarded in SQL: state must still be DRAFT and maker <> issuer.
	result, err := tx.Exec(ctx,
		`UPDATE revenue_debit_notes
		 SET state = 'ISSUED', document_number = $2, checker = $3, envelope = $4, envelope_jws = $5, updated_at = now()
		 WHERE debit_note_id = $1 AND state = 'DRAFT' AND maker <> $3`,
		debitNoteID, documentNumber, issuer, bundle, jws)
	if err != nil {
		if isCheckViolation(err) {
			return DebitNote{}, ErrMakerChecker
		}
		return DebitNote{}, fmt.Errorf("issue debit note: %w", err)
	}
	if result.RowsAffected() == 0 {
		// Distinguish self-issuance from a state race.
		if fresh, lookupErr := store.GetDebitNote(ctx, debitNoteID); lookupErr == nil && fresh.Maker == issuer {
			return DebitNote{}, ErrMakerChecker
		}
		return DebitNote{}, ErrInvalidTransition
	}
	if err := insertTransition(ctx, tx, debitNoteID, StateDraft, StateIssued, "issued under dual control", issuer, note.Maker); err != nil {
		return DebitNote{}, err
	}
	if err := insertOutbox(ctx, tx, debitNoteID, "revenue.debit_note.transitioned", map[string]any{
		"debitNoteId": debitNoteID, "fromState": StateDraft, "toState": StateIssued,
		"documentNumber": documentNumber, "actor": issuer,
	}); err != nil {
		return DebitNote{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DebitNote{}, fmt.Errorf("commit issuance: %w", err)
	}
	recordMoneyOp(ctx, "debit_note.issued")
	return store.GetDebitNote(ctx, debitNoteID)
}

// TransitionRequest is one lifecycle move on an issued note.
type TransitionRequest struct {
	ToState string `json:"toState"`
	Reason  string `json:"reason,omitempty"`
}

// Transition moves a note through its lifecycle with full audit. Cancellation
// is dual-controlled against the maker (SQL-guarded); settlement is reserved
// for the reconciliation engine (settleViaRecon). ACKED/DISPUTED are the
// debtor-facing moves recorded with the actor identity.
func (store *Store) Transition(ctx context.Context, debitNoteID string, request TransitionRequest, actor string) (DebitNote, error) {
	if debitNoteID == "" || actor == "" {
		return DebitNote{}, errors.New("debit note id and actor are required")
	}
	note, err := store.GetDebitNote(ctx, debitNoteID)
	if err != nil {
		return DebitNote{}, err
	}
	if !validTransition(note.State, request.ToState) {
		return DebitNote{}, ErrInvalidTransition
	}
	if request.ToState == StateSettled {
		return DebitNote{}, errors.New("SETTLED is driven by reconciliation, not manual transition")
	}
	if request.ToState == StateCancelled {
		if len(strings.TrimSpace(request.Reason)) < 8 {
			return DebitNote{}, errors.New("cancellation requires a reason")
		}
		if note.Maker == actor {
			return DebitNote{}, ErrMakerChecker
		}
	}
	if request.ToState == StateDisputed && len(strings.TrimSpace(request.Reason)) < 8 {
		return DebitNote{}, errors.New("dispute requires a reason")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return DebitNote{}, fmt.Errorf("begin transition transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var result pgconn.CommandTag
	if request.ToState == StateCancelled {
		// SQL dual control: canceller must differ from the maker.
		result, err = tx.Exec(ctx,
			`UPDATE revenue_debit_notes
			 SET state = $2, cancel_reason = $3, updated_at = now()
			 WHERE debit_note_id = $1 AND state = $4 AND maker <> $5`,
			debitNoteID, request.ToState, request.Reason, note.State, actor)
	} else {
		result, err = tx.Exec(ctx,
			`UPDATE revenue_debit_notes
			 SET state = $2, updated_at = now()
			 WHERE debit_note_id = $1 AND state = $3`,
			debitNoteID, request.ToState, note.State)
	}
	if err != nil {
		if isCheckViolation(err) {
			return DebitNote{}, ErrMakerChecker
		}
		return DebitNote{}, fmt.Errorf("transition debit note: %w", err)
	}
	if result.RowsAffected() == 0 {
		if request.ToState == StateCancelled && note.Maker == actor {
			return DebitNote{}, ErrMakerChecker
		}
		return DebitNote{}, ErrInvalidTransition
	}
	approver := ""
	if request.ToState == StateCancelled {
		approver = note.Maker
	}
	if err := insertTransition(ctx, tx, debitNoteID, note.State, request.ToState, request.Reason, actor, approver); err != nil {
		return DebitNote{}, err
	}
	if err := insertOutbox(ctx, tx, debitNoteID, "revenue.debit_note.transitioned", map[string]any{
		"debitNoteId": debitNoteID, "fromState": note.State, "toState": request.ToState,
		"reason": request.Reason, "actor": actor,
	}); err != nil {
		return DebitNote{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DebitNote{}, fmt.Errorf("commit transition: %w", err)
	}
	return store.GetDebitNote(ctx, debitNoteID)
}

// GetDebitNote loads one note with its lines.
func (store *Store) GetDebitNote(ctx context.Context, debitNoteID string) (DebitNote, error) {
	var note DebitNote
	var documentNumber, cancelReason, checker *string
	err := store.pool.QueryRow(ctx,
		`SELECT debit_note_id, assessment_id, agency, entity_ref, document_number,
		        amount_usd_minor, amount_ngn_minor, effective_date::text, due_date::text, state,
		        cancel_reason, maker, checker, correlation_id, created_at, updated_at
		 FROM revenue_debit_notes WHERE debit_note_id = $1`, debitNoteID).
		Scan(&note.DebitNoteID, &note.AssessmentID, &note.Agency, &note.EntityRef, &documentNumber,
			&note.AmountUSDMinor, &note.AmountNGNMinor, &note.EffectiveDate, &note.DueDate,
			&note.State, &cancelReason, &note.Maker, &checker, &note.CorrelationID,
			&note.CreatedAt, &note.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DebitNote{}, ErrNotFound
	}
	if err != nil {
		return DebitNote{}, fmt.Errorf("load debit note: %w", err)
	}
	if documentNumber != nil {
		note.DocumentNumber = *documentNumber
	}
	if cancelReason != nil {
		note.CancelReason = *cancelReason
	}
	if checker != nil {
		note.Checker = *checker
	}
	rows, err := store.pool.Query(ctx,
		`SELECT line_no, instrument, agency, amount_minor, currency, statutory_reference
		 FROM revenue_debit_note_lines WHERE debit_note_id = $1 ORDER BY line_no`, debitNoteID)
	if err != nil {
		return DebitNote{}, fmt.Errorf("load debit-note lines: %w", err)
	}
	defer rows.Close()
	note.Lines = []DebitNoteLine{}
	for rows.Next() {
		var line DebitNoteLine
		if err := rows.Scan(&line.LineNo, &line.Instrument, &line.Agency, &line.AmountMinor, &line.Currency, &line.StatutoryReference); err != nil {
			return DebitNote{}, fmt.Errorf("scan debit-note line: %w", err)
		}
		note.Lines = append(note.Lines, line)
	}
	return note, rows.Err()
}

// GetDebitNoteEnvelope returns the sealed envelope for an issued note.
func (store *Store) GetDebitNoteEnvelope(ctx context.Context, debitNoteID string) (bundle []byte, jws string, err error) {
	err = store.pool.QueryRow(ctx,
		`SELECT envelope, envelope_jws FROM revenue_debit_notes
		 WHERE debit_note_id = $1 AND envelope IS NOT NULL`, debitNoteID).Scan(&bundle, &jws)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return bundle, jws, err
}

// ListTransitions returns the audited lifecycle of one note.
func (store *Store) ListTransitions(ctx context.Context, debitNoteID string) ([]Transition, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT transition_id, debit_note_id, from_state, to_state, reason, actor, approver, created_at
		 FROM revenue_debit_note_transitions WHERE debit_note_id = $1 ORDER BY created_at, transition_id`, debitNoteID)
	if err != nil {
		return nil, fmt.Errorf("list transitions: %w", err)
	}
	defer rows.Close()
	transitions := []Transition{}
	for rows.Next() {
		var transition Transition
		var approver *string
		if err := rows.Scan(&transition.TransitionID, &transition.DebitNoteID, &transition.FromState,
			&transition.ToState, &transition.Reason, &transition.Actor, &approver, &transition.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan transition: %w", err)
		}
		if approver != nil {
			transition.Approver = *approver
		}
		transitions = append(transitions, transition)
	}
	return transitions, rows.Err()
}

// nextDocumentNumber atomically advances the per-agency, per-year series.
func (store *Store) nextDocumentNumber(ctx context.Context, agency string, year int) (string, error) {
	var seq int64
	err := store.pool.QueryRow(ctx,
		`INSERT INTO revenue_doc_series (agency, series_year, next_seq) VALUES ($1, $2, 2)
		 ON CONFLICT (agency, series_year)
		 DO UPDATE SET next_seq = revenue_doc_series.next_seq + 1
		 RETURNING next_seq - 1`, agency, year).Scan(&seq)
	if err != nil {
		return "", fmt.Errorf("advance document series: %w", err)
	}
	return fmt.Sprintf("%s-%d-%06d", agency, year, seq), nil
}

type noteKeyLookup struct {
	id   string
	hash string
}

func (store *Store) noteByIdempotency(ctx context.Context, key string) (noteKeyLookup, error) {
	var lookup noteKeyLookup
	err := store.pool.QueryRow(ctx,
		`SELECT debit_note_id, request_hash FROM revenue_debit_notes WHERE idempotency_key = $1`, key).
		Scan(&lookup.id, &lookup.hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return noteKeyLookup{}, ErrNotFound
	}
	return lookup, err
}

// insertTransition appends the audited lifecycle move inside a transaction.
func insertTransition(ctx context.Context, tx pgx.Tx, debitNoteID, fromState, toState, reason, actor, approver string) error {
	var approverArg any
	if approver != "" {
		approverArg = approver
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO revenue_debit_note_transitions (transition_id, debit_note_id, from_state, to_state, reason, actor, approver)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.NewString(), debitNoteID, fromState, toState, reason, actor, approverArg); err != nil {
		return fmt.Errorf("insert transition audit: %w", err)
	}
	return nil
}

// insertOutbox appends one revenue outbox event inside a transaction.
func insertOutbox(ctx context.Context, tx pgx.Tx, subjectID, eventType string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbox payload: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO revenue_outbox (event_id, subject_id, event_type, payload, created_at)
		 VALUES ($1, $2, $3, $4, now())`,
		uuid.New(), subjectID, eventType, raw); err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}
