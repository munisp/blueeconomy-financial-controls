// Package stampsintake consumes the tax-stamps service's signed excise
// stamp lifecycle events from the stamps.* Kafka topics and lands them in
// the financial-controls revenue pipeline.
//
// Wire contract (read from the producer at blueeconomy-tax-stamps
// src/taxstamps/events/envelope.py + services/outbox.py @ phase7/remediation):
// the message value is the platform envelope v1.0 (envelopeVersion/eventId/
// eventType/occurredAt/producer/correlationId/classification/fhir/provenance).
// The provenance signature is a JWS compact serialization (EdDSA/Ed25519)
// over the RFC 8785 JCS-canonical envelope with the signature field
// excluded; the kid identifies the tax-stamps signing key. The FHIR message
// bundle's single entry carries the structural event resource directly
// (@type type.googleapis.com/blueeconomy.contracts.v1.<EventResource>).
//
// Fail-closed posture: untrusted kids, missing/malformed signatures,
// canonical-payload mismatches, wrong topics and signature failures are all
// rejected and NEVER landed. Verification is against env-only trusted key
// material (STAMPS_INTAKE_TRUSTED_KEYS). Authentic events whose resource
// cannot be deterministically mapped are still recorded (with
// mapping_error) — amounts are never guessed.
package stampsintake

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// EnvelopeVersion is the binding platform envelope version.
	EnvelopeVersion = "1.0"
	// Producer is the expected platform producer identity.
	Producer = "blueeconomy-tax-stamps"

	// EventTypeAssessed is stamps.assessed.v1 (topic stamps.assessed).
	EventTypeAssessed = "stamps.assessed.v1"
	// EventTypeApproved is stamps.approved.v1 (topic stamps.approved).
	EventTypeApproved = "stamps.approved.v1"
	// EventTypeIssued is stamps.issued.v1 (topic stamps.issued).
	EventTypeIssued = "stamps.issued.v1"
	// EventTypeActivated is stamps.activated.v1 (topic stamps.activated).
	EventTypeActivated = "stamps.activated.v1"

	typeURLPrefix = "type.googleapis.com/blueeconomy.contracts.v1."
)

// TopicByEventType is the binding event-type/topic contract (mirrors the
// producer's TOPIC_BY_EVENT).
var TopicByEventType = map[string]string{
	EventTypeAssessed:  "stamps.assessed",
	EventTypeApproved:  "stamps.approved",
	EventTypeIssued:    "stamps.issued",
	EventTypeActivated: "stamps.activated",
}

// resourceNameByEventType binds each event type to its structural resource
// name (mirrors the producer's _EVENT_RESOURCES).
var resourceNameByEventType = map[string]string{
	EventTypeAssessed:  "TaxStampAssessed",
	EventTypeApproved:  "TaxStampAssessmentApproved",
	EventTypeIssued:    "TaxStampBatchIssued",
	EventTypeActivated: "TaxStampBatchActivated",
}

// Topics is the consumed topic set in deterministic order.
var Topics = []string{"stamps.assessed", "stamps.approved", "stamps.issued", "stamps.activated"}

var (
	// ErrMalformed rejects a message that is not a well-formed v1.0 event.
	ErrMalformed = errors.New("stamps intake event is not a well-formed v1.0 envelope")
	// ErrUnsupported rejects well-formed envelopes this consumer does not land.
	ErrUnsupported = errors.New("stamps intake event is not a landed stamps lifecycle event")
	// ErrWrongTopic rejects an event whose topic does not match its event type.
	ErrWrongTopic = errors.New("stamps intake event arrived on the wrong topic")
)

// StampEvent is one verified stamps lifecycle event.
type StampEvent struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	Topic         string    `json:"-"`
	OccurredAt    time.Time `json:"occurredAt"`
	Producer      string    `json:"producer"`
	CorrelationID string    `json:"correlationId"`
	SignerKeyID   string    `json:"-"`
	// Mapped resource-side fields ('' / nil when unmappable).
	AssessmentID   string
	DeclarationRef string
	BatchID        string
	TotalDutyKobo  *int64
	Quantity       *int64
	// MappingError explains why an authentic event could not be mapped
	// deterministically ('' = mapped record); amounts are never guessed.
	MappingError string
	// Payload is the full verified envelope as received.
	Payload json.RawMessage `json:"-"`
}

// envelopeView mirrors the platform envelope for decoding. The signature
// lives only in the verifier's scope; the landed payload keeps it.
type envelopeView struct {
	EnvelopeVersion string          `json:"envelopeVersion"`
	EventID         string          `json:"eventId"`
	EventType       string          `json:"eventType"`
	OccurredAt      time.Time       `json:"occurredAt"`
	Producer        string          `json:"producer"`
	CorrelationID   string          `json:"correlationId"`
	Classification  string          `json:"classification"`
	FHIR            json.RawMessage `json:"fhir"`
	Provenance      struct {
		PrincipalID      string `json:"principalId"`
		PrincipalRole    string `json:"principalRole"`
		Signature        string `json:"signature"`
		LedgerCommitHash string `json:"ledgerCommitHash,omitempty"`
	} `json:"provenance"`
}

type fhirBundleView struct {
	ResourceType string `json:"resourceType"`
	Type         string `json:"type"`
	Entry        []struct {
		Resource json.RawMessage `json:"resource"`
	} `json:"entry"`
}

// stampResource is the union of the structural stamps lifecycle resources.
// Only the intake-relevant fields are decoded; the full resource stays in
// Payload. Numbers decode as json.Number so a float round-trip can never
// alter a money or quantity value.
type stampResource struct {
	TypeURL        string      `json:"@type"`
	AssessmentID   string      `json:"assessmentId"`
	DeclarationRef string      `json:"declarationRef"`
	BatchID        string      `json:"batchId"`
	TotalDutyKobo  json.Number `json:"totalDutyKobo"`
	StampsRequired json.Number `json:"stampsRequired"`
	Quantity       json.Number `json:"quantity"`
	ActivatedCount json.Number `json:"activatedCount"`
	ApprovalsReq   json.Number `json:"approvalsRequired"`
	CategoryCode   string      `json:"categoryCode"`
}

// parseEvent decodes and validates the envelope structure, checks the
// topic/event-type contract and maps the resource. Verification
// (signature/trust) happens separately in Verifier; parseEvent never trusts
// its input.
func parseEvent(raw []byte, topic string) (StampEvent, error) {
	if len(raw) == 0 || len(raw) > 4<<20 {
		return StampEvent{}, fmt.Errorf("%w: empty or oversized message", ErrMalformed)
	}
	var view envelopeView
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&view); err != nil {
		return StampEvent{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if view.EnvelopeVersion != EnvelopeVersion {
		return StampEvent{}, fmt.Errorf("%w: envelopeVersion %q", ErrMalformed, view.EnvelopeVersion)
	}
	if strings.TrimSpace(view.EventID) == "" {
		return StampEvent{}, fmt.Errorf("%w: eventId is required", ErrMalformed)
	}
	expectedTopic, landed := TopicByEventType[view.EventType]
	if !landed {
		return StampEvent{}, fmt.Errorf("%w: eventType %q", ErrUnsupported, view.EventType)
	}
	if topic != expectedTopic {
		return StampEvent{}, fmt.Errorf("%w: eventType %q on topic %q, expected %q",
			ErrWrongTopic, view.EventType, topic, expectedTopic)
	}
	if view.Producer != Producer {
		return StampEvent{}, fmt.Errorf("%w: producer %q", ErrMalformed, view.Producer)
	}
	if view.OccurredAt.IsZero() || strings.TrimSpace(view.CorrelationID) == "" {
		return StampEvent{}, fmt.Errorf("%w: occurredAt and correlationId are required", ErrMalformed)
	}
	event := StampEvent{
		EventID:       view.EventID,
		EventType:     view.EventType,
		Topic:         topic,
		OccurredAt:    view.OccurredAt.UTC(),
		Producer:      view.Producer,
		CorrelationID: view.CorrelationID,
		Payload:       json.RawMessage(raw),
	}
	event.AssessmentID, event.DeclarationRef, event.BatchID,
		event.TotalDutyKobo, event.Quantity, event.MappingError = mapResource(view.EventType, view.FHIR)
	return event, nil
}

// mapResource extracts the intake-relevant fields from the structural FHIR
// entry resource. Any ambiguity is reported as a mapping error — the event
// is still landed (authentic) but never guessed into a money record.
func mapResource(eventType string, fhir json.RawMessage) (assessmentID, declarationRef, batchID string, totalDutyKobo, quantity *int64, mappingError string) {
	fail := func(reason string) (string, string, string, *int64, *int64, string) {
		return "", "", "", nil, nil, reason
	}
	var bundle fhirBundleView
	if err := json.Unmarshal(fhir, &bundle); err != nil {
		return fail("FHIR bundle does not decode")
	}
	if bundle.ResourceType != "Bundle" || bundle.Type != "message" || len(bundle.Entry) != 1 {
		return fail("FHIR bundle is not a single-entry message")
	}
	decoder := json.NewDecoder(strings.NewReader(string(bundle.Entry[0].Resource)))
	var resource stampResource
	if err := decoder.Decode(&resource); err != nil {
		return fail("entry resource does not decode")
	}
	expectedName := resourceNameByEventType[eventType]
	if !strings.HasSuffix(resource.TypeURL, typeURLPrefix+expectedName) {
		return fail(fmt.Sprintf("resource @type %q does not name %s", resource.TypeURL, expectedName))
	}
	toInt := func(n json.Number) (int64, bool) {
		if n == "" {
			return 0, false
		}
		value, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return value, true
	}
	assessmentID = resource.AssessmentID
	declarationRef = resource.DeclarationRef
	batchID = resource.BatchID
	switch eventType {
	case EventTypeAssessed:
		duty, ok := toInt(resource.TotalDutyKobo)
		if !ok || duty < 0 {
			return fail("assessed totalDutyKobo is missing, negative or non-integral")
		}
		stamps, ok := toInt(resource.StampsRequired)
		if !ok || stamps < 0 {
			return fail("assessed stampsRequired is missing, negative or non-integral")
		}
		if assessmentID == "" || declarationRef == "" {
			return fail("assessed assessmentId/declarationRef are required")
		}
		totalDutyKobo = &duty
		quantity = &stamps
	case EventTypeApproved:
		if assessmentID == "" {
			return fail("approved assessmentId is required")
		}
		if _, ok := toInt(resource.ApprovalsReq); !ok {
			return fail("approved approvalsRequired is missing or non-integral")
		}
	case EventTypeIssued:
		qty, ok := toInt(resource.Quantity)
		if !ok || qty <= 0 {
			return fail("issued quantity is missing, non-positive or non-integral")
		}
		if batchID == "" || assessmentID == "" {
			return fail("issued batchId/assessmentId are required")
		}
		quantity = &qty
	case EventTypeActivated:
		count, ok := toInt(resource.ActivatedCount)
		if !ok || count < 0 {
			return fail("activated activatedCount is missing, negative or non-integral")
		}
		if batchID == "" {
			return fail("activated batchId is required")
		}
		quantity = &count
	}
	return assessmentID, declarationRef, batchID, totalDutyKobo, quantity, ""
}
