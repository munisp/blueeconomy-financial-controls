package revenue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
)

// insertReconRun opens one run row inside a transaction.
func (store *Store) insertReconRun(ctx context.Context, tx pgx.Tx, triggerKind, actor string) (string, error) {
	runID := uuid.NewString()
	if _, err := tx.Exec(ctx,
		`INSERT INTO recon_runs (run_id, trigger_kind, state, started_by) VALUES ($1, $2, 'RUNNING', $3)`,
		runID, triggerKind, actor); err != nil {
		return "", fmt.Errorf("insert recon run: %w", err)
	}
	return runID, nil
}

// insertException appends one exception (plus its RAISED audit event) inside
// a transaction. The OPEN dedupe index makes re-raising idempotent: a
// duplicate dedupe key is skipped, not doubled.
func (store *Store) insertException(ctx context.Context, tx pgx.Tx, runID string, decision exceptionDecision) error {
	var existing string
	probe := tx.QueryRow(ctx,
		`SELECT exception_id FROM recon_exceptions WHERE dedupe_key = $1 AND state = 'OPEN'`,
		decision.DedupeKey).Scan(&existing)
	if probe == nil {
		return nil // already queued — re-runs never double-raise
	}
	if !errors.Is(probe, pgx.ErrNoRows) {
		return fmt.Errorf("check exception dedupe: %w", probe)
	}
	exceptionID := uuid.NewString()
	detail, err := json.Marshal(map[string]string{"summary": decision.Detail})
	if err != nil {
		return err
	}
	noteArg := nullableString(decision.DebitNoteID)
	settlementArg := nullableString(decision.SettlementID)
	statementArg := nullableString(decision.StatementID)
	var lineArg any
	if decision.StatementLineNo > 0 {
		lineArg = decision.StatementLineNo
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO recon_exceptions (exception_id, run_id, class, state, dedupe_key, debit_note_id,
		     settlement_id, statement_id, statement_line_no, expected_amount_minor, actual_amount_minor,
		     currency, detail, raised_by)
		 VALUES ($1, $2, $3, 'OPEN', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		exceptionID, runID, decision.Class, decision.DedupeKey, noteArg, settlementArg,
		statementArg, lineArg, decision.ExpectedMinor, decision.ActualMinor,
		nullableString(decision.Currency), detail, reconEngineActor); err != nil {
		return fmt.Errorf("insert exception: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO recon_exception_events (event_id, exception_id, event_kind, actor, note)
		 VALUES ($1, $2, 'RAISED', $3, $4)`,
		uuid.NewString(), exceptionID, reconEngineActor, decision.Detail); err != nil {
		return fmt.Errorf("insert exception event: %w", err)
	}
	if err := insertOutbox(ctx, tx, exceptionID, "revenue.recon.exception_raised", map[string]any{
		"exceptionId": exceptionID, "runId": runID, "class": decision.Class,
		"debitNoteId": decision.DebitNoteID, "settlementId": decision.SettlementID,
	}); err != nil {
		return err
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// RunRecon executes one three-way reconciliation batch atomically: load the
// unmatched legs, plan deterministically, persist matches + SETTLED
// transitions + exceptions, close the run. triggerKind is MANUAL or
// TEMPORAL_BATCH (the long-running path the Temporal workflow drives).
func (store *Store) RunRecon(ctx context.Context, triggerKind, actor string, asOf time.Time) (RunSummary, error) {
	if triggerKind != "MANUAL" && triggerKind != "TEMPORAL_BATCH" {
		return RunSummary{}, errors.New("trigger kind must be MANUAL or TEMPORAL_BATCH")
	}
	if actor == "" {
		return RunSummary{}, errors.New("actor is required")
	}
	ctx, span := store.startSpan(ctx, "revenue.recon.batch",
		attribute.String("recon.trigger", triggerKind))
	defer span.End()
	_ = ctx

	input, err := store.loadReconInput(ctx, asOf)
	if err != nil {
		return RunSummary{}, err
	}
	plan := planRecon(input)

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return RunSummary{}, fmt.Errorf("begin recon transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	runID, err := store.insertReconRun(ctx, tx, triggerKind, actor)
	if err != nil {
		return RunSummary{}, err
	}
	for _, match := range plan.Matches {
		if _, err := tx.Exec(ctx,
			`INSERT INTO recon_matches (match_id, run_id, debit_note_id, settlement_id, statement_id,
			     statement_line_no, amount_minor, currency)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (settlement_id) DO NOTHING`,
			uuid.NewString(), runID, match.DebitNoteID, match.SettlementID, match.StatementID,
			match.StatementLineNo, match.AmountMinor, match.Currency); err != nil {
			return RunSummary{}, fmt.Errorf("insert match: %w", err)
		}
	}
	// Matched notes move to SETTLED under the recon system actor.
	for noteID, settlementID := range plan.SettledNotes {
		var priorState string
		probe := tx.QueryRow(ctx,
			`SELECT state FROM revenue_debit_notes WHERE debit_note_id = $1 FOR UPDATE`, noteID).Scan(&priorState)
		if probe != nil {
			continue
		}
		result, err := tx.Exec(ctx,
			`UPDATE revenue_debit_notes SET state = 'SETTLED', updated_at = now()
			 WHERE debit_note_id = $1 AND state IN ('ISSUED', 'ACKED', 'DISPUTED')`, noteID)
		if err != nil {
			return RunSummary{}, fmt.Errorf("settle note: %w", err)
		}
		if result.RowsAffected() == 0 {
			continue
		}
		if err := insertTransition(ctx, tx, noteID, priorState, StateSettled,
			"three-way match with settlement "+settlementID, reconEngineActor, ""); err != nil {
			return RunSummary{}, err
		}
	}
	// Intake assessments observed by a settlement move to SETTLEMENT_OBSERVED
	// with the settlement link (one guarded update; a concurrent rerun that
	// already linked is skipped).
	for _, intakeMatch := range plan.IntakeMatches {
		if _, err := tx.Exec(ctx,
			`UPDATE revenue_intake_assessments
			 SET state = 'SETTLEMENT_OBSERVED', settlement_id = $2
			 WHERE event_id = $1 AND state = 'OPEN'`,
			intakeMatch.EventID, intakeMatch.SettlementID); err != nil {
			return RunSummary{}, fmt.Errorf("link intake assessment settlement: %w", err)
		}
	}
	for _, decision := range plan.Exceptions {
		if err := store.insertException(ctx, tx, runID, decision); err != nil {
			return RunSummary{}, err
		}
	}
	var openExceptions int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM recon_exceptions WHERE run_id = $1`, runID).Scan(&openExceptions); err != nil {
		return RunSummary{}, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE recon_runs SET state = 'COMPLETED', finished_at = now(), matched_count = $2, exception_count = $3
		 WHERE run_id = $1`, runID, len(plan.Matches), openExceptions); err != nil {
		return RunSummary{}, fmt.Errorf("close recon run: %w", err)
	}
	if err := insertOutbox(ctx, tx, runID, "revenue.recon.run_completed", map[string]any{
		"runId": runID, "matchedCount": len(plan.Matches), "exceptionCount": openExceptions,
		"triggerKind": triggerKind, "actor": actor,
	}); err != nil {
		return RunSummary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RunSummary{}, fmt.Errorf("commit recon run: %w", err)
	}
	return store.GetRun(ctx, runID)
}

// loadReconInput snapshots the unmatched legs for one batch.
func (store *Store) loadReconInput(ctx context.Context, asOf time.Time) (reconInput, error) {
	input := reconInput{AsOf: asOf}
	noteRows, err := store.pool.Query(ctx,
		`SELECT n.debit_note_id, n.assessment_id, n.agency, n.entity_ref, n.document_number,
		        n.amount_usd_minor, n.amount_ngn_minor, n.effective_date::text, n.due_date::text, n.state,
		        n.maker, n.correlation_id
		 FROM revenue_debit_notes n
		 WHERE n.state IN ('ISSUED', 'ACKED', 'DISPUTED')
		   AND NOT EXISTS (SELECT 1 FROM recon_matches m WHERE m.debit_note_id = n.debit_note_id)`)
	if err != nil {
		return input, fmt.Errorf("load open notes: %w", err)
	}
	defer noteRows.Close()
	for noteRows.Next() {
		var note DebitNote
		var documentNumber *string
		if err := noteRows.Scan(&note.DebitNoteID, &note.AssessmentID, &note.Agency, &note.EntityRef,
			&documentNumber, &note.AmountUSDMinor, &note.AmountNGNMinor, &note.EffectiveDate,
			&note.DueDate, &note.State, &note.Maker, &note.CorrelationID); err != nil {
			return input, fmt.Errorf("scan open note: %w", err)
		}
		if documentNumber != nil {
			note.DocumentNumber = *documentNumber
		}
		input.Notes = append(input.Notes, note)
	}
	if err := noteRows.Err(); err != nil {
		return input, err
	}

	settlementRows, err := store.pool.Query(ctx,
		`SELECT s.settlement_id, s.debit_note_id, s.bank_reference, s.amount_minor, s.currency,
		        s.payer_ref, s.value_date::text, s.recorded_by
		 FROM settlement_records s
		 WHERE NOT EXISTS (SELECT 1 FROM recon_matches m WHERE m.settlement_id = s.settlement_id)
		   AND NOT EXISTS (SELECT 1 FROM revenue_intake_assessments i
		                   WHERE i.settlement_id = s.settlement_id)
		 ORDER BY s.created_at, s.settlement_id`)
	if err != nil {
		return input, fmt.Errorf("load unmatched settlements: %w", err)
	}
	defer settlementRows.Close()
	for settlementRows.Next() {
		var settlement Settlement
		var noteID *string
		if err := settlementRows.Scan(&settlement.SettlementID, &noteID, &settlement.BankReference,
			&settlement.AmountMinor, &settlement.Currency, &settlement.PayerRef,
			&settlement.ValueDate, &settlement.RecordedBy); err != nil {
			return input, fmt.Errorf("scan settlement: %w", err)
		}
		if noteID != nil {
			settlement.DebitNoteID = *noteID
		}
		input.Settlements = append(input.Settlements, settlement)
	}
	if err := settlementRows.Err(); err != nil {
		return input, err
	}

	lineRows, err := store.pool.Query(ctx,
		`SELECT l.statement_id, l.line_no, l.bank_reference, l.value_date::text, l.amount_minor, l.currency
		 FROM bank_statement_lines l
		 WHERE l.direction = 'CREDIT'
		   AND NOT EXISTS (SELECT 1 FROM recon_matches m
		                   WHERE m.statement_id = l.statement_id AND m.statement_line_no = l.line_no)
		 ORDER BY l.statement_id, l.line_no`)
	if err != nil {
		return input, fmt.Errorf("load unmatched statement lines: %w", err)
	}
	defer lineRows.Close()
	for lineRows.Next() {
		var line statementLeg
		if err := lineRows.Scan(&line.StatementID, &line.LineNo, &line.BankReference,
			&line.ValueDate, &line.AmountMinor, &line.Currency); err != nil {
			return input, fmt.Errorf("scan statement line: %w", err)
		}
		input.Lines = append(input.Lines, line)
	}
	if err := lineRows.Err(); err != nil {
		return input, err
	}

	// Open revenue-intake assessments (verified port-interoperability
	// events) form the assessment-side leg for external fee/dues
	// assessments.
	intakeRows, err := store.pool.Query(ctx,
		`SELECT event_id::text, call_reference, assessment_id,
		        COALESCE(total_minor, 0), currency, mapping_error
		 FROM revenue_intake_assessments
		 WHERE state = 'OPEN'
		 ORDER BY received_at, event_id`)
	if err != nil {
		return input, fmt.Errorf("load open intake assessments: %w", err)
	}
	defer intakeRows.Close()
	for intakeRows.Next() {
		var intake IntakeAssessment
		if err := intakeRows.Scan(&intake.EventID, &intake.CallReference, &intake.AssessmentID,
			&intake.TotalMinor, &intake.Currency, &intake.MappingError); err != nil {
			return input, fmt.Errorf("scan intake assessment: %w", err)
		}
		input.Intake = append(input.Intake, intake)
	}
	return input, intakeRows.Err()
}

// GetRun loads one run summary.
func (store *Store) GetRun(ctx context.Context, runID string) (RunSummary, error) {
	var summary RunSummary
	var finishedAt *time.Time
	err := store.pool.QueryRow(ctx,
		`SELECT run_id, state, matched_count, exception_count, started_by, started_at, finished_at
		 FROM recon_runs WHERE run_id = $1`, runID).
		Scan(&summary.RunID, &summary.State, &summary.MatchedCount, &summary.ExceptionCount,
			&summary.StartedBy, &summary.StartedAt, &finishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunSummary{}, ErrNotFound
	}
	if err != nil {
		return RunSummary{}, fmt.Errorf("load recon run: %w", err)
	}
	if finishedAt != nil {
		summary.FinishedAt = *finishedAt
	}
	return summary, nil
}

// ListExceptions returns the exception queue, optionally only OPEN entries.
func (store *Store) ListExceptions(ctx context.Context, openOnly bool) ([]Exception, error) {
	query := `SELECT exception_id, run_id, class, state, debit_note_id, settlement_id, statement_id,
	          statement_line_no, expected_amount_minor, actual_amount_minor, currency, detail,
	          resolver, resolution_note, resolved_at, created_at
	          FROM recon_exceptions`
	if openOnly {
		query += ` WHERE state = 'OPEN'`
	}
	query += ` ORDER BY created_at, exception_id`
	rows, err := store.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list exceptions: %w", err)
	}
	defer rows.Close()
	exceptions := []Exception{}
	for rows.Next() {
		var exception Exception
		var noteID, settlementID, statementID, currency, resolver, resolutionNote *string
		var lineNo *int
		var detailRaw []byte
		if err := rows.Scan(&exception.ExceptionID, &exception.RunID, &exception.Class, &exception.State,
			&noteID, &settlementID, &statementID, &lineNo, &exception.ExpectedMinor, &exception.ActualMinor,
			&currency, &detailRaw, &resolver, &resolutionNote, &exception.ResolvedAt, &exception.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan exception: %w", err)
		}
		if noteID != nil {
			exception.DebitNoteID = *noteID
		}
		if settlementID != nil {
			exception.SettlementID = *settlementID
		}
		if statementID != nil {
			exception.StatementID = *statementID
		}
		if lineNo != nil {
			exception.StatementLineNo = *lineNo
		}
		if currency != nil {
			exception.Currency = *currency
		}
		if resolver != nil {
			exception.Resolver = *resolver
		}
		if resolutionNote != nil {
			exception.ResolutionNote = *resolutionNote
		}
		exception.Detail = string(detailRaw)
		exceptions = append(exceptions, exception)
	}
	return exceptions, rows.Err()
}

// ResolveException closes one OPEN exception with a mandatory resolution
// note; the resolver is the verified subject and every resolution is
// audited in recon_exception_events. Idempotent: resolving an already
// resolved exception returns it unchanged.
func (store *Store) ResolveException(ctx context.Context, exceptionID, resolver, note string) (Exception, error) {
	if exceptionID == "" || resolver == "" {
		return Exception{}, errors.New("exception id and resolver are required")
	}
	if len(strings.TrimSpace(note)) < 8 {
		return Exception{}, errors.New("resolution note is required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Exception{}, fmt.Errorf("begin resolution transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx,
		`UPDATE recon_exceptions
		 SET state = 'RESOLVED', resolver = $2, resolution_note = $3, resolved_at = now()
		 WHERE exception_id = $1 AND state = 'OPEN'`, exceptionID, resolver, note)
	if err != nil {
		return Exception{}, fmt.Errorf("resolve exception: %w", err)
	}
	if result.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO recon_exception_events (event_id, exception_id, event_kind, actor, note)
			 VALUES ($1, $2, 'RESOLVED', $3, $4)`, uuid.NewString(), exceptionID, resolver, note); err != nil {
			return Exception{}, fmt.Errorf("insert resolution audit: %w", err)
		}
		if err := insertOutbox(ctx, tx, exceptionID, "revenue.recon.exception_resolved", map[string]any{
			"exceptionId": exceptionID, "resolver": resolver,
		}); err != nil {
			return Exception{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Exception{}, fmt.Errorf("commit resolution: %w", err)
	}
	exceptions, err := store.listExceptionsByID(ctx, exceptionID)
	if err != nil {
		return Exception{}, err
	}
	if len(exceptions) == 0 {
		return Exception{}, ErrNotFound
	}
	return exceptions[0], nil
}

func (store *Store) listExceptionsByID(ctx context.Context, exceptionID string) ([]Exception, error) {
	all, err := store.ListExceptions(ctx, false)
	if err != nil {
		return nil, err
	}
	var found []Exception
	for _, exception := range all {
		if exception.ExceptionID == exceptionID {
			found = append(found, exception)
		}
	}
	return found, nil
}

// ListMatches returns the completed three-way matches for one run.
func (store *Store) ListMatches(ctx context.Context, runID string) ([]Match, error) {
	rows, err := store.pool.Query(ctx,
		`SELECT match_id, run_id, debit_note_id, settlement_id, statement_id, statement_line_no,
		        amount_minor, currency, created_at
		 FROM recon_matches WHERE run_id = $1 ORDER BY created_at, match_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list matches: %w", err)
	}
	defer rows.Close()
	matches := []Match{}
	for rows.Next() {
		var match Match
		if err := rows.Scan(&match.MatchID, &match.RunID, &match.DebitNoteID, &match.SettlementID,
			&match.StatementID, &match.StatementLineNo, &match.AmountMinor, &match.Currency, &match.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan match: %w", err)
		}
		matches = append(matches, match)
	}
	return matches, rows.Err()
}
