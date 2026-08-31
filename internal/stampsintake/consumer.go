package stampsintake

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// Consumer drains all four stamps.* lifecycle topics: every message is
// signature-verified against the trusted tax-stamps keys, checked against
// the topic/event-type contract, deduplicated by event id and landed.
// Nothing unverified is ever landed, and an uncommitted offset is the only
// retry mechanism — a verification failure blocks the group (fail-closed)
// until operators fix the key trust or the poison producer.
type Consumer struct {
	readers  []*kafka.Reader
	verifier *Verifier
	store    *Store
	logger   *log.Logger
}

// Config carries the consumer wiring. Trusted keys come from VerifierFromEnv.
type Config struct {
	Brokers []string
	GroupID string
}

// NewConsumer fails closed on missing brokers, group, verifier or store —
// an unsigned/unverifiable intake pipeline must never start. The topic set
// is the fixed contract set (TopicByEventType), not configurable: a stamps
// intake consumer must never silently watch the wrong topic.
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
	if verifier == nil {
		return nil, errors.New("stamps intake verifier is required (fail-closed)")
	}
	if store == nil {
		return nil, errors.New("stamps intake store is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	readers := make([]*kafka.Reader, 0, len(Topics))
	for _, topic := range Topics {
		readers = append(readers, kafka.NewReader(kafka.ReaderConfig{
			Brokers:     addresses,
			GroupID:     config.GroupID,
			Topic:       topic,
			StartOffset: kafka.FirstOffset,
			MinBytes:    1,
			MaxBytes:    4 << 20,
			MaxWait:     time.Second,
		}))
	}
	return &Consumer{readers: readers, verifier: verifier, store: store, logger: logger}, nil
}

// Run fetches and lands messages from every contract topic until ctx is
// cancelled or a message fails (fail-closed: the first failure stops the
// consumer without committing the offending offset).
func (consumer *Consumer) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(consumer.readers))
	var wg sync.WaitGroup
	for _, reader := range consumer.readers {
		wg.Add(1)
		go func(reader *kafka.Reader) {
			defer wg.Done()
			if err := consumer.runTopic(ctx, reader); err != nil {
				errs <- err
				cancel()
			}
		}(reader)
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

func (consumer *Consumer) runTopic(ctx context.Context, reader *kafka.Reader) error {
	topic := reader.Config().Topic
	for {
		message, err := reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch from %s: %w", topic, err)
		}
		processErr := telemetry.Default().ConsumeSpan(ctx, topic, message.Headers, func(spanCtx context.Context) error {
			return consumer.process(spanCtx, message)
		})
		if processErr != nil {
			// Fail closed: the offset is NOT committed. Verification failures
			// are permanent until trust changes; store failures are
			// transient. Either way nothing unverified is ever landed and the
			// message is retried after rejoin/restart.
			consumer.logger.Printf("stamps-intake: %s offset %d not committed: %v", topic, message.Offset, processErr)
			return processErr
		}
		if err := reader.CommitMessages(ctx, message); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("commit offset %d: %w", message.Offset, err)
		}
	}
}

// process verifies and lands one message. A dedupe replay commits normally.
func (consumer *Consumer) process(ctx context.Context, message kafka.Message) error {
	event, err := consumer.verifier.VerifyAndParse(message.Value, message.Topic)
	if err != nil {
		return err
	}
	created, err := consumer.store.RecordEvent(ctx, event)
	if err != nil {
		return err
	}
	if !created {
		consumer.logger.Printf("stamps-intake: event %s already landed (idempotent replay)", event.EventID)
	}
	return nil
}

// Close releases the readers.
func (consumer *Consumer) Close() error {
	var first error
	for _, reader := range consumer.readers {
		if err := reader.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
