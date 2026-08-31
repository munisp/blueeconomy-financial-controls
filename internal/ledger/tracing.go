package ledger

import (
	"context"

	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/munisp/blueeconomy-financial-controls/internal/telemetry"
)

// This file is the TigerBeetle coverage mandated by OTEL_DESIGN §3:
// TigerBeetle core has no native OTel, so every ledger operation emits a
// client-side span (kind=client, db.system=tigerbeetle) that joins the caller
// trace, plus a low-cardinality money counter (tenant label only here). The
// traced methods delegate to the fail-closed untraced implementations —
// instrumentation never changes ledger semantics, and with telemetry disabled
// every span is a cheap non-recording noop.

// ledgerAttributes returns the shared low-cardinality span attributes; ledger
// IDs and amounts stay off spans (cardinality), only the operation shape and
// the ledger/code coordinates are recorded.
func (service *Service) ledgerAttributes() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("db.system", "tigerbeetle"),
		attribute.Int("tigerbeetle.ledger", int(service.ledger)),
		attribute.Int("tigerbeetle.code", int(service.code)),
	}
}

func (service *Service) traced(ctx context.Context, operation string, run func() error) error {
	pipeline := telemetry.Default()
	spanAttributes := append(service.ledgerAttributes(), attribute.String("db.operation.name", operation))
	spanCtx, span := pipeline.StartSpan(ctx, "tigerbeetle."+operation, trace.SpanKindClient, spanAttributes...)
	defer span.End()
	err := run()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "tigerbeetle "+operation+" failed")
	}
	pipeline.RecordMoneyOp(spanCtx, operation, attribute.Int("tigerbeetle.ledger", int(service.ledger)))
	return err
}

// CreateAccountTraced is CreateAccount with a client-side TigerBeetle span.
func (service *Service) CreateAccountTraced(ctx context.Context, id tigerbeetle.Uint128, history bool) error {
	return service.traced(ctx, "create_account", func() error {
		return service.CreateAccount(id, history)
	})
}

// ReserveTraced is Reserve with a client-side TigerBeetle span and a money
// counter for the reserved amount.
func (service *Service) ReserveTraced(ctx context.Context, transferID, debitAccountID, creditAccountID tigerbeetle.Uint128, amount uint64, timeout uint32) error {
	return service.traced(ctx, "reserve", func() error {
		return service.Reserve(transferID, debitAccountID, creditAccountID, amount, timeout)
	})
}

// PostTraced is Post with a client-side TigerBeetle span.
func (service *Service) PostTraced(ctx context.Context, postTransferID, pendingTransferID tigerbeetle.Uint128) error {
	return service.traced(ctx, "post", func() error {
		return service.Post(postTransferID, pendingTransferID)
	})
}

// VoidTraced is Void with a client-side TigerBeetle span.
func (service *Service) VoidTraced(ctx context.Context, voidTransferID, pendingTransferID tigerbeetle.Uint128) error {
	return service.traced(ctx, "void", func() error {
		return service.Void(voidTransferID, pendingTransferID)
	})
}

// LookupTransferTraced is LookupTransfer with a client-side TigerBeetle span.
func (service *Service) LookupTransferTraced(ctx context.Context, id tigerbeetle.Uint128) (tigerbeetle.Transfer, bool, error) {
	var transfer tigerbeetle.Transfer
	var found bool
	err := service.traced(ctx, "lookup_transfer", func() error {
		var lookupErr error
		transfer, found, lookupErr = service.LookupTransfer(id)
		return lookupErr
	})
	return transfer, found, err
}

// CreateDisbursementPairTraced is CreateDisbursementPair with a client-side
// TigerBeetle span covering both deterministic legs of the disbursement.
func (service *Service) CreateDisbursementPairTraced(ctx context.Context, input DisbursementPairInput) (DisbursementLegs, error) {
	var legs DisbursementLegs
	err := service.traced(ctx, "create_disbursement_pair", func() error {
		var disburseErr error
		legs, disburseErr = service.CreateDisbursementPair(input)
		return disburseErr
	})
	return legs, err
}
