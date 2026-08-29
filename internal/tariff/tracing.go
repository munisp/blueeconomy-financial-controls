package tariff

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// ComputeTraced wraps the pure statutory Compute in a "tariff.compute" span.
// The computation is deterministic, so the span carries the asOf date that
// pins the rate window (reproducibility for audit) plus the count of charged
// lines; declaration content stays off the span. With telemetry disabled the
// span is a non-recording noop and the result is byte-identical.
func ComputeTraced(ctx context.Context, request AssessRequest, rates []RateRow, exemptions []ExemptionRow, asOf time.Time) Computation {
	_, span := telemetry.Default().StartSpan(ctx, "tariff.compute", trace.SpanKindInternal,
		attribute.String("tariff.as_of", asOf.UTC().Format("2006-01-02")),
		attribute.Int("tariff.rate_rows", len(rates)),
		attribute.Int("tariff.exemption_rows", len(exemptions)),
	)
	defer span.End()
	computation := Compute(request, rates, exemptions, asOf)
	charged := 0
	for _, line := range computation.Lines {
		if line.Applicability == LineCharged {
			charged++
		}
	}
	span.SetAttributes(
		attribute.Int("tariff.lines", len(computation.Lines)),
		attribute.Int("tariff.lines_charged", charged),
		attribute.Int("tariff.exemptions_applied", len(computation.Exemptions)),
	)
	return computation
}
