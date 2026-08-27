package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Producer publishes one keyed message. Kafka is the production
// implementation; tests substitute fakes.
type Producer interface {
	Publish(ctx context.Context, key, value []byte) error
}

// EventSource reads unpublished outbox rows and marks them published.
type EventSource interface {
	// Unpublished returns up to limit unpublished events in creation order.
	Unpublished(ctx context.Context, limit int) ([]Event, error)
	// MarkPublished records one event as published. It must fail when the
	// event does not exist or was already marked.
	MarkPublished(ctx context.Context, event Event) error
}

// Drain publishes up to batchSize unpublished events at-least-once: an event
// is marked published only after the producer accepts it, and the idempotent
// key makes replays safe. Any failure aborts the batch (fail-closed) and
// returns the count already published.
func Drain(ctx context.Context, source EventSource, producer Producer, batchSize int) (int, error) {
	if source == nil {
		return 0, errors.New("outbox event source is required")
	}
	if producer == nil {
		return 0, errors.New("Kafka producer is required")
	}
	if batchSize <= 0 {
		return 0, errors.New("batch size must be positive")
	}
	events, err := source.Unpublished(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("read outbox: %w", err)
	}
	published := 0
	for _, event := range events {
		envelope, err := BuildEnvelope(event)
		if err != nil {
			return published, fmt.Errorf("build envelope for %s: %w", event.EventID, err)
		}
		value, err := json.Marshal(envelope)
		if err != nil {
			return published, fmt.Errorf("encode envelope for %s: %w", event.EventID, err)
		}
		if err := producer.Publish(ctx, []byte(IdempotentKey(event)), value); err != nil {
			return published, fmt.Errorf("publish %s: %w", event.EventID, err)
		}
		if err := source.MarkPublished(ctx, event); err != nil {
			return published, fmt.Errorf("mark %s published: %w", event.EventID, err)
		}
		published++
	}
	return published, nil
}
