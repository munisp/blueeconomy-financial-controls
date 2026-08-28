package cvff

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DisbursementLegs is the persisted dual-ledger evidence for one disbursement.
type DisbursementLegs struct {
	ApplicationID     string    `json:"application_id"`
	RateID            string    `json:"rate_id"`
	NGNPerUSDMicro    uint64    `json:"ngn_per_usd_micro"`
	FeeTransferID     string    `json:"fee_transfer_id"`
	CostTransferID    string    `json:"cost_transfer_id"`
	FeeNGNMinor       uint64    `json:"fee_ngn_minor"`
	CostUSDMinor      uint64    `json:"cost_usd_minor"`
	CostNGNEquivalent uint64    `json:"cost_ngn_equivalent"`
	CreatedAt         time.Time `json:"created_at"`
}

// RecordDisbursementLegs persists the posted ledger legs idempotently (one row
// per application) and writes the disbursement outbox event in one transaction.
func (store *Store) RecordDisbursementLegs(ctx context.Context, legs DisbursementLegs) error {
	if legs.ApplicationID == "" || legs.RateID == "" || legs.FeeTransferID == "" || legs.CostTransferID == "" {
		return errors.New("disbursement legs identifiers are required")
	}
	if legs.NGNPerUSDMicro == 0 || legs.FeeNGNMinor == 0 || legs.CostUSDMinor == 0 || legs.CostNGNEquivalent == 0 {
		return errors.New("disbursement legs amounts must be non-zero")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin disbursement legs: %w", err)
	}
	defer tx.Rollback(ctx)
	createdAt := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO cvff_disbursement_legs (
			application_id, rate_id, ngn_per_usd_micro, fee_transfer_id, cost_transfer_id,
			fee_ngn_minor, cost_usd_minor, cost_ngn_equivalent, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (application_id) DO NOTHING`,
		legs.ApplicationID, legs.RateID, legs.NGNPerUSDMicro, legs.FeeTransferID, legs.CostTransferID,
		legs.FeeNGNMinor, legs.CostUSDMinor, legs.CostNGNEquivalent, createdAt); err != nil {
		return fmt.Errorf("insert disbursement legs: %w", err)
	}
	if err := appendEvent(ctx, tx, legs.ApplicationID, "cvff.disbursed", legs, createdAt); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit disbursement legs: %w", err)
	}
	return nil
}

// DualLedgerReport is one row of the NGN/USD dual-ledger disbursement report.
type DualLedgerReport struct {
	ApplicationID     string    `json:"application_id"`
	BeneficiaryID     string    `json:"beneficiary_id"`
	State             State     `json:"state"`
	FeeNGNMinor       uint64    `json:"fee_ngn_minor"`
	CostUSDMinor      uint64    `json:"cost_usd_minor"`
	CostNGNEquivalent uint64    `json:"cost_ngn_equivalent"`
	NGNPerUSDMicro    uint64    `json:"ngn_per_usd_micro"`
	FeeTransferID     string    `json:"fee_transfer_id"`
	CostTransferID    string    `json:"cost_transfer_id"`
	DisbursedAt       time.Time `json:"disbursed_at"`
}

// DualLedgerReport joins applications with their disbursement legs so auditors
// see the NGN custodial fee and the FX-adjusted USD cost side by side. The
// report window is mandatory and half-open [from, to) on the leg timestamp;
// an absent or inverted window is a hard error, never a silent full scan.
func (store *Store) DualLedgerReport(ctx context.Context, from, to time.Time) ([]DualLedgerReport, error) {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, errors.New("dual-ledger report window must be a non-empty [from, to) range")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT a.application_id, a.beneficiary_id, a.state,
			l.fee_ngn_minor, l.cost_usd_minor, l.cost_ngn_equivalent, l.ngn_per_usd_micro,
			l.fee_transfer_id, l.cost_transfer_id, l.created_at
		FROM cvff_disbursement_legs l
		JOIN cvff_applications a ON a.application_id = l.application_id
		WHERE l.created_at >= $1 AND l.created_at < $2
		ORDER BY l.created_at`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("dual-ledger report: %w", err)
	}
	defer rows.Close()
	report := make([]DualLedgerReport, 0)
	for rows.Next() {
		var row DualLedgerReport
		if err := rows.Scan(&row.ApplicationID, &row.BeneficiaryID, &row.State,
			&row.FeeNGNMinor, &row.CostUSDMinor, &row.CostNGNEquivalent, &row.NGNPerUSDMicro,
			&row.FeeTransferID, &row.CostTransferID, &row.DisbursedAt); err != nil {
			return nil, fmt.Errorf("scan dual-ledger report row: %w", err)
		}
		report = append(report, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dual-ledger report: %w", err)
	}
	return report, nil
}

// DisbursementLegs returns the recorded legs for one application.
func (store *Store) DisbursementLegs(ctx context.Context, applicationID string) (DisbursementLegs, error) {
	var legs DisbursementLegs
	err := store.pool.QueryRow(ctx, `
		SELECT application_id, rate_id, ngn_per_usd_micro, fee_transfer_id, cost_transfer_id,
			fee_ngn_minor, cost_usd_minor, cost_ngn_equivalent, created_at
		FROM cvff_disbursement_legs WHERE application_id = $1`, applicationID).
		Scan(&legs.ApplicationID, &legs.RateID, &legs.NGNPerUSDMicro, &legs.FeeTransferID, &legs.CostTransferID,
			&legs.FeeNGNMinor, &legs.CostUSDMinor, &legs.CostNGNEquivalent, &legs.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DisbursementLegs{}, ErrNotFound
	}
	if err != nil {
		return DisbursementLegs{}, fmt.Errorf("get disbursement legs: %w", err)
	}
	return legs, nil
}
