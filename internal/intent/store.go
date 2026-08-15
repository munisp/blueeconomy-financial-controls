package intent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
func (store *Store) Close()              { store.pool.Close() }
func (store *Store) Pool() *pgxpool.Pool { return store.pool }
func (store *Store) Exec(ctx context.Context, statement string) error {
	_, err := store.pool.Exec(ctx, statement)
	return err
}

func scanIntent(row pgx.Row) (Intent, error) {
	var retained Intent
	err := row.Scan(&retained.IntentID, &retained.ExternalRef, &retained.DebitAccountID, &retained.CreditAccountID,
		&retained.Amount, &retained.Ledger, &retained.Code, &retained.Currency, &retained.Maker,
		&retained.Checker, &retained.State, &retained.CreatedAt, &retained.UpdatedAt, &retained.Version)
	return retained, err
}

func (store *Store) Create(ctx context.Context, request CreateRequest) (Intent, error) {
	if err := request.Validate(); err != nil {
		return Intent{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Intent{}, fmt.Errorf("begin create: %w", err)
	}
	defer tx.Rollback(ctx)
	createdAt := time.Now().UTC()
	retained, err := scanIntent(tx.QueryRow(ctx, `
		INSERT INTO financial_intents (
			intent_id, external_ref, debit_account_id, credit_account_id, amount, ledger, code, currency, maker, state, created_at, updated_at, version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,1)
		ON CONFLICT (external_ref) DO NOTHING
		RETURNING intent_id, external_ref, debit_account_id, credit_account_id, amount, ledger, code, currency, maker, checker, state, created_at, updated_at, version`,
		request.IntentID, request.ExternalRef, request.DebitAccountID, request.CreditAccountID, request.Amount, request.Ledger, request.Code, request.Currency, request.Maker, StateDraft, createdAt))
	if errors.Is(err, pgx.ErrNoRows) {
		retained, err = scanIntent(tx.QueryRow(ctx, `SELECT intent_id, external_ref, debit_account_id, credit_account_id, amount, ledger, code, currency, maker, checker, state, created_at, updated_at, version FROM financial_intents WHERE external_ref = $1 FOR UPDATE`, request.ExternalRef))
		if errors.Is(err, pgx.ErrNoRows) {
			return Intent{}, ErrNotFound
		}
		if err != nil {
			return Intent{}, fmt.Errorf("lookup intent replay: %w", err)
		}
		if !retained.Matches(request) {
			return Intent{}, ErrImmutableConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Intent{}, fmt.Errorf("commit replay: %w", err)
		}
		return retained, nil
	}
	if err != nil {
		return Intent{}, fmt.Errorf("insert financial intent: %w", err)
	}
	if err := appendEvent(ctx, tx, retained, "financial_intent.created", createdAt); err != nil {
		return Intent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Intent{}, fmt.Errorf("commit intent: %w", err)
	}
	return retained, nil
}

func (store *Store) ListReconciliationIntents(ctx context.Context) ([]Intent, error) {
	rows, err := store.pool.Query(ctx, `SELECT intent_id, external_ref, debit_account_id, credit_account_id, amount, ledger, code, currency, maker, checker, state, created_at, updated_at, version FROM financial_intents WHERE state IN ($1, $2) ORDER BY external_ref`, StatePosted, StateVoided)
	if err != nil {
		return nil, fmt.Errorf("list reconciliation intents: %w", err)
	}
	defer rows.Close()
	intents := make([]Intent, 0)
	for rows.Next() {
		retained, scanErr := scanIntent(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan reconciliation intent: %w", scanErr)
		}
		intents = append(intents, retained)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reconciliation intents: %w", err)
	}
	return intents, nil
}

func (store *Store) Get(ctx context.Context, intentID string) (Intent, error) {
	retained, err := scanIntent(store.pool.QueryRow(ctx, `SELECT intent_id, external_ref, debit_account_id, credit_account_id, amount, ledger, code, currency, maker, checker, state, created_at, updated_at, version FROM financial_intents WHERE intent_id = $1`, intentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, ErrNotFound
	}
	if err != nil {
		return Intent{}, fmt.Errorf("get financial intent: %w", err)
	}
	return retained, nil
}

func (store *Store) Approve(ctx context.Context, intentID string, expectedVersion int64, checker string) (Intent, error) {
	current, err := store.Get(ctx, intentID)
	if err != nil {
		return Intent{}, err
	}
	if current.Version != expectedVersion {
		return Intent{}, ErrConflict
	}
	approved, err := Approve(current, checker)
	if err != nil {
		return Intent{}, err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Intent{}, fmt.Errorf("begin approval: %w", err)
	}
	defer tx.Rollback(ctx)
	updatedAt := time.Now().UTC()
	updated, err := scanIntent(tx.QueryRow(ctx, `UPDATE financial_intents SET checker = $1, state = $2, updated_at = $3, version = version + 1 WHERE intent_id = $4 AND state = $5 AND version = $6 RETURNING intent_id, external_ref, debit_account_id, credit_account_id, amount, ledger, code, currency, maker, checker, state, created_at, updated_at, version`, approved.Checker, approved.State, updatedAt, intentID, StateDraft, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, ErrConflict
	}
	if err != nil {
		return Intent{}, fmt.Errorf("approve financial intent: %w", err)
	}
	if err := appendEvent(ctx, tx, updated, "financial_intent.approved", updatedAt); err != nil {
		return Intent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Intent{}, fmt.Errorf("commit approval: %w", err)
	}
	return updated, nil
}

func (store *Store) Transition(ctx context.Context, intentID string, expectedVersion int64, next State) (Intent, error) {
	current, err := store.Get(ctx, intentID)
	if err != nil {
		return Intent{}, err
	}
	if current.Version != expectedVersion || !ValidOperationalTransition(current.State, next) {
		return Intent{}, ErrConflict
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return Intent{}, fmt.Errorf("begin transition: %w", err)
	}
	defer tx.Rollback(ctx)
	updatedAt := time.Now().UTC()
	updated, err := scanIntent(tx.QueryRow(ctx, `UPDATE financial_intents SET state = $1, updated_at = $2, version = version + 1 WHERE intent_id = $3 AND state = $4 AND version = $5 RETURNING intent_id, external_ref, debit_account_id, credit_account_id, amount, ledger, code, currency, maker, checker, state, created_at, updated_at, version`, next, updatedAt, intentID, current.State, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, ErrConflict
	}
	if err != nil {
		return Intent{}, fmt.Errorf("transition financial intent: %w", err)
	}
	if err := appendEvent(ctx, tx, updated, "financial_intent.state_changed", updatedAt); err != nil {
		return Intent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Intent{}, fmt.Errorf("commit state change: %w", err)
	}
	return updated, nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, value Intent, eventType string, createdAt time.Time) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode financial event: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO financial_intent_outbox (event_id, intent_id, event_type, payload, created_at) VALUES ($1,$2,$3,$4,$5)`, uuid.New(), value.IntentID, eventType, payload, createdAt); err != nil {
		return fmt.Errorf("write financial event: %w", err)
	}
	return nil
}
