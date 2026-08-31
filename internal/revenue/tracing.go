package revenue

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// OTel wiring for the revenue-assurance chain (extends the existing
// telemetry package — no parallel path). Spans: revenue.debit_note.issue,
// revenue.debit_note.transition, revenue.split.compute, revenue.recon.batch.
// Money counters go through telemetry.RecordMoneyOp so the tenant.id label
// appears only there (OTEL_DESIGN §2); with telemetry disabled every call
// is a non-recording noop and behavior is byte-identical.

// startSpan begins one internal span on the default pipeline.
func (store *Store) startSpan(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	return telemetry.Default().StartSpan(ctx, name, trace.SpanKindInternal, attributes...)
}

// transitionTraced wraps Transition in the lifecycle span.
func (store *Store) transitionTraced(ctx context.Context, debitNoteID string, request TransitionRequest, actor string) (DebitNote, error) {
	_, span := store.startSpan(ctx, "revenue.debit_note.transition",
		attribute.String("debit_note.to_state", request.ToState))
	defer span.End()
	note, err := store.Transition(ctx, debitNoteID, request, actor)
	if err != nil {
		span.RecordError(err)
		return note, err
	}
	span.SetAttributes(attribute.String("debit_note.from_state", note.State))
	return note, nil
}

// ComputeSplitTraced wraps the pure ComputeSplit in the split span. The asOf
// attribute pins the rule window for reproducibility; amounts stay off the
// span (money flows only through the low-cardinality counter).
func ComputeSplitTraced(ctx context.Context, amountMinor int64, rules []SplitRuleRow, asOf time.Time) ([]Allocation, error) {
	_, span := telemetry.Default().StartSpan(ctx, "revenue.split.compute", trace.SpanKindInternal,
		attribute.String("tsa.as_of", asOf.UTC().Format("2006-01-02")),
		attribute.Int("tsa.rule_rows", len(rules)),
	)
	defer span.End()
	allocations, err := ComputeSplit(amountMinor, rules, asOf)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(attribute.Int("tsa.allocations", len(allocations)))
	return allocations, nil
}

// recordMoneyOp counts one money operation (tenant label only via baggage).
func recordMoneyOp(ctx context.Context, operation string, attributes ...attribute.KeyValue) {
	telemetry.Default().RecordMoneyOp(ctx, operation, attributes...)
}
