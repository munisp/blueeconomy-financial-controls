package revenueintake

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the PostgreSQL landing boundary for verified assessment events.
// The event id is the idempotency key: a replay is a no-op returning the
// stored row, never a second record.
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

// RecordAssessment lands one verified assessment event. created is false
// when the event id was already landed (at-least-once replay is a no-op).
func (store *Store) RecordAssessment(ctx context.Context, event AssessmentEvent) (created bool, err error) {
	if event.EventID == "" || len(event.Payload) == 0 {
		return false, errors.New("verified event id and payload are required")
	}
	if event.SignerKeyID == "" {
		return false, errors.New("verified signer key id is required (unverified events never land)")
	}
	var totalArg any
	if event.TotalMinor != nil {
		totalArg = *event.TotalMinor
	}
	var inserted string
	err = store.pool.QueryRow(ctx, `
		INSERT INTO revenue_intake_assessments
		    (event_id, topic, event_type, producer, signer_kid, occurred_at, correlation_id,
		     domain, call_reference, schedule_id, assessment_id, total_minor, currency,
		     mapping_error, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (event_id) DO NOTHING
		RETURNING event_id`,
		event.EventID, Topic, event.EventType, event.Producer, event.SignerKeyID,
		event.OccurredAt, event.CorrelationID, event.Domain, event.CallReference,
		event.ScheduleID, event.AssessmentID, totalArg, event.Currency,
		event.MappingError, event.Payload).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // replay: the event id is already landed
	}
	if err != nil {
		return false, fmt.Errorf("land revenue intake assessment: %w", err)
	}
	return true, nil
}
