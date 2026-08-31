package stampsintake

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the PostgreSQL landing boundary for verified stamps events. The
// event id is the idempotency key: a replay is a no-op returning the stored
// row, never a second record.
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

// RecordEvent lands one verified stamps lifecycle event. created is false
// when the event id was already landed (at-least-once replay is a no-op).
func (store *Store) RecordEvent(ctx context.Context, event StampEvent) (created bool, err error) {
	if event.EventID == "" || len(event.Payload) == 0 {
		return false, errors.New("verified event id and payload are required")
	}
	if event.SignerKeyID == "" {
		return false, errors.New("verified signer key id is required (unverified events never land)")
	}
	var dutyArg, quantityArg any
	if event.TotalDutyKobo != nil {
		dutyArg = *event.TotalDutyKobo
	}
	if event.Quantity != nil {
		quantityArg = *event.Quantity
	}
	var inserted string
	err = store.pool.QueryRow(ctx, `
		INSERT INTO stamps_intake_events
		    (event_id, topic, event_type, producer, signer_kid, occurred_at, correlation_id,
		     assessment_id, declaration_ref, batch_id, total_duty_kobo, quantity,
		     mapping_error, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (event_id) DO NOTHING
		RETURNING event_id`,
		event.EventID, event.Topic, event.EventType, event.Producer, event.SignerKeyID,
		event.OccurredAt, event.CorrelationID, event.AssessmentID, event.DeclarationRef,
		event.BatchID, dutyArg, quantityArg, event.MappingError, event.Payload).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // replay: the event id is already landed
	}
	if err != nil {
		return false, fmt.Errorf("land stamps intake event: %w", err)
	}
	return true, nil
}
