package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleEvent() Event {
	return Event{
		EventID:   "6f1a2b3c-0000-4000-8000-000000000001",
		SubjectID: "intent-001",
		EventType: "financial_intent.approved",
		Payload:   json.RawMessage(`{"intent_id":"intent-001","maker":"kc-maker","checker":"kc-checker","state":"APPROVED","currency":"NGN"}`),
		CreatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	}
}

func TestBuildEnvelopeMapping(t *testing.T) {
	envelope, err := BuildEnvelope(sampleEvent())
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if envelope.EnvelopeVersion != "1.0" || envelope.Producer != "financial-controls" {
		t.Fatalf("envelope header: %+v", envelope)
	}
	if envelope.EventType != "cvff.disbursement.v1" {
		t.Fatalf("event type = %s", envelope.EventType)
	}
	if envelope.EventID != "6f1a2b3c-0000-4000-8000-000000000001" || envelope.CorrelationID != envelope.EventID {
		t.Fatalf("ids: %+v", envelope)
	}
	if envelope.OccurredAt != "2026-08-28T12:00:00Z" {
		t.Fatalf("occurredAt = %s", envelope.OccurredAt)
	}
	if envelope.FHIR.ResourceType != "Bundle" || envelope.FHIR.Type != "message" || len(envelope.FHIR.Entry) != 1 {
		t.Fatalf("fhir: %+v", envelope.FHIR)
	}
	resource, ok := envelope.FHIR.Entry[0].Resource.(map[string]any)
	if !ok {
		t.Fatalf("resource type %T", envelope.FHIR.Entry[0].Resource)
	}
	if resource["internalEventType"] != "financial_intent.approved" || resource["subjectId"] != "intent-001" {
		t.Fatalf("resource: %+v", resource)
	}
	if envelope.Provenance.PrincipalID != "kc-maker" || envelope.Provenance.LedgerCommitHash != "intent-001" {
		t.Fatalf("provenance: %+v", envelope.Provenance)
	}
	if len(envelope.Provenance.Signature) != 64 {
		t.Fatalf("signature = %q", envelope.Provenance.Signature)
	}
	if envelope.Classification != "FIDUCIARY_SEGREGATED" {
		t.Fatalf("classification = %s", envelope.Classification)
	}
	// Deterministic signature over the payload.
	again, err := BuildEnvelope(sampleEvent())
	if err != nil {
		t.Fatalf("rebuild envelope: %v", err)
	}
	if again.Provenance.Signature != envelope.Provenance.Signature {
		t.Fatal("signature is not deterministic")
	}
}

func TestBuildEnvelopeFailsClosed(t *testing.T) {
	event := sampleEvent()
	event.EventType = "unknown.type"
	if _, err := BuildEnvelope(event); err == nil {
		t.Fatal("unknown event type accepted")
	}
	event = sampleEvent()
	event.Payload = json.RawMessage(`[1,2]`)
	if _, err := BuildEnvelope(event); err == nil {
		t.Fatal("non-object payload accepted")
	}
	event = sampleEvent()
	event.Payload = json.RawMessage(`{broken`)
	if _, err := BuildEnvelope(event); err == nil {
		t.Fatal("malformed payload accepted")
	}
	event = sampleEvent()
	event.EventID = ""
	if _, err := BuildEnvelope(event); err == nil {
		t.Fatal("missing event ID accepted")
	}
}

type fakeSource struct {
	events  []Event
	marked  []string
	readErr error
	markErr error
}

func (source *fakeSource) Unpublished(context.Context, int) ([]Event, error) {
	if source.readErr != nil {
		return nil, source.readErr
	}
	return source.events, nil
}

func (source *fakeSource) MarkPublished(_ context.Context, event Event) error {
	if source.markErr != nil {
		return source.markErr
	}
	source.marked = append(source.marked, event.EventID)
	return nil
}

type fakeProducer struct {
	keys   []string
	values [][]byte
	err    error
}

func (producer *fakeProducer) Publish(_ context.Context, key, value []byte) error {
	if producer.err != nil {
		return producer.err
	}
	producer.keys = append(producer.keys, string(key))
	producer.values = append(producer.values, value)
	return nil
}

func TestDrainPublishesWithIdempotentKeys(t *testing.T) {
	source := &fakeSource{events: []Event{sampleEvent()}}
	producer := &fakeProducer{}
	published, err := Drain(context.Background(), source, producer, 100)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if published != 1 || len(producer.keys) != 1 {
		t.Fatalf("published = %d, keys = %v", published, producer.keys)
	}
	if producer.keys[0] != "6f1a2b3c-0000-4000-8000-000000000001" {
		t.Fatalf("key = %s", producer.keys[0])
	}
	var envelope Envelope
	if err := json.Unmarshal(producer.values[0], &envelope); err != nil {
		t.Fatalf("published value is not an envelope: %v", err)
	}
	if envelope.Classification != "FIDUCIARY_SEGREGATED" {
		t.Fatalf("classification = %s", envelope.Classification)
	}
	if len(source.marked) != 1 || source.marked[0] != producer.keys[0] {
		t.Fatalf("marked = %v", source.marked)
	}
}

func TestDrainFailsClosedOnProducerError(t *testing.T) {
	source := &fakeSource{events: []Event{sampleEvent()}}
	producer := &fakeProducer{err: errors.New("kafka unavailable")}
	published, err := Drain(context.Background(), source, producer, 100)
	if err == nil || !strings.Contains(err.Error(), "kafka unavailable") {
		t.Fatalf("drain error = %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d on failure", published)
	}
	if len(source.marked) != 0 {
		t.Fatal("event marked published despite producer failure")
	}
}

func TestDrainPartialFailureReportsCount(t *testing.T) {
	second := sampleEvent()
	second.EventID = "6f1a2b3c-0000-4000-8000-000000000002"
	source := &fakeSource{events: []Event{sampleEvent(), second}, markErr: errors.New("db write failed")}
	producer := &fakeProducer{}
	published, err := Drain(context.Background(), source, producer, 100)
	if err == nil {
		t.Fatal("mark failure swallowed")
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0 (mark failed for first)", published)
	}
}

func TestDrainValidatesInputs(t *testing.T) {
	if _, err := Drain(context.Background(), nil, &fakeProducer{}, 1); err == nil {
		t.Fatal("nil source accepted")
	}
	if _, err := Drain(context.Background(), &fakeSource{}, nil, 1); err == nil {
		t.Fatal("nil producer accepted")
	}
	if _, err := Drain(context.Background(), &fakeSource{}, &fakeProducer{}, 0); err == nil {
		t.Fatal("zero batch accepted")
	}
	if _, err := Drain(context.Background(), &fakeSource{readErr: errors.New("db down")}, &fakeProducer{}, 1); err == nil {
		t.Fatal("read error swallowed")
	}
}

func TestKafkaProducerFailsClosed(t *testing.T) {
	if _, err := NewKafkaProducer("", "topic"); err == nil {
		t.Fatal("empty brokers accepted")
	}
	if _, err := NewKafkaProducer("localhost:9092,,broker:9092", "topic"); err == nil {
		t.Fatal("empty broker address accepted")
	}
	if _, err := NewKafkaProducer("localhost:9092", "  "); err == nil {
		t.Fatal("empty topic accepted")
	}
	producer, err := NewKafkaProducer("localhost:9092", "cvff.events")
	if err != nil {
		t.Fatalf("valid producer rejected: %v", err)
	}
	defer producer.Close()
	if err := producer.Publish(context.Background(), nil, []byte("{}")); err == nil {
		t.Fatal("empty key accepted")
	}
}
