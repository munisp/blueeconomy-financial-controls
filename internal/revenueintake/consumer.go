package revenueintake

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// Consumer drains finance.revenue-assessments.v1: every message is
// signature-verified against the trusted authority keys, deduplicated by
// event id and landed as an assessment-side recon record. Nothing
// unverified is ever landed, and an uncommitted offset is the only retry
// mechanism — a verification failure blocks the group (fail-closed) until
// operators fix the key trust or the poison producer.
type Consumer struct {
	reader   *kafka.Reader
	verifier *Verifier
	store    *Store
	logger   *log.Logger
}

// Config carries the consumer wiring. Trusted keys come from VerifierFromEnv.
type Config struct {
	Brokers []string
	GroupID string
	Topic   string
}

// NewConsumer fails closed on missing brokers, group, topic, verifier or
// store — an unsigned/unverifiable intake pipeline must never start.
func NewConsumer(config Config, verifier *Verifier, store *Store, logger *log.Logger) (*Consumer, error) {
	addresses := make([]string, 0, len(config.Brokers))
	for _, broker := range config.Brokers {
		broker = strings.TrimSpace(broker)
		if broker == "" {
			return nil, errors.New("Kafka broker list contains an empty address")
		}
		addresses = append(addresses, broker)
	}
	if len(addresses) == 0 {
		return nil, errors.New("at least one Kafka broker is required")
	}
	if strings.TrimSpace(config.GroupID) == "" {
		return nil, errors.New("Kafka consumer group id is required")
	}
	topic := strings.TrimSpace(config.Topic)
	if topic == "" {
		return nil, errors.New("revenue intake topic is required")
	}
	if topic != Topic {
		return nil, fmt.Errorf("revenue intake topic %q is not the contract topic %q", topic, Topic)
	}
	if verifier == nil {
		return nil, errors.New("revenue intake verifier is required (fail-closed)")
	}
	if store == nil {
		return nil, errors.New("revenue intake store is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Consumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:     addresses,
			GroupID:     config.GroupID,
			Topic:       topic,
			StartOffset: kafka.FirstOffset,
			MinBytes:    1,
			MaxBytes:    4 << 20,
			MaxWait:     time.Second,
		}),
		verifier: verifier,
		store:    store,
		logger:   logger,
	}, nil
}

// Run fetches and lands messages until ctx is cancelled. Processing runs
// inside a Kafka consumer span that joins the producer trace through the
// W3C traceparent header (manual KafkaCarrier extraction — the repo's
// established discipline); with telemetry disabled the span is a
// non-recording noop and behavior is unchanged.
func (consumer *Consumer) Run(ctx context.Context) error {
	for {
		message, err := consumer.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch from %s: %w", Topic, err)
		}
		processErr := telemetry.Default().ConsumeSpan(ctx, Topic, message.Headers, func(spanCtx context.Context) error {
			return consumer.process(spanCtx, message)
		})
		if processErr != nil {
			// Fail closed: the offset is NOT committed. Verification failures
			// are permanent until trust changes; store failures are
			// transient. Either way nothing unverified is ever landed and the
			// message is retried after rejoin/restart.
			consumer.logger.Printf("revenue-intake: offset %d not committed: %v", message.Offset, processErr)
			return processErr
		}
		if err := consumer.reader.CommitMessages(ctx, message); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("commit offset %d: %w", message.Offset, err)
		}
	}
}

// process verifies and lands one message. A dedupe replay commits normally.
func (consumer *Consumer) process(ctx context.Context, message kafka.Message) error {
	event, err := consumer.verifier.VerifyAndParse(message.Value)
	if err != nil {
		return err
	}
	created, err := consumer.store.RecordAssessment(ctx, event)
	if err != nil {
		return err
	}
	if !created {
		consumer.logger.Printf("revenue-intake: event %s already landed (idempotent replay)", event.EventID)
	}
	return nil
}

// Close releases the reader.
func (consumer *Consumer) Close() error { return consumer.reader.Close() }
