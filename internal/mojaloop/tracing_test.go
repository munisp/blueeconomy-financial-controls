package mojaloop

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// installSpanRecorder installs a recorder-backed pipeline as the process-wide
// default and restores the previous default pipeline AND the previous OTel
// globals on cleanup. The prior teardown (`InstallDefault(nil)`) was a no-op
// that leaked the test pipeline as the process default while the installed
// nil-meter pipeline had already poisoned the global meter state — the
// PRA-138 test-isolation defect. Save/restore keeps each test hermetic.
func installSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	pipeline, err := telemetry.NewPipelineForTest(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)), "mojaloop-test")
	require.NoError(t, err)

	priorDefault := telemetry.Default()
	priorTracerProvider := otel.GetTracerProvider()
	priorMeterProvider := otel.GetMeterProvider()
	priorPropagator := otel.GetTextMapPropagator()

	telemetry.InstallDefault(pipeline)
	t.Cleanup(func() {
		telemetry.InstallDefault(priorDefault)
		otel.SetTracerProvider(priorTracerProvider)
		otel.SetMeterProvider(priorMeterProvider)
		otel.SetTextMapPropagator(priorPropagator)
	})
	return recorder
}

// TestFSPIOPTraceparentMapsToSpanLink proves the FSPIOP correlation handle
// (the inbound traceparent header) is mapped onto the callback span as a
// span link (OTEL_DESIGN §3 Mojaloop row).
func TestFSPIOPTraceparentMapsToSpanLink(t *testing.T) {
	recorder := installSpanRecorder(t)

	headers := http.Header{}
	headers.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	_, span := fspiopSpan(context.Background(), headers, "mojaloop.callback.transfer", "fspiop.transfer_id", "tr-123")
	span.End()

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	require.Len(t, spans[0].Links(), 1, "FSPIOP traceparent must become a span link")
	require.Equal(t, "0af7651916cd43dd8448eb211c80319c", spans[0].Links()[0].SpanContext.TraceID().String())
	var transferID string
	for _, kv := range spans[0].Attributes() {
		if string(kv.Key) == "fspiop.transfer_id" {
			transferID = kv.Value.AsString()
		}
	}
	require.Equal(t, "tr-123", transferID)
}

// TestFSPIOPSpanWithoutTraceparentHasNoLinks pins behavior when the Hub does
// not send a traceparent: the span still records, simply without a link.
func TestFSPIOPSpanWithoutTraceparentHasNoLinks(t *testing.T) {
	recorder := installSpanRecorder(t)

	_, span := fspiopSpan(context.Background(), http.Header{}, "mojaloop.callback.quote", "fspiop.quote_id", "q-1")
	span.End()

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	require.Empty(t, spans[0].Links())
}
