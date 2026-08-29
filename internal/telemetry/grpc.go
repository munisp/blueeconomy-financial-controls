package telemetry

import (
	"context"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// GRPCServerOption installs the otelgrpc stats handler: every RPC gets a
// server span that joins the caller trace through the gRPC metadata carrier
// (W3C tracecontext+baggage). With telemetry disabled the handler runs over a
// noop provider and records nothing.
func (telemetry *Telemetry) GRPCServerOption() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler(
		otelgrpc.WithTracerProvider(telemetry.TracerProvider()),
		otelgrpc.WithMeterProvider(telemetry.MeterProvider()),
		otelgrpc.WithPropagators(telemetry.propagator),
	))
}

// UnaryBaggageAttributesInterceptor copies tenant.id/agency baggage (already
// extracted into the RPC context by the stats handler) onto the server span.
// Chain it before the authenticator so even UNAUTHENTICATED spans carry
// tenant attribution.
func UnaryBaggageAttributesInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		SetBaggageAttributes(ctx, trace.SpanFromContext(ctx))
		return handler(ctx, request)
	}
}
