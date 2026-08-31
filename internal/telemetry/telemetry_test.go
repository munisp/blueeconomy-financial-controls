package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// newRecordingPipeline builds an enabled Telemetry whose spans land in an
// in-process recorder (and whose metrics land in a manual reader), so tests
// assert on real SDK behavior without any collector.
func newRecordingPipeline(t *testing.T) (*Telemetry, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	pipeline := &Telemetry{
		config:         Config{Enabled: true, ServiceName: "telemetry-test"},
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		tracer:         tracerProvider.Tracer("telemetry-test"),
		meter:          meterProvider.Meter("telemetry-test"),
		propagator:     propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		dropped:        &atomic.Int64{},
	}
	require.NoError(t, pipeline.initMoneyCounter())
	return pipeline, recorder, reader
}

// endedSpan returns the single ended span with the given name.
func endedSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range recorder.Ended() {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("no ended span named %q (have %d spans)", name, len(recorder.Ended()))
	return nil
}

func spanAttribute(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

// TestKafkaCarrierRoundTrip proves W3C tracecontext survives the Kafka
// produce/consume boundary through the manual kafka-go header carrier.
func TestKafkaCarrierRoundTrip(t *testing.T) {
	pipeline, recorder, _ := newRecordingPipeline(t)
	ctx, span := pipeline.StartSpan(context.Background(), "upstream", trace.SpanKindInternal)
	headers := make([]kafka.Header, 0)
	pipeline.InjectKafka(ctx, &headers)
	span.End()

	traceparent := KafkaCarrier{Headers: &headers}.Get("traceparent")
	require.NotEmpty(t, traceparent, "traceparent must be injected into Kafka headers")

	extracted := pipeline.ExtractKafka(context.Background(), headers)
	remote := trace.SpanContextFromContext(extracted)
	require.True(t, remote.IsValid())
	require.True(t, remote.IsRemote())
	upstream := endedSpan(t, recorder, "upstream")
	require.Equal(t, upstream.SpanContext().TraceID(), remote.TraceID(), "consumer must join the producer trace")

	// A consumer span started from the extracted context is a child of the
	// producer trace.
	consumerCtx, consumer := pipeline.StartSpan(extracted, "kafka.consume test", trace.SpanKindConsumer)
	require.Equal(t, upstream.SpanContext().TraceID(), trace.SpanContextFromContext(consumerCtx).TraceID())
	consumer.End()
}

// TestHTTPPropagationRoundTripAndBaggageAttributes proves the otelhttp server
// middleware joins the caller trace from the traceparent header and stamps
// tenant.id/agency baggage onto the server span.
func TestHTTPPropagationRoundTripAndBaggageAttributes(t *testing.T) {
	pipeline, recorder, _ := newRecordingPipeline(t)
	handler := pipeline.Middleware("test-service", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/assessments", nil)
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	request.Header.Set("baggage", "tenant.id=tenant-kano,agency=NIMASA")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	serverSpan := endedSpan(t, recorder, http.MethodPost)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", serverSpan.SpanContext().TraceID().String(), "server span must join the caller trace")
	require.Equal(t, trace.SpanKindServer, serverSpan.SpanKind())
	tenant, ok := spanAttribute(serverSpan, "tenant.id")
	require.True(t, ok, "tenant.id baggage must become a span attribute")
	require.Equal(t, "tenant-kano", tenant)
	agency, ok := spanAttribute(serverSpan, "agency")
	require.True(t, ok, "agency baggage must become a span attribute")
	require.Equal(t, "NIMASA", agency)
}

// metadataCarrier adapts gRPC metadata to a propagation carrier for the test
// client (the server side is exercised through the real otelgrpc handler).
type metadataCarrier metadata.MD

func (carrier metadataCarrier) Get(key string) string {
	return metadata.MD(carrier).Get(key)[0]
}

func (carrier metadataCarrier) Set(key, value string) {
	metadata.MD(carrier).Set(key, value)
}

func (carrier metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(carrier))
	for key := range carrier {
		keys = append(keys, key)
	}
	return keys
}

// TestGRPCMetadataCarrierPropagation proves the otelgrpc stats handler joins
// the caller trace carried in gRPC metadata and the baggage interceptor
// stamps tenant attribution on the server span.
func TestGRPCMetadataCarrierPropagation(t *testing.T) {
	pipeline, recorder, _ := newRecordingPipeline(t)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(
		pipeline.GRPCServerOption(),
		grpc.ChainUnaryInterceptor(UnaryBaggageAttributesInterceptor()),
	)
	healthpb.RegisterHealthServer(server, health.NewServer())
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithInsecure(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	// The client injects traceparent+baggage into the RPC metadata exactly as
	// an instrumented caller would.
	outgoing, cancel := context.WithCancel(context.Background())
	defer cancel()
	member, err := baggage.NewMember("tenant.id", "tenant-lagos")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	outgoing = baggage.ContextWithBaggage(outgoing, bag)
	carrier := metadataCarrier(metadata.MD{})
	pipeline.Propagator().Inject(trace.ContextWithSpanContext(outgoing, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		SpanID:     trace.SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})), carrier)
	outgoing = metadata.NewOutgoingContext(outgoing, metadata.MD(carrier))

	_, err = healthpb.NewHealthClient(conn).Check(outgoing, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)

	serverSpan := endedSpan(t, recorder, "grpc.health.v1.Health/Check")
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", serverSpan.SpanContext().TraceID().String(), "RPC span must join the caller trace through gRPC metadata")
	tenant, ok := spanAttribute(serverSpan, "tenant.id")
	require.True(t, ok, "tenant.id baggage must become a span attribute on the RPC span")
	require.Equal(t, "tenant-lagos", tenant)
}

// TestDisabledTelemetryBootAndRequest proves the sanctioned fail-open: with
// no OTLP endpoint the pipeline sets up cleanly, middleware serves requests
// unchanged, spans are non-recording, and shutdown is a noop.
func TestDisabledTelemetryBootAndRequest(t *testing.T) {
	pipeline, err := Setup(context.Background(), Config{ServiceName: "disabled-test"})
	require.NoError(t, err)
	require.False(t, pipeline.Enabled())

	called := false
	handler := pipeline.Middleware("disabled-test", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		writer.WriteHeader(http.StatusTeapot)
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/anything", nil)
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	handler.ServeHTTP(recorder, request)
	require.True(t, called, "request must be served with telemetry disabled")
	require.Equal(t, http.StatusTeapot, recorder.Code)

	// Manual spans and carriers are safe noops when disabled.
	ctx, span := pipeline.StartSpan(context.Background(), "noop", trace.SpanKindInternal)
	require.False(t, span.IsRecording())
	headers := make([]kafka.Header, 0)
	pipeline.InjectKafka(ctx, &headers)
	require.Empty(t, headers, "a disabled pipeline must not alter the Kafka wire format")
	span.End()
	pipeline.RecordMoneyOp(ctx, "reserve")
	require.NoError(t, pipeline.Shutdown(context.Background()))
}

// TestDropCountingExporter proves collector-down export failures are counted
// on telemetry_dropped_total, never surfaced to callers as request failures.
func TestDropCountingExporter(t *testing.T) {
	dropped := &atomic.Int64{}
	exporter := &dropCountingExporter{inner: failingExporter{}, dropped: dropped}
	err := exporter.ExportSpans(context.Background(), make([]sdktrace.ReadOnlySpan, 7))
	require.Error(t, err, "the batcher still sees the failure (it logs and discards)")
	require.Equal(t, int64(7), dropped.Load())
}

type failingExporter struct{}

func (failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector unreachable")
}

func (failingExporter) Shutdown(context.Context) error { return nil }

// TestMoneyCounterCarriesTenantLabel proves the tenant.id label is attached
// to the money counter (and only there) from baggage.
func TestMoneyCounterCarriesTenantLabel(t *testing.T) {
	pipeline, _, reader := newRecordingPipeline(t)
	member, err := baggage.NewMember("tenant.id", "tenant-money")
	require.NoError(t, err)
	bag, err := baggage.New(member)
	require.NoError(t, err)
	pipeline.RecordMoneyOp(baggage.ContextWithBaggage(context.Background(), bag), "reserve",
		attribute.Int("tigerbeetle.ledger", 1))

	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	var found bool
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != MoneyCounterName {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			require.Len(t, sum.DataPoints, 1)
			value, ok := sum.DataPoints[0].Attributes.Value(attribute.Key("tenant.id"))
			require.True(t, ok, "money counter must carry the tenant.id label")
			require.Equal(t, "tenant-money", value.AsString())
			found = true
		}
	}
	require.True(t, found, "money counter metric must be recorded")
}

// TestLoadConfigContract pins the endpoint contract: unset = disabled,
// malformed = fail-closed startup error.
func TestLoadConfigContract(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
	config, err := LoadConfig("contract-test")
	require.NoError(t, err)
	require.False(t, config.Enabled, "unset endpoint must mean telemetry disabled")

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "not-a-host-port")
	_, err = LoadConfig("contract-test")
	require.Error(t, err, "malformed endpoint fails closed")

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4317")
	config, err = LoadConfig("contract-test")
	require.NoError(t, err)
	require.True(t, config.Enabled)
	require.Equal(t, "otel-collector:4317", config.Endpoint)

	t.Setenv("OTEL_SDK_DISABLED", "true")
	_, err = LoadConfig("contract-test")
	require.Error(t, err, "OTEL_SDK_DISABLED=true conflicting with an endpoint fails closed")
}

// TestInstallDefaultNeverInstallsNilGlobals is the PRA-138 regression pin:
// NewPipelineForTest pipelines carry no SDK meter provider; InstallDefault
// must fall back to the explicit noop providers instead of pushing a nil
// delegate into the OTel global state. Before the fix this panicked in
// global.(*meter).setDelegate whenever a placeholder meter already existed
// (e.g. created by otelpgx during DB-gated store tests).
func TestInstallDefaultNeverInstallsNilGlobals(t *testing.T) {
	// Force placeholder instruments to exist against the global placeholder
	// provider BEFORE any real provider is installed — the exact ordering
	// that made the mojaloop package panic in one invocation.
	placeholder := otel.Meter("pra-138-placeholder")
	_, err := placeholder.Int64Counter("pra_138_placeholder_total")
	require.NoError(t, err)

	priorDefault := Default()
	priorTracerProvider := otel.GetTracerProvider()
	priorMeterProvider := otel.GetMeterProvider()
	priorPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		InstallDefault(priorDefault)
		otel.SetTracerProvider(priorTracerProvider)
		otel.SetMeterProvider(priorMeterProvider)
		otel.SetTextMapPropagator(priorPropagator)
	})

	pipeline, err := NewPipelineForTest(sdktrace.NewTracerProvider(), "pra-138-test")
	require.NoError(t, err)
	require.NotPanics(t, func() { InstallDefault(pipeline) },
		"InstallDefault must never push a nil meter delegate into the OTel global state")

	// The installed globals must be usable (noop) providers, not nil delegates.
	meter := otel.GetMeterProvider().Meter("pra-138-after-install")
	counter, err := meter.Int64Counter("pra_138_after_install_total")
	require.NoError(t, err)
	require.NotPanics(t, func() { counter.Add(context.Background(), 1) })

	tracer := otel.GetTracerProvider().Tracer("pra-138-after-install")
	_, span := tracer.Start(context.Background(), "pra-138")
	require.NotPanics(t, func() { span.End() })

	// InstallDefault(nil) remains a no-op (callers use it defensively).
	require.NotPanics(t, func() { InstallDefault(nil) })
}
