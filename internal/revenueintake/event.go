// Package revenueintake consumes the port-interoperability tariff engine's
// signed revenue assessment events from finance.revenue-assessments.v1 and
// lands them in the financial-controls reconciliation pipeline.
//
// Wire contract (read from the producer at blueeconomy-port-interoperability
// internal/events + internal/tariff @ phase7/w-feat6): the message value is
// the platform envelope v1.0 (envelopeVersion/eventId/eventType/occurredAt/
// producer/correlationId/classification/fhir/provenance). The provenance
// signature is a JWS compact serialization (EdDSA/Ed25519) over the
// RFC 8785 JCS-canonical envelope with the signature field excluded; the
// kid identifies the signing authority key. The FHIR message bundle carries
// the assessment as a domain-payload extension (JSON string) plus flat
// scalar extensions (domain, schedule-id, currency, total-minor,
// call-reference).
//
// Fail-closed posture: untrusted kids, missing/malformed signatures,
// canonical-payload mismatches and signature failures are all rejected and
// NEVER landed. Verification is against env-only trusted key material.
// Authentic events whose assessment payload cannot be deterministically
// mapped onto a recon leg are still recorded (with mapping_error) and
// surface as UNMATCHED_STATEMENT-class recon items — amounts are never
// guessed.
package revenueintake

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// Topic is the binding platform contract topic.
	Topic = "finance.revenue-assessments.v1"
	// EventTypeAssessmentIssued is the only event type this consumer lands.
	EventTypeAssessmentIssued = "revenue.assessment_issued"
	// EnvelopeVersion is the binding platform envelope version.
	EnvelopeVersion = "1.0"
	// Producer is the expected platform producer identity.
	Producer = "s1-port-interoperability"

	extensionPrefix        = "https://blueeconomy.gov.ng/fhir/StructureDefinition/"
	extensionDomainPayload = extensionPrefix + "domain-payload"
)

var (
	// ErrMalformed rejects a message that is not a well-formed v1.0 event.
	ErrMalformed = errors.New("revenue intake event is not a well-formed v1.0 envelope")
	// ErrUnsupported rejects well-formed envelopes this consumer does not land.
	ErrUnsupported = errors.New("revenue intake event is not an assessment issuance")
)

// AssessmentEvent is one verified revenue assessment event.
type AssessmentEvent struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	OccurredAt    time.Time `json:"occurredAt"`
	Producer      string    `json:"producer"`
	CorrelationID string    `json:"correlationId"`
	SignerKeyID   string    `json:"-"`
	// Mapped assessment-side fields ('' / nil when unmappable).
	Domain        string
	CallReference string
	ScheduleID    string
	AssessmentID  string
	TotalMinor    *int64
	Currency      string
	// MappingError explains why an authentic event could not be mapped onto
	// a deterministic recon leg ('' = mapped assessment-side record).
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
		Resource struct {
			ResourceType string `json:"resourceType"`
			ID           string `json:"id"`
			Extension    []struct {
				URL         string `json:"url"`
				ValueString string `json:"valueString"`
			} `json:"extension"`
		} `json:"resource"`
	} `json:"entry"`
}

// assessmentPayload is the port-interoperability tariff.Assessment wire
// shape carried inside the domain-payload extension (JSON string). Only the
// recon-relevant fields are decoded; the rest stays in Payload.
type assessmentPayload struct {
	AssessmentID  string `json:"assessment_id"`
	Domain        string `json:"domain"`
	CallReference string `json:"call_reference"`
	ScheduleID    string `json:"schedule_id"`
	TotalMinor    *int64 `json:"total_minor"`
	Currency      string `json:"currency"`
}

// parseEvent decodes and validates the envelope structure and maps the
// assessment extensions. Verification (signature/trust) happens separately
// in Verifier; parseEvent never trusts its input.
func parseEvent(raw []byte) (AssessmentEvent, error) {
	if len(raw) == 0 || len(raw) > 4<<20 {
		return AssessmentEvent{}, fmt.Errorf("%w: empty or oversized message", ErrMalformed)
	}
	var view envelopeView
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&view); err != nil {
		return AssessmentEvent{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if view.EnvelopeVersion != EnvelopeVersion {
		return AssessmentEvent{}, fmt.Errorf("%w: envelopeVersion %q", ErrMalformed, view.EnvelopeVersion)
	}
	if strings.TrimSpace(view.EventID) == "" {
		return AssessmentEvent{}, fmt.Errorf("%w: eventId is required", ErrMalformed)
	}
	if view.EventType != EventTypeAssessmentIssued {
		return AssessmentEvent{}, fmt.Errorf("%w: eventType %q", ErrUnsupported, view.EventType)
	}
	if view.Producer != Producer {
		return AssessmentEvent{}, fmt.Errorf("%w: producer %q", ErrMalformed, view.Producer)
	}
	if view.OccurredAt.IsZero() || strings.TrimSpace(view.CorrelationID) == "" {
		return AssessmentEvent{}, fmt.Errorf("%w: occurredAt and correlationId are required", ErrMalformed)
	}
	event := AssessmentEvent{
		EventID:       view.EventID,
		EventType:     view.EventType,
		OccurredAt:    view.OccurredAt.UTC(),
		Producer:      view.Producer,
		CorrelationID: view.CorrelationID,
		Payload:       json.RawMessage(raw),
	}
	event.Domain, event.CallReference, event.ScheduleID, event.AssessmentID,
		event.TotalMinor, event.Currency, event.MappingError = mapAssessment(view.FHIR)
	return event, nil
}

// mapAssessment extracts the recon-relevant assessment fields from the FHIR
// bundle. The domain-payload extension (full assessment JSON) is the source
// of truth; the flat scalar extensions must agree when both are present.
// Any ambiguity is reported as a mapping error — the recon pipeline then
// surfaces the event as an UNMATCHED_STATEMENT-class item rather than
// guessing a money amount.
func mapAssessment(fhir json.RawMessage) (domain, callReference, scheduleID, assessmentID string, totalMinor *int64, currency, mappingError string) {
	fail := func(reason string) (string, string, string, string, *int64, string, string) {
		return "", "", "", "", nil, "", reason
	}
	var bundle fhirBundleView
	if err := json.Unmarshal(fhir, &bundle); err != nil {
		return fail("FHIR bundle does not decode")
	}
	if bundle.ResourceType != "Bundle" || bundle.Type != "message" || len(bundle.Entry) != 1 {
		return fail("FHIR bundle is not a single-entry message")
	}
	resource := bundle.Entry[0].Resource
	if resource.ResourceType != "Basic" {
		return fail("FHIR entry resource is not Basic")
	}
	flat := map[string]string{}
	var payloadText string
	for _, extension := range resource.Extension {
		if extension.URL == extensionDomainPayload {
			payloadText = extension.ValueString
			continue
		}
		if strings.HasPrefix(extension.URL, extensionPrefix) {
			flat[strings.TrimPrefix(extension.URL, extensionPrefix)] = extension.ValueString
		}
	}
	if payloadText == "" {
		return fail("assessment domain-payload extension is missing")
	}
	var payload assessmentPayload
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		return fail("assessment domain-payload is not valid JSON")
	}
	domain = payload.Domain
	callReference = payload.CallReference
	scheduleID = payload.ScheduleID
	assessmentID = payload.AssessmentID
	totalMinor = payload.TotalMinor
	currency = payload.Currency
	// Cross-check the flat extensions against the payload (both come from the
	// same signed envelope; a disagreement means the mapping is ambiguous).
	if value, ok := flat["call-reference"]; ok && value != callReference {
		return fail("call-reference extension disagrees with the assessment payload")
	}
	if value, ok := flat["domain"]; ok && value != domain {
		return fail("domain extension disagrees with the assessment payload")
	}
	if value, ok := flat["currency"]; ok && value != currency {
		return fail("currency extension disagrees with the assessment payload")
	}
	if value, ok := flat["total-minor"]; ok {
		var flatTotal int64
		if _, err := fmt.Sscan(value, &flatTotal); err != nil || totalMinor == nil || flatTotal != *totalMinor {
			return fail("total-minor extension disagrees with the assessment payload")
		}
	}
	if callReference == "" || len(callReference) > 64 {
		return fail("assessment call_reference is missing or oversized")
	}
	if domain != "OFFSHORE_TERMINAL" && domain != "CRUISE_DUES" {
		return fail("assessment domain is not a known revenue domain")
	}
	if totalMinor == nil || *totalMinor < 0 {
		return fail("assessment total_minor is missing or negative")
	}
	if currency != "USD" && currency != "NGN" {
		return fail("assessment currency is not USD or NGN")
	}
	return domain, callReference, scheduleID, assessmentID, totalMinor, currency, ""
}
