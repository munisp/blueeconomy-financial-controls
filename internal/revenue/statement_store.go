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

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// SettlementInput is one collection record command.
type SettlementInput struct {
	DebitNoteID   string `json:"debitNoteId,omitempty"`
	BankReference string `json:"bankReference"`
	TBTransferID  string `json:"tbTransferId,omitempty"`
	AmountMinor   int64  `json:"amountMinor"`
	Currency      string `json:"currency"`
	PayerRef      string `json:"payerRef"`
	ValueDate     string `json:"valueDate"`
}

// Validate fails closed on a malformed settlement.
func (input SettlementInput) Validate() error {
	if strings.TrimSpace(input.BankReference) == "" || len(input.BankReference) > 128 {
		return errors.New("bankReference is required")
	}
	if input.AmountMinor <= 0 {
		return errors.New("amountMinor must be positive")
	}
	if input.Currency != "USD" && input.Currency != "NGN" {
		return errors.New("currency must be USD or NGN")
	}
	if strings.TrimSpace(input.PayerRef) == "" || len(input.PayerRef) > 256 {
		return errors.New("payerRef is required")
	}
	if _, err := time.Parse("2006-01-02", input.ValueDate); err != nil {
		return errors.New("valueDate must be YYYY-MM-DD")
	}
	if len(input.TBTransferID) > 64 {
		return errors.New("tbTransferId is too long")
	}
	return nil
}

// RecordSettlement persists one collection record (the queryable mirror of
// a TigerBeetle ledger entry; tb_transfer_id links upstream). Idempotent by
// key; replay returns the stored record.
func (store *Store) RecordSettlement(ctx context.Context, input SettlementInput, idempotencyKey, actor string) (Settlement, error) {
	if err := input.Validate(); err != nil {
		return Settlement{}, err
	}
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return Settlement{}, errors.New("idempotency key is required")
	}
	if actor == "" {
		return Settlement{}, errors.New("actor is required")
	}
	_, requestHash, err := canonicalJSON(input)
	if err != nil {
		return Settlement{}, err
	}
	if existing, err := store.settlementByIdempotency(ctx, idempotencyKey); err == nil {
		if existing.hash != requestHash {
			return Settlement{}, ErrIdempotencyConflict
		}
		return store.GetSettlement(ctx, existing.id)
	} else if !errors.Is(err, ErrNotFound) {
		return Settlement{}, err
	}
	settlement := Settlement{
		SettlementID:  uuid.NewString(),
		DebitNoteID:   input.DebitNoteID,
		BankReference: input.BankReference,
		TBTransferID:  input.TBTransferID,
		AmountMinor:   input.AmountMinor,
		Currency:      input.Currency,
		PayerRef:      input.PayerRef,
		ValueDate:     input.ValueDate,
		RecordedBy:    actor,
	}
	var noteArg any
	if input.DebitNoteID != "" {
		noteArg = input.DebitNoteID
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Settlement{}, fmt.Errorf("begin settlement transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO settlement_records (settlement_id, idempotency_key, request_hash, debit_note_id,
		     bank_reference, tb_transfer_id, amount_minor, currency, payer_ref, value_date, recorded_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		settlement.SettlementID, idempotencyKey, requestHash, noteArg, settlement.BankReference,
		settlement.TBTransferID, settlement.AmountMinor, settlement.Currency, settlement.PayerRef,
		settlement.ValueDate, actor); err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			if existing, lookupErr := store.settlementByIdempotency(ctx, idempotencyKey); lookupErr == nil {
				if existing.hash != requestHash {
					return Settlement{}, ErrIdempotencyConflict
				}
				return store.GetSettlement(ctx, existing.id)
			}
			return Settlement{}, ErrIdempotencyConflict
		}
		return Settlement{}, fmt.Errorf("insert settlement: %w", err)
	}
	if err := insertOutbox(ctx, tx, settlement.SettlementID, "revenue.settlement.recorded", map[string]any{
		"settlementId": settlement.SettlementID, "debitNoteId": input.DebitNoteID,
		"bankReference": settlement.BankReference, "amountMinor": settlement.AmountMinor,
		"currency": settlement.Currency, "actor": actor,
	}); err != nil {
		return Settlement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Settlement{}, fmt.Errorf("commit settlement: %w", err)
	}
	recordMoneyOp(ctx, "settlement.recorded")
	return store.GetSettlement(ctx, settlement.SettlementID)
}

// GetSettlement loads one settlement record.
func (store *Store) GetSettlement(ctx context.Context, settlementID string) (Settlement, error) {
	var settlement Settlement
	var noteID, tbID *string
	err := store.pool.QueryRow(ctx,
		`SELECT settlement_id, debit_note_id, bank_reference, tb_transfer_id, amount_minor,
		        currency, payer_ref, value_date::text, recorded_by, created_at
		 FROM settlement_records WHERE settlement_id = $1`, settlementID).
		Scan(&settlement.SettlementID, &noteID, &settlement.BankReference, &tbID,
			&settlement.AmountMinor, &settlement.Currency, &settlement.PayerRef,
			&settlement.ValueDate, &settlement.RecordedBy, &settlement.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settlement{}, ErrNotFound
	}
	if err != nil {
		return Settlement{}, fmt.Errorf("load settlement: %w", err)
	}
	if noteID != nil {
		settlement.DebitNoteID = *noteID
	}
	if tbID != nil {
		settlement.TBTransferID = *tbID
	}
	return settlement, nil
}

type settlementKeyLookup struct {
	id   string
	hash string
}

func (store *Store) settlementByIdempotency(ctx context.Context, key string) (settlementKeyLookup, error) {
	var lookup settlementKeyLookup
	err := store.pool.QueryRow(ctx,
		`SELECT settlement_id, request_hash FROM settlement_records WHERE idempotency_key = $1`, key).
		Scan(&lookup.id, &lookup.hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return settlementKeyLookup{}, ErrNotFound
	}
	return lookup, err
}

// IngestResult reports one statement ingest.
type IngestResult struct {
	StatementID  string `json:"statementId"`
	StatementRef string `json:"statementRef"`
	LineCount    int    `json:"lineCount"`
	DedupedLines int    `json:"dedupedLines"`
	Replay       bool   `json:"replay"`
}

// IngestStatement verifies a signed statement envelope (fail closed: no
// trusted key, bad signature or non-canonical payload all refuse) and
// persists the structured lines. Re-ingest of the same statement content is
// an idempotent replay; a conflicting duplicate bank reference raises a
// DUPLICATE_BANK_REF exception instead of inserting.
func (store *Store) IngestStatement(ctx context.Context, verifier *envelope.Verifier, bundle []byte, ingestedBy string) (IngestResult, error) {
	if verifier == nil {
		return IngestResult{}, errors.New("envelope verifier is required (fail-closed)")
	}
	if ingestedBy == "" {
		return IngestResult{}, errors.New("ingest actor is required")
	}
	verified, err := verifier.Verify(bundle)
	if err != nil {
		return IngestResult{}, err
	}
	if verified.ArtifactKind != ArtifactBankStatement {
		return IngestResult{}, fmt.Errorf("envelope artifact %q is not a bank statement", verified.ArtifactKind)
	}
	var statement StatementRequest
	if err := json.Unmarshal(verified.Payload, &statement); err != nil {
		return IngestResult{}, fmt.Errorf("decode statement payload: %w", err)
	}
	if err := statement.Validate(); err != nil {
		return IngestResult{}, err
	}
	statementHash := verified.PayloadSHA256

	// Idempotent replay: same content hash returns the stored statement.
	var existingID string
	err = store.pool.QueryRow(ctx,
		`SELECT statement_id FROM bank_statements WHERE statement_hash = $1`, statementHash).Scan(&existingID)
	if err == nil {
		return IngestResult{StatementID: existingID, StatementRef: statement.StatementRef,
			LineCount: len(statement.Lines), Replay: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return IngestResult{}, fmt.Errorf("check statement replay: %w", err)
	}

	statementID := uuid.NewString()
	// Extract the JWS from the envelope for audit indexing.
	var envelopeDoc struct {
		Entry []struct {
			Resource struct {
				Extension []struct {
					ValueString string `json:"valueString"`
				} `json:"extension"`
			} `json:"resource"`
		} `json:"entry"`
	}
	jws := ""
	if err := json.Unmarshal(bundle, &envelopeDoc); err == nil && len(envelopeDoc.Entry) == 1 {
		if extensions := envelopeDoc.Entry[0].Resource.Extension; len(extensions) > 0 {
			jws = extensions[0].ValueString
		}
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return IngestResult{}, fmt.Errorf("begin ingest transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO bank_statements (statement_id, statement_hash, bank, account_ref, statement_ref,
		     period_start, period_end, signer_kid, envelope, envelope_jws, ingested_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		statementID, statementHash, statement.Bank, statement.AccountRef, statement.StatementRef,
		statement.PeriodStart, statement.PeriodEnd, verified.SignerKeyID, bundle, jws, ingestedBy); err != nil {
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			return IngestResult{StatementID: statementID, StatementRef: statement.StatementRef,
				LineCount: len(statement.Lines), Replay: true}, nil
		}
		return IngestResult{}, fmt.Errorf("insert statement: %w", err)
	}
	deduped := 0
	var conflicts []exceptionDecision
	for index, line := range statement.Lines {
		lineNo := index + 1
		// Dedupe by bank reference: identical re-delivery is skipped; a
		// conflicting amount/currency on a known reference is an exception.
		var existingAmount int64
		var existingCurrency string
		probe := tx.QueryRow(ctx,
			`SELECT amount_minor, currency FROM bank_statement_lines WHERE bank = $1 AND bank_reference = $2`,
			statement.Bank, line.BankReference).Scan(&existingAmount, &existingCurrency)
		if probe == nil {
			if existingAmount != line.AmountMinor || existingCurrency != line.Currency {
				conflicts = append(conflicts, exceptionDecision{
					Class:           ExceptionDuplicateBankRef,
					DedupeKey:       ExceptionDuplicateBankRef + "|ref:" + statement.Bank + ":" + line.BankReference,
					StatementID:     statementID,
					StatementLineNo: lineNo,
					ExpectedMinor:   int64Ptr(existingAmount),
					ActualMinor:     int64Ptr(line.AmountMinor),
					Currency:        line.Currency,
					Detail:          "bank reference re-ingested with conflicting amount/currency",
				})
				continue
			}
			deduped++
			continue
		}
		if !errors.Is(probe, pgx.ErrNoRows) {
			return IngestResult{}, fmt.Errorf("check bank reference dedupe: %w", probe)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO bank_statement_lines (statement_id, line_no, bank, bank_reference, value_date,
			     direction, amount_minor, currency, narrative)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			statementID, lineNo, statement.Bank, line.BankReference, line.ValueDate,
			line.Direction, line.AmountMinor, line.Currency, line.Narrative); err != nil {
			if isUniqueViolation(err) {
				deduped++
				continue
			}
			return IngestResult{}, fmt.Errorf("insert statement line: %w", err)
		}
	}
	// Conflicting duplicate references raise exceptions against a dedicated
	// ingest run so the queue stays the single pane of glass.
	if len(conflicts) > 0 {
		runID, err := store.insertReconRun(ctx, tx, "MANUAL", ingestedBy)
		if err != nil {
			return IngestResult{}, err
		}
		for _, conflict := range conflicts {
			if err := store.insertException(ctx, tx, runID, conflict); err != nil {
				return IngestResult{}, err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE recon_runs SET state = 'COMPLETED', finished_at = now(), exception_count = $2 WHERE run_id = $1`,
			runID, len(conflicts)); err != nil {
			return IngestResult{}, fmt.Errorf("close ingest run: %w", err)
		}
	}
	if err := insertOutbox(ctx, tx, statementID, "revenue.statement.ingested", map[string]any{
		"statementId": statementID, "bank": statement.Bank, "statementRef": statement.StatementRef,
		"lineCount": len(statement.Lines), "dedupedLines": deduped, "signerKid": verified.SignerKeyID,
		"actor": ingestedBy,
	}); err != nil {
		return IngestResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IngestResult{}, fmt.Errorf("commit statement ingest: %w", err)
	}
	return IngestResult{StatementID: statementID, StatementRef: statement.StatementRef,
		LineCount: len(statement.Lines), DedupedLines: deduped}, nil
}
