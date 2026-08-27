package fx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists CBN reference rates under dual control.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return NewStore(pool), nil
}

func (store *Store) Close() { store.pool.Close() }

func scanRate(row pgx.Row) (Rate, error) {
	var retained Rate
	var state string
	err := row.Scan(&retained.RateID, &retained.NGNPerUSDMicro, &retained.EffectiveDate, &retained.Maker,
		&retained.Checker, &state, &retained.CreatedAt)
	retained.Confirmed = state == "CONFIRMED"
	if state == "REJECTED" {
		return Rate{}, ErrRateNotFound
	}
	return retained, err
}

// Enter records a maker-entered rate awaiting dual-control confirmation.
func (store *Store) Enter(ctx context.Context, rate Rate) (Rate, error) {
	if _, err := NewRate(rate.RateID, rate.NGNPerUSDMicro, rate.EffectiveDate, rate.Maker); err != nil {
		return Rate{}, err
	}
	createdAt := time.Now().UTC()
	retained, err := scanRate(store.pool.QueryRow(ctx, `
		INSERT INTO fx_rates (rate_id, base_currency, quote_currency, ngn_per_usd_micro, effective_date, maker, state, created_at)
		VALUES ($1,'USD','NGN',$2,$3,$4,'PENDING_CONFIRMATION',$5)
		RETURNING rate_id, ngn_per_usd_micro, effective_date, maker, checker, state, created_at`,
		rate.RateID, rate.NGNPerUSDMicro, rate.EffectiveDate, rate.Maker, createdAt))
	if err != nil {
		return Rate{}, fmt.Errorf("enter fx rate: %w", err)
	}
	return retained, nil
}

// Confirm applies the checker half of dual control. The database unique index
// permits at most one confirmed rate per effective date.
func (store *Store) Confirm(ctx context.Context, rateID string, checker string) (Rate, error) {
	current, err := store.get(ctx, rateID)
	if err != nil {
		return Rate{}, err
	}
	if _, err := current.Confirm(checker); err != nil {
		return Rate{}, err
	}
	retained, err := scanRate(store.pool.QueryRow(ctx, `
		UPDATE fx_rates SET checker = $1, state = 'CONFIRMED', confirmed_at = $2
		WHERE rate_id = $3 AND state = 'PENDING_CONFIRMATION'
		RETURNING rate_id, ngn_per_usd_micro, effective_date, maker, checker, state, created_at`,
		checker, time.Now().UTC(), rateID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rate{}, ErrRateConflict
	}
	if err != nil {
		return Rate{}, fmt.Errorf("confirm fx rate: %w", err)
	}
	return retained, nil
}

// Reject fail-closes a pending rate entry.
func (store *Store) Reject(ctx context.Context, rateID string, checker string) error {
	current, err := store.get(ctx, rateID)
	if err != nil {
		return err
	}
	if checker == "" || checker == current.Maker {
		return ErrDualControl
	}
	result, err := store.pool.Exec(ctx, `
		UPDATE fx_rates SET checker = $1, state = 'REJECTED', confirmed_at = $2
		WHERE rate_id = $3 AND state = 'PENDING_CONFIRMATION'`, checker, time.Now().UTC(), rateID)
	if err != nil {
		return fmt.Errorf("reject fx rate: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrRateConflict
	}
	return nil
}

// ConfirmedForDate returns the dual-control-confirmed CBN reference rate for a
// disbursement date. It fails closed when none exists.
func (store *Store) ConfirmedForDate(ctx context.Context, date time.Time) (Rate, error) {
	retained, err := scanRate(store.pool.QueryRow(ctx, `
		SELECT rate_id, ngn_per_usd_micro, effective_date, maker, checker, state, created_at
		FROM fx_rates WHERE effective_date = $1 AND state = 'CONFIRMED'`, date))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rate{}, ErrRateNotFound
	}
	if err != nil {
		return Rate{}, fmt.Errorf("lookup confirmed fx rate: %w", err)
	}
	return retained, nil
}

func (store *Store) get(ctx context.Context, rateID string) (Rate, error) {
	retained, err := scanRate(store.pool.QueryRow(ctx, `
		SELECT rate_id, ngn_per_usd_micro, effective_date, maker, checker, state, created_at
		FROM fx_rates WHERE rate_id = $1`, rateID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rate{}, ErrRateNotFound
	}
	if err != nil {
		return Rate{}, fmt.Errorf("get fx rate: %w", err)
	}
	return retained, nil
}
