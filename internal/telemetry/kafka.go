package telemetry

import (
	"context"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// KafkaCarrier adapts kafka-go message headers to a TextMapCarrier so the W3C
// traceparent (and baggage) ride inside the Kafka message across the
// asynchronous produce/consume boundary.
type KafkaCarrier struct {
	Headers *[]kafka.Header
}

// Get returns the first header value for key (case-insensitive per the W3C
// header contract).
func (carrier KafkaCarrier) Get(key string) string {
	for _, header := range *carrier.Headers {
		if equalFoldASCII(header.Key, key) {
			return string(header.Value)
		}
	}
	return ""
}

// Set replaces any existing header with the same key and appends the value.
func (carrier KafkaCarrier) Set(key, value string) {
	headers := *carrier.Headers
	for index, header := range headers {
		if equalFoldASCII(header.Key, key) {
			headers[index].Value = []byte(value)
			return
		}
	}
	*carrier.Headers = append(headers, kafka.Header{Key: key, Value: []byte(value)})
}

// Keys lists the header keys present on the message.
func (carrier KafkaCarrier) Keys() []string {
	keys := make([]string, 0, len(*carrier.Headers))
	for _, header := range *carrier.Headers {
		keys = append(keys, header.Key)
	}
	return keys
}

// InjectKafka writes the trace context of ctx into the message headers. A
// disabled pipeline or an invalid span context injects nothing, so a
// telemetry-disabled service emits byte-identical messages.
func (telemetry *Telemetry) InjectKafka(ctx context.Context, headers *[]kafka.Header) {
	telemetry.propagator.Inject(ctx, KafkaCarrier{Headers: headers})
}

// ExtractKafka reads the trace context from consumed message headers so the
// consumer span joins the producer trace.
func (telemetry *Telemetry) ExtractKafka(ctx context.Context, headers []kafka.Header) context.Context {
	return telemetry.propagator.Extract(ctx, KafkaCarrier{Headers: &headers})
}

// PublishSpan wraps one Kafka produce in a producer span and injects the
// trace context into the message headers; the write itself happens in
// publish. Produce failures are recorded on the span and returned unchanged.
func (telemetry *Telemetry) PublishSpan(ctx context.Context, topic string, headers *[]kafka.Header, publish func(ctx context.Context) error) error {
	ctx, span := telemetry.StartSpan(ctx, "kafka.publish "+topic, trace.SpanKindProducer, messagingAttributes(topic)...)
	defer span.End()
	telemetry.InjectKafka(ctx, headers)
	if err := publish(ctx); err != nil {
		span.RecordError(err)
		return err
	}
	return nil
}

// ConsumeSpan starts a consumer span that joins the producer trace carried in
// the message headers; handle runs with the extracted context.
func (telemetry *Telemetry) ConsumeSpan(ctx context.Context, topic string, headers []kafka.Header, handle func(ctx context.Context) error) error {
	ctx = telemetry.ExtractKafka(ctx, headers)
	ctx, span := telemetry.StartSpan(ctx, "kafka.consume "+topic, trace.SpanKindConsumer, messagingAttributes(topic)...)
	defer span.End()
	if err := handle(ctx); err != nil {
		span.RecordError(err)
		return err
	}
	return nil
}

func messagingAttributes(topic string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("messaging.system", "kafka"),
		attribute.String("messaging.destination.name", topic),
	}
}

// equalFoldASCII compares header keys case-insensitively (ASCII only, which
// covers every W3C propagation header).
func equalFoldASCII(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := 0; index < len(left); index++ {
		a, b := left[index], right[index]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

var _ propagation.TextMapCarrier = KafkaCarrier{}
