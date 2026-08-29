package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/segmentio/kafka-go"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// KafkaProducer publishes envelopes to one Kafka topic with all-broker acks.
type KafkaProducer struct {
	writer *kafka.Writer
	topic  string
}

// NewKafkaProducer fails closed on missing brokers or topic. The producer
// requires full acknowledgment so a Kafka outage surfaces as an error rather
// than silent loss.
func NewKafkaProducer(brokers, topic string) (*KafkaProducer, error) {
	addresses := make([]string, 0)
	for _, broker := range strings.Split(brokers, ",") {
		broker = strings.TrimSpace(broker)
		if broker == "" {
			return nil, errors.New("Kafka broker list contains an empty address")
		}
		addresses = append(addresses, broker)
	}
	if len(addresses) == 0 {
		return nil, errors.New("at least one Kafka broker is required")
	}
	if strings.TrimSpace(topic) == "" {
		return nil, errors.New("Kafka topic is required")
	}
	return &KafkaProducer{writer: &kafka.Writer{
		Addr:         kafka.TCP(addresses...),
		Topic:        topic,
		RequiredAcks: kafka.RequireAll,
	}, topic: topic}, nil
}

// Publish writes one keyed message. The key is the outbox event ID, making
// at-least-once replays idempotent for downstream consumers. The publish runs
// inside a Kafka producer span and the W3C traceparent (plus baggage) is
// injected into the message headers so consumers join the publishing trace;
// with telemetry disabled the span is a non-recording noop, no header is
// added, and the wire format is unchanged.
func (producer *KafkaProducer) Publish(ctx context.Context, key, value []byte) error {
	if len(key) == 0 || len(value) == 0 {
		return errors.New("Kafka key and value are required")
	}
	message := kafka.Message{Key: key, Value: value}
	err := telemetry.Default().PublishSpan(ctx, producer.topic, &message.Headers, func(ctx context.Context) error {
		return producer.writer.WriteMessages(ctx, message)
	})
	if err != nil {
		return fmt.Errorf("write Kafka message: %w", err)
	}
	return nil
}

// Close releases the writer.
func (producer *KafkaProducer) Close() error { return producer.writer.Close() }
