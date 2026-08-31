package mojaloop

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// Mojaloop rail coverage (OTEL_DESIGN §3): our FSPIOP handlers and client
// calls emit spans, the FSPIOP signature verification gets its own span, and
// the FSPIOP correlation handle — the W3C traceparent header when the Hub
// sends one — is mapped onto the callback span as a span link, joining the
// quote/transfer/fulfil legs across the asynchronous boundary. With telemetry
// disabled every span is a non-recording noop and the rail is unchanged.

// fspiopSpan starts one rail span, mapping the inbound traceparent (FSPIOP
// correlation) to a span link when present and recording the correlation ID
// (quote/transfer ID) as an attribute.
func fspiopSpan(ctx context.Context, headers http.Header, name, correlationKey, correlationID string) (context.Context, trace.Span) {
	pipeline := telemetry.Default()
	options := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("messaging.system", "mojaloop-fspiop"),
			attribute.String(correlationKey, correlationID),
		),
	}
	extracted := pipeline.Propagator().Extract(ctx, propagation.HeaderCarrier(headers))
	if remote := trace.SpanContextFromContext(extracted); remote.IsValid() {
		options = append(options, trace.WithLinks(trace.Link{SpanContext: remote}))
	}
	return pipeline.Tracer().Start(ctx, name, options...)
}

// verifySpan wraps the FSPIOP JWS verification in its own span: a forged or
// misrouted callback is visible as a failed verify span on the money path.
func verifySpan(ctx context.Context, verify func() error) error {
	_, span := telemetry.Default().StartSpan(ctx, "mojaloop.fspiop.verify", trace.SpanKindInternal)
	defer span.End()
	err := verify()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "FSPIOP signature verification failed")
	}
	return err
}

// clientSpan wraps one outbound signed FSPIOP call (quote/transfer/fulfil
// leg) in a client span; retries inside Do stay within the single logical
// span and the final error is recorded on it.
func (client Client) clientSpan(ctx context.Context, name, correlationKey, correlationID string, call func(ctx context.Context) (*http.Response, error)) (*http.Response, error) {
	attributes := []attribute.KeyValue{attribute.String("messaging.system", "mojaloop-fspiop")}
	if correlationID != "" {
		attributes = append(attributes, attribute.String(correlationKey, correlationID))
	}
	ctx, span := telemetry.Default().StartSpan(ctx, name, trace.SpanKindClient, attributes...)
	defer span.End()
	response, err := call(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "FSPIOP call failed")
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
	return response, nil
}
