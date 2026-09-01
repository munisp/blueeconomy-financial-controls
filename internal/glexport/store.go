package glexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists GL-export state on PostgreSQL. The journal itself is not
// duplicated: postings are derived on read from the authoritative settlement
// and disbursement records (the materialized mirror of the signed ledger),
// so export, trial balance and reconciliation all read one source of truth.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore fails closed on a missing pool.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("postgres pool is required")
	}
	return &Store{pool: pool}, nil
}

// journalSQL derives double-entry legs from the authoritative records:
//   - every settled collection debits the TSA cash account and credits the
//     revenue account for its currency;
//   - every approved disbursement (DISBURSEMENT_PENDING and beyond) debits the
//     CVFF expense account and credits the TSA cash account.
//
// Entry ids are deterministic (<source>-<id>-<D|C>) so re-derivation is
// byte-stable and exports are replayable.
const journalSQL = `
SELECT entry_id, account, direction, amount_minor, currency, value_date, booking_date, reference, counterparty, narrative
FROM (` + journalInnerSQL + `) legs
WHERE value_date >= $1 AND value_date <= $2
ORDER BY value_date, entry_id`

// JournalEntries returns every double-entry leg whose value date falls inside
// [start, end], ordered deterministically by (value_date, entry_id).
func (store *Store) JournalEntries(ctx context.Context, start, end time.Time) ([]JournalEntry, error) {
	rows, err := store.pool.Query(ctx, journalSQL, start, end)
	if err != nil {
		return nil, fmt.Errorf("query journal entries: %w", err)
	}
	defer rows.Close()
	var entries []JournalEntry
	for rows.Next() {
		var entry JournalEntry
		if err := rows.Scan(
			&entry.EntryID, &entry.Account, &entry.Direction, &entry.AmountMinor,
			&entry.Currency, &entry.ValueDate, &entry.BookingDate,
			&entry.Reference, &entry.Counterparty, &entry.Narrative,
		); err != nil {
			return nil, fmt.Errorf("scan journal entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate journal entries: %w", err)
	}
	return entries, nil
}

// TrialBalanceRow is one GL account's movement inside a period.
type TrialBalanceRow struct {
	Account          string `json:"account"`
	Currency         string `json:"currency"`
	DebitTotalMinor  int64  `json:"debit_total_minor"`
	CreditTotalMinor int64  `json:"credit_total_minor"`
	NetMinor         int64  `json:"net_minor"` // credit - debit (signed balance)
	EntryCount       int    `json:"entry_count"`
}

// TrialBalance aggregates journal legs per account over [start, end].
func (store *Store) TrialBalance(ctx context.Context, start, end time.Time) ([]TrialBalanceRow, error) {
	rows, err := store.pool.Query(ctx, `
SELECT account, currency,
       COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0)  AS debit_total,
       COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0) AS credit_total,
       COUNT(*)
FROM (`+journalInnerSQL+`) legs
WHERE value_date >= $1 AND value_date <= $2
GROUP BY account, currency
ORDER BY account`, start, end)
	if err != nil {
		return nil, fmt.Errorf("query trial balance: %w", err)
	}
	defer rows.Close()
	var result []TrialBalanceRow
	for rows.Next() {
		var row TrialBalanceRow
		if err := rows.Scan(&row.Account, &row.Currency, &row.DebitTotalMinor, &row.CreditTotalMinor, &row.EntryCount); err != nil {
			return nil, fmt.Errorf("scan trial balance row: %w", err)
		}
		row.NetMinor = row.CreditTotalMinor - row.DebitTotalMinor
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trial balance: %w", err)
	}
	return result, nil
}

// OpeningBalance returns the signed balance (credit minus debit) of one GL
// account over all journal legs with value date strictly before `start`.
func (store *Store) OpeningBalance(ctx context.Context, account string, start time.Time) (int64, error) {
	var balance int64
	err := store.pool.QueryRow(ctx, `
SELECT COALESCE(SUM(CASE WHEN direction = 'CREDIT' THEN amount_minor ELSE -amount_minor END), 0)
FROM (`+journalInnerSQL+`) legs
WHERE account = $1 AND value_date < $2`, account, start).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("query opening balance: %w", err)
	}
	return balance, nil
}

// journalInnerSQL is the UNION ALL subquery shared by every journal read.
const journalInnerSQL = `
    SELECT 'SETL-' || settlement_id || '-D' AS entry_id,
           'TSA:' || currency              AS account,
           'DEBIT'                          AS direction,
           amount_minor,
           currency,
           value_date,
           created_at::date                 AS booking_date,
           bank_reference                   AS reference,
           payer_ref                        AS counterparty,
           'Collection settlement ' || settlement_id AS narrative
    FROM settlement_records
    UNION ALL
    SELECT 'SETL-' || settlement_id || '-C',
           'REVENUE:' || currency,
           'CREDIT',
           amount_minor,
           currency,
           value_date,
           created_at::date,
           bank_reference,
           payer_ref,
           'Collection settlement ' || settlement_id
    FROM settlement_records
    UNION ALL
    SELECT 'CVFF-' || a.application_id || '-D',
           'CVFF:EXPENSE:' || a.currency,
           'DEBIT',
           a.amount,
           a.currency,
           l.created_at::date,
           l.created_at::date,
           a.external_ref,
           a.beneficiary_id,
           'CVFF disbursement ' || a.application_id
    FROM cvff_disbursement_legs l
    JOIN cvff_applications a ON a.application_id = l.application_id
    WHERE a.state IN ('DISBURSEMENT_PENDING', 'DISBURSED', 'AUDITED')
    UNION ALL
    SELECT 'CVFF-' || a.application_id || '-C',
           'TSA:' || a.currency,
           'CREDIT',
           a.amount,
           a.currency,
           l.created_at::date,
           l.created_at::date,
           a.external_ref,
           a.beneficiary_id,
           'CVFF disbursement ' || a.application_id
    FROM cvff_disbursement_legs l
    JOIN cvff_applications a ON a.application_id = l.application_id
    WHERE a.state IN ('DISBURSEMENT_PENDING', 'DISBURSED', 'AUDITED')
`

// DisbursementInstructions returns approved disbursement/refund instructions
// whose execution date falls inside [start, end]. The creditor account is the
// verified receiving-bank principal recorded under strict separation of
// duties — never a fabricated account.
func (store *Store) DisbursementInstructions(ctx context.Context, start, end time.Time) ([]CreditInstruction, error) {
	rows, err := store.pool.Query(ctx, `
SELECT a.application_id, a.amount, a.currency, a.beneficiary_id,
       ra.principal_id, a.external_ref, l.created_at::date
FROM cvff_disbursement_legs l
JOIN cvff_applications a ON a.application_id = l.application_id
JOIN cvff_role_assignments ra
  ON ra.application_id = a.application_id AND ra.role = 'RECEIVING_BANK'
WHERE a.state IN ('DISBURSEMENT_PENDING', 'DISBURSED', 'AUDITED')
  AND l.created_at::date >= $1 AND l.created_at::date <= $2
ORDER BY l.created_at::date, a.application_id`, start, end)
	if err != nil {
		return nil, fmt.Errorf("query disbursement instructions: %w", err)
	}
	defer rows.Close()
	var instructions []CreditInstruction
	for rows.Next() {
		var instruction CreditInstruction
		var externalRef string
		if err := rows.Scan(
			&instruction.InstructionID, &instruction.AmountMinor, &instruction.Currency,
			&instruction.CreditorName, &instruction.CreditorAcct, &externalRef,
			&instruction.ExecutionDate,
		); err != nil {
			return nil, fmt.Errorf("scan disbursement instruction: %w", err)
		}
		instruction.Narrative = "CVFF disbursement " + externalRef
		instructions = append(instructions, instruction)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate disbursement instructions: %w", err)
	}
	return instructions, nil
}

// ExportBatch is the recorded audit trail of one emitted ISO 20022 feed.
type ExportBatch struct {
	BatchID          string          `json:"batch_id"`
	ExportType       string          `json:"export_type"`
	AccountRef       string          `json:"account_ref"`
	PeriodStart      time.Time       `json:"period_start"`
	PeriodEnd        time.Time       `json:"period_end"`
	Currency         string          `json:"currency"`
	EntryCount       int             `json:"entry_count"`
	TotalDebitMinor  int64           `json:"total_debit_minor"`
	TotalCreditMinor int64           `json:"total_credit_minor"`
	PayloadSHA256    string          `json:"payload_sha256"`
	Payload          string          `json:"-"`
	Envelope         json.RawMessage `json:"-"`
	EnvelopeJWS      string          `json:"-"`
	SignerKeyID      string          `json:"signer_kid"`
	ExportedBy       string          `json:"exported_by"`
	CreatedAt        time.Time       `json:"created_at"`
}

// ErrExportReplay marks a byte-identical re-export (payload hash already
// recorded). Callers surface this as 409 Conflict.
var ErrExportReplay = errors.New("export payload already recorded")

// InsertExportBatch records one export. A duplicate payload hash fails closed
// as ErrExportReplay (idempotent replay, not silent success).
func (store *Store) InsertExportBatch(ctx context.Context, batch ExportBatch) error {
	_, err := store.pool.Exec(ctx, `
INSERT INTO export_batches (
    batch_id, export_type, account_ref, period_start, period_end, currency,
    entry_count, total_debit_minor, total_credit_minor,
    payload_sha256, payload, envelope, envelope_jws, signer_kid, exported_by
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		batch.BatchID, batch.ExportType, batch.AccountRef, batch.PeriodStart, batch.PeriodEnd, batch.Currency,
		batch.EntryCount, batch.TotalDebitMinor, batch.TotalCreditMinor,
		batch.PayloadSHA256, batch.Payload, batch.Envelope, batch.EnvelopeJWS, batch.SignerKeyID, batch.ExportedBy)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrExportReplay
		}
		return fmt.Errorf("insert export batch: %w", err)
	}
	return nil
}

// ExportBatches returns recorded exports for an account/period scope, oldest
// first. An empty account matches all accounts.
func (store *Store) ExportBatches(ctx context.Context, account string, start, end time.Time) ([]ExportBatch, error) {
	rows, err := store.pool.Query(ctx, `
SELECT batch_id, export_type, account_ref, period_start, period_end, currency,
       entry_count, total_debit_minor, total_credit_minor, payload_sha256,
       signer_kid, exported_by, created_at
FROM export_batches
WHERE ($1 = '' OR account_ref = $1) AND period_start = $2 AND period_end = $3
ORDER BY created_at, batch_id`, account, start, end)
	if err != nil {
		return nil, fmt.Errorf("query export batches: %w", err)
	}
	defer rows.Close()
	var batches []ExportBatch
	for rows.Next() {
		var batch ExportBatch
		if err := rows.Scan(
			&batch.BatchID, &batch.ExportType, &batch.AccountRef, &batch.PeriodStart, &batch.PeriodEnd,
			&batch.Currency, &batch.EntryCount, &batch.TotalDebitMinor, &batch.TotalCreditMinor,
			&batch.PayloadSHA256, &batch.SignerKeyID, &batch.ExportedBy, &batch.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan export batch: %w", err)
		}
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate export batches: %w", err)
	}
	return batches, nil
}

// GetExportPayload returns the stored XML payload of one batch (for audit
// download / ERP replay).
func (store *Store) GetExportPayload(ctx context.Context, batchID string) (string, string, error) {
	var payload, payloadHash string
	err := store.pool.QueryRow(ctx,
		`SELECT payload, payload_sha256 FROM export_batches WHERE batch_id = $1`, batchID).
		Scan(&payload, &payloadHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("export batch %q not found", batchID)
	}
	if err != nil {
		return "", "", fmt.Errorf("query export payload: %w", err)
	}
	return payload, payloadHash, nil
}

// PeriodClose is the maker-checker close record for one accounting period.
type PeriodClose struct {
	PeriodCloseID string          `json:"period_close_id"`
	PeriodStart   time.Time       `json:"period_start"`
	PeriodEnd     time.Time       `json:"period_end"`
	Status        string          `json:"status"`
	RequestedBy   string          `json:"requested_by"`
	ApprovedBy    string          `json:"approved_by,omitempty"`
	TrialBalance  json.RawMessage `json:"trial_balance,omitempty"`
	RequestedAt   time.Time       `json:"requested_at"`
	ApprovedAt    *time.Time      `json:"approved_at,omitempty"`
}

// ErrPeriodExists marks a close request for an already-registered period.
var ErrPeriodExists = errors.New("period close already exists")

// CreatePeriodClose registers a PENDING_APPROVAL close (maker step).
func (store *Store) CreatePeriodClose(ctx context.Context, close PeriodClose) error {
	_, err := store.pool.Exec(ctx, `
INSERT INTO period_close (period_close_id, period_start, period_end, status, requested_by)
VALUES ($1, $2, $3, 'PENDING_APPROVAL', $4)`,
		close.PeriodCloseID, close.PeriodStart, close.PeriodEnd, close.RequestedBy)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrPeriodExists
		}
		return fmt.Errorf("insert period close: %w", err)
	}
	return nil
}

// ErrPeriodApprove marks a failed checker step: unknown id, already closed,
// or maker == checker (dual control enforced in the UPDATE predicate).
var ErrPeriodApprove = errors.New("period close cannot be approved (unknown, already closed, or maker equals checker)")

// ApprovePeriodClose executes the checker step atomically: it only transitions
// a PENDING_APPROVAL close whose maker differs from the checker, freezing the
// trial-balance snapshot at approval time.
func (store *Store) ApprovePeriodClose(ctx context.Context, periodCloseID, checker string, trialBalance json.RawMessage, approvedAt time.Time) error {
	result, err := store.pool.Exec(ctx, `
UPDATE period_close
SET status = 'CLOSED', approved_by = $2, trial_balance = $3, approved_at = $4
WHERE period_close_id = $1 AND status = 'PENDING_APPROVAL' AND requested_by <> $2`,
		periodCloseID, checker, trialBalance, approvedAt)
	if err != nil {
		return fmt.Errorf("approve period close: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrPeriodApprove
	}
	return nil
}

// GetPeriodClose loads one close record by id.
func (store *Store) GetPeriodClose(ctx context.Context, periodCloseID string) (PeriodClose, error) {
	var close PeriodClose
	var approvedBy *string
	err := store.pool.QueryRow(ctx, `
SELECT period_close_id, period_start, period_end, status, requested_by, approved_by, trial_balance, requested_at, approved_at
FROM period_close WHERE period_close_id = $1`, periodCloseID).Scan(
		&close.PeriodCloseID, &close.PeriodStart, &close.PeriodEnd, &close.Status,
		&close.RequestedBy, &approvedBy, &close.TrialBalance, &close.RequestedAt, &close.ApprovedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PeriodClose{}, fmt.Errorf("period close %q not found", periodCloseID)
	}
	if err != nil {
		return PeriodClose{}, fmt.Errorf("query period close: %w", err)
	}
	if approvedBy != nil {
		close.ApprovedBy = *approvedBy
	}
	return close, nil
}

// ListPeriodCloses returns all close records, most recent first.
func (store *Store) ListPeriodCloses(ctx context.Context) ([]PeriodClose, error) {
	rows, err := store.pool.Query(ctx, `
SELECT period_close_id, period_start, period_end, status, requested_by, approved_by, trial_balance, requested_at, approved_at
FROM period_close ORDER BY requested_at DESC, period_close_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("query period closes: %w", err)
	}
	defer rows.Close()
	var closes []PeriodClose
	for rows.Next() {
		var close PeriodClose
		var approvedBy *string
		if err := rows.Scan(
			&close.PeriodCloseID, &close.PeriodStart, &close.PeriodEnd, &close.Status,
			&close.RequestedBy, &approvedBy, &close.TrialBalance, &close.RequestedAt, &close.ApprovedAt,
		); err != nil {
			return nil, fmt.Errorf("scan period close: %w", err)
		}
		if approvedBy != nil {
			close.ApprovedBy = *approvedBy
		}
		closes = append(closes, close)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate period closes: %w", err)
	}
	return closes, nil
}

// ReconciliationRun is the persisted journals-vs-exports comparison.
type ReconciliationRun struct {
	RunID              string          `json:"run_id"`
	PeriodStart        time.Time       `json:"period_start"`
	PeriodEnd          time.Time       `json:"period_end"`
	JournalEntryCount  int             `json:"journal_entry_count"`
	JournalTotalMinor  int64           `json:"journal_total_minor"`
	ExportedEntryCount int             `json:"exported_entry_count"`
	ExportedTotalMinor int64           `json:"exported_total_minor"`
	Balanced           bool            `json:"balanced"`
	Differences        json.RawMessage `json:"differences"`
	RunBy              string          `json:"run_by"`
	CreatedAt          time.Time       `json:"created_at"`
}

// InsertReconciliationRun persists one run.
func (store *Store) InsertReconciliationRun(ctx context.Context, run ReconciliationRun) error {
	_, err := store.pool.Exec(ctx, `
INSERT INTO reconciliation_runs (
    run_id, period_start, period_end, journal_entry_count, journal_total_minor,
    exported_entry_count, exported_total_minor, balanced, differences, run_by
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		run.RunID, run.PeriodStart, run.PeriodEnd, run.JournalEntryCount, run.JournalTotalMinor,
		run.ExportedEntryCount, run.ExportedTotalMinor, run.Balanced, run.Differences, run.RunBy)
	if err != nil {
		return fmt.Errorf("insert reconciliation run: %w", err)
	}
	return nil
}

// ListReconciliationRuns returns runs for a period scope, most recent first.
func (store *Store) ListReconciliationRuns(ctx context.Context, start, end time.Time, limit int) ([]ReconciliationRun, error) {
	rows, err := store.pool.Query(ctx, `
SELECT run_id, period_start, period_end, journal_entry_count, journal_total_minor,
       exported_entry_count, exported_total_minor, balanced, differences, run_by, created_at
FROM reconciliation_runs
WHERE period_start = $1 AND period_end = $2
ORDER BY created_at DESC, run_id DESC
LIMIT $3`, start, end, limit)
	if err != nil {
		return nil, fmt.Errorf("query reconciliation runs: %w", err)
	}
	defer rows.Close()
	var runs []ReconciliationRun
	for rows.Next() {
		var run ReconciliationRun
		if err := rows.Scan(
			&run.RunID, &run.PeriodStart, &run.PeriodEnd, &run.JournalEntryCount, &run.JournalTotalMinor,
			&run.ExportedEntryCount, &run.ExportedTotalMinor, &run.Balanced, &run.Differences,
			&run.RunBy, &run.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan reconciliation run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reconciliation runs: %w", err)
	}
	return runs, nil
}
