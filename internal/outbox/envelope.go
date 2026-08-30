// Package outbox drains the transactional outboxes to Kafka with the
// platform FHIR-aligned envelope, at-least-once delivery and idempotent keys.
// Every envelope carries an Ed25519 JWS provenance signature over the
// RFC 8785 canonical envelope; envelopes are never published unsigned.
package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// EnvelopeVersion is the binding platform envelope version.
	EnvelopeVersion = "1.0"
	// ProducerName identifies this service in every envelope.
	ProducerName = "financial-controls"
	// ClassificationFIDUCIARY marks segregated fiduciary funds events.
	ClassificationFIDUCIARY = "FIDUCIARY_SEGREGATED"
)

// Event is one unpublished outbox row from either transactional outbox.
type Event struct {
	EventID   string
	SubjectID string
	EventType string
	Payload   json.RawMessage
	CreatedAt time.Time
	table     outboxTable
}

// Envelope is the binding platform message envelope.
type Envelope struct {
	EnvelopeVersion string     `json:"envelopeVersion"`
	EventID         string     `json:"eventId"`
	EventType       string     `json:"eventType"`
	OccurredAt      string     `json:"occurredAt"`
	Producer        string     `json:"producer"`
	CorrelationID   string     `json:"correlationId"`
	FHIR            FHIRBundle `json:"fhir"`
	Provenance      Provenance `json:"provenance"`
	Classification  string     `json:"classification"`
}

// FHIRBundle is the FHIR-aligned message wrapper.
type FHIRBundle struct {
	ResourceType string      `json:"resourceType"`
	Type         string      `json:"type"`
	Entry        []FHIREntry `json:"entry"`
}

// FHIREntry wraps one domain resource.
type FHIREntry struct {
	Resource any `json:"resource"`
}

// Provenance binds the envelope to the deciding principal and ledger commit.
type Provenance struct {
	PrincipalID      string `json:"principalId"`
	PrincipalRole    string `json:"principalRole"`
	Signature        string `json:"signature"`
	LedgerCommitHash string `json:"ledgerCommitHash"`
}

// envelopeEventTypes maps internal outbox event types to envelope event types.
// Unknown types fail closed.
var envelopeEventTypes = map[string]string{
	"financial_intent.created":       "cvff.disbursement.v1",
	"financial_intent.approved":      "cvff.disbursement.v1",
	"financial_intent.state_changed": "cvff.disbursement.v1",
	"cvff.application.submitted":     "cvff.disbursement.v1",
	"cvff.underwriting_started":      "cvff.disbursement.v1",
	"cvff.decision.recorded":         "cvff.disbursement.v1",
	"cvff.sla_escalated":             "cvff.disbursement.v1",
	"cvff.disbursed":                 "cvff.disbursement.v1",
	"cvff.audited":                   "cvff.disbursement.v1",
	"cvff.reconciliation_required":   "cvff.disbursement.v1",
	"cvff.roles_assigned":            "cvff.disbursement.v1",
	"cvff.reconciliation_resolved":   "cvff.disbursement.v1",
	// Revenue-assurance chain (W-FEAT-7 / W-CLOSE-FC): every revenue_outbox
	// event publishes onto the finance revenue contract topic.
	"revenue.debit_note.created":       "finance.revenue.v1",
	"revenue.debit_note.transitioned":  "finance.revenue.v1",
	"revenue.split_rule.created":       "finance.revenue.v1",
	"revenue.split_rule.activated":     "finance.revenue.v1",
	"revenue.remittance_advice.issued": "finance.revenue.v1",
	"revenue.settlement.recorded":      "finance.revenue.v1",
	"revenue.statement.ingested":       "finance.revenue.v1",
	"revenue.recon.run_completed":      "finance.revenue.v1",
	"revenue.recon.exception_raised":   "finance.revenue.v1",
	"revenue.recon.exception_resolved": "finance.revenue.v1",
}

// BuildEnvelope maps one outbox event to the platform envelope. It fails
// closed on unknown event types and malformed payloads.
func BuildEnvelope(event Event) (Envelope, error) {
	if event.EventID == "" || event.SubjectID == "" || event.EventType == "" || len(event.Payload) == 0 {
		return Envelope{}, errors.New("outbox event identifiers and payload are required")
	}
	eventType, ok := envelopeEventTypes[event.EventType]
	if !ok {
		return Envelope{}, fmt.Errorf("outbox event type %q has no envelope mapping", event.EventType)
	}
	var resource map[string]any
	if err := json.Unmarshal(event.Payload, &resource); err != nil {
		return Envelope{}, fmt.Errorf("decode outbox payload: %w", err)
	}
	if resource == nil {
		return Envelope{}, errors.New("outbox payload is not a JSON object")
	}
	resource["internalEventType"] = event.EventType
	resource["subjectId"] = event.SubjectID
	// The provenance signature is deliberately empty here: SignEnvelope fills
	// it with the Ed25519 JWS over the canonical envelope before publication.
	return Envelope{
		EnvelopeVersion: EnvelopeVersion,
		EventID:         event.EventID,
		EventType:       eventType,
		OccurredAt:      event.CreatedAt.UTC().Format(time.RFC3339),
		Producer:        ProducerName,
		CorrelationID:   event.EventID,
		FHIR: FHIRBundle{
			ResourceType: "Bundle",
			Type:         "message",
			Entry:        []FHIREntry{{Resource: resource}},
		},
		Provenance: Provenance{
			PrincipalID:      stringField(resource, "maker", "principal_id", "PrincipalID"),
			PrincipalRole:    stringField(resource, "role", "state", "State"),
			LedgerCommitHash: stringField(resource, "intent_id", "application_id", "fee_transfer_id"),
		},
		Classification: ClassificationFIDUCIARY,
	}, nil
}

func stringField(resource map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := resource[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// IdempotentKey returns the stable Kafka message key for at-least-once
// republish safety.
func IdempotentKey(event Event) string { return event.EventID }
