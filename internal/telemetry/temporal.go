package telemetry

import (
	"fmt"

	oteltemporal "go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
)

// TemporalInterceptor builds the official Temporal SDK OpenTelemetry
// interceptor: workflow/activity spans and client calls join the service
// trace, and the trace context rides inside Temporal payloads across the
// async workflow boundary. With telemetry disabled the interceptor runs over
// a noop tracer and records nothing.
func (telemetry *Telemetry) TemporalInterceptor() (interceptor.Interceptor, error) {
	tracing, err := oteltemporal.NewTracingInterceptor(oteltemporal.TracerOptions{
		Tracer:            telemetry.tracer,
		TextMapPropagator: telemetry.propagator,
	})
	if err != nil {
		return nil, fmt.Errorf("create Temporal tracing interceptor: %w", err)
	}
	return tracing, nil
}
