package stampsintake

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// fixtureKey is the Ed25519 key the test envelopes are signed with
// (kid "blueeconomy-tax-stamps-0", the producer's canonical kid shape).
func fixtureSeed() []byte {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return seed
}

func fixtureVerifier(t *testing.T) *Verifier {
	t.Helper()
	public := ed25519.NewKeyFromSeed(fixtureSeed()).Public().(ed25519.PublicKey)
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{"blueeconomy-tax-stamps-0": public})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

// sign builds a producer-compatible envelope: structural resource in the
// single-entry FHIR message Bundle, provenance JWS EdDSA over the
// JCS-canonical envelope minus the signature field.
func sign(t *testing.T, eventType string, resource map[string]any, key ed25519.PrivateKey, kid string) []byte {
	t.Helper()
	resource["@type"] = typeURLPrefix + resourceNameByEventType[eventType]
	env := map[string]any{
		"envelopeVersion": "1.0",
		"eventId":         "evt-11111111-2222-4333-8444-555555555555",
		"eventType":       eventType,
		"occurredAt":      "2026-08-01T10:15:30Z",
		"producer":        Producer,
		"correlationId":   "corr-1",
		"classification":  "CONFIDENTIAL",
		"fhir": map[string]any{
			"resourceType": "Bundle",
			"type":         "message",
			"bundleId":     "bdl-1",
			"entry": []any{
				map[string]any{"fullUrl": "urn:uuid:1", "resource": resource},
			},
		},
		"provenance": map[string]any{
			"principalId":   "sub-1",
			"principalRole": "excise-officer",
		},
	}
	canonical, err := envelope.JCS(env)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","kid":"` + kid + `"}`))
	payload := base64.RawURLEncoding.EncodeToString(canonical)
	signature := ed25519.Sign(key, []byte(header+"."+payload))
	env["provenance"].(map[string]any)["signature"] =
		header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(signature)
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fixtureKeyPair(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	return ed25519.NewKeyFromSeed(fixtureSeed())
}

func assessedResource() map[string]any {
	return map[string]any{
		"assessmentId":   "a1b2c3d4-0000-4000-8000-0000000000aa",
		"declarationRef": "DECL-2026-001",
		"consigneeTin":   "12345678-0001",
		"totalDutyKobo":  1250000,
		"stampsRequired": 500,
		"riskTier":       "LOW",
	}
}

func TestSignedAssessedEventVerifies(t *testing.T) {
	raw := sign(t, EventTypeAssessed, assessedResource(), fixtureKeyPair(t), "blueeconomy-tax-stamps-0")
	event, err := fixtureVerifier(t).VerifyAndParse(raw, "stamps.assessed")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if event.EventType != EventTypeAssessed || event.Producer != Producer {
		t.Fatalf("header: %+v", event)
	}
	if event.SignerKeyID != "blueeconomy-tax-stamps-0" {
		t.Fatalf("kid = %s", event.SignerKeyID)
	}
	if event.MappingError != "" {
		t.Fatalf("mapping error: %s", event.MappingError)
	}
	if event.AssessmentID != "a1b2c3d4-0000-4000-8000-0000000000aa" || event.DeclarationRef != "DECL-2026-001" {
		t.Fatalf("mapped: %+v", event)
	}
	if event.TotalDutyKobo == nil || *event.TotalDutyKobo != 1250000 {
		t.Fatalf("duty: %+v", event.TotalDutyKobo)
	}
	if event.Quantity == nil || *event.Quantity != 500 {
		t.Fatalf("quantity: %+v", event.Quantity)
	}
}

func TestSignedLifecycleEventsVerify(t *testing.T) {
	cases := []struct {
		eventType string
		topic     string
		resource  map[string]any
	}{
		{EventTypeApproved, "stamps.approved", map[string]any{"assessmentId": "a-1", "approvalsRequired": 3}},
		{EventTypeIssued, "stamps.issued", map[string]any{"batchId": "b-1", "assessmentId": "a-1", "categoryCode": "TBC", "quantity": 500, "merkleRoot": "ab"}},
		{EventTypeActivated, "stamps.activated", map[string]any{"batchId": "b-1", "activatedCount": 500}},
	}
	for _, tc := range cases {
		raw := sign(t, tc.eventType, tc.resource, fixtureKeyPair(t), "blueeconomy-tax-stamps-0")
		event, err := fixtureVerifier(t).VerifyAndParse(raw, tc.topic)
		if err != nil {
			t.Fatalf("%s verify: %v", tc.eventType, err)
		}
		if event.MappingError != "" {
			t.Fatalf("%s mapping error: %s", tc.eventType, event.MappingError)
		}
	}
}

func TestUntrustedKidRejected(t *testing.T) {
	_, rogue, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := sign(t, EventTypeAssessed, assessedResource(), rogue, "rogue-producer-0")
	if _, err := fixtureVerifier(t).VerifyAndParse(raw, "stamps.assessed"); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("err = %v, want ErrUntrustedKey", err)
	}
}

func TestUnsignedRejected(t *testing.T) {
	raw := sign(t, EventTypeAssessed, assessedResource(), fixtureKeyPair(t), "blueeconomy-tax-stamps-0")
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	generic["provenance"].(map[string]any)["signature"] = ""
	raw, _ = json.Marshal(generic)
	if _, err := fixtureVerifier(t).VerifyAndParse(raw, "stamps.assessed"); !errors.Is(err, ErrUntrustedKey) {
		t.Fatalf("err = %v, want ErrUntrustedKey", err)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	raw := sign(t, EventTypeAssessed, assessedResource(), fixtureKeyPair(t), "blueeconomy-tax-stamps-0")
	tampered := strings.Replace(string(raw), `"DECL-2026-001"`, `"DECL-2026-002"`, 1)
	if tampered == string(raw) {
		t.Fatal("tamper did not apply")
	}
	if _, err := fixtureVerifier(t).VerifyAndParse([]byte(tampered), "stamps.assessed"); !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v, want ErrSignature", err)
	}
}

func TestWrongTopicRejected(t *testing.T) {
	raw := sign(t, EventTypeAssessed, assessedResource(), fixtureKeyPair(t), "blueeconomy-tax-stamps-0")
	if _, err := fixtureVerifier(t).VerifyAndParse(raw, "stamps.issued"); !errors.Is(err, ErrWrongTopic) {
		t.Fatalf("err = %v, want ErrWrongTopic", err)
	}
	if _, err := fixtureVerifier(t).VerifyAndParse(raw, "trade.declarations.v1"); !errors.Is(err, ErrWrongTopic) {
		t.Fatalf("err = %v, want ErrWrongTopic", err)
	}
}

func TestUnsupportedEventTypeRejected(t *testing.T) {
	raw := sign(t, EventTypeAssessed, assessedResource(), fixtureKeyPair(t), "blueeconomy-tax-stamps-0")
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	generic["eventType"] = "stamps.voided.v1"
	raw, _ = json.Marshal(generic)
	if _, err := fixtureVerifier(t).VerifyAndParse(raw, "stamps.voided"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestMappingErrorRecordedNotGuessed(t *testing.T) {
	// Authentic, well-signed event whose money field is missing: lands with
	// a mapping error, never with a guessed amount.
	resource := assessedResource()
	delete(resource, "totalDutyKobo")
	raw := sign(t, EventTypeAssessed, resource, fixtureKeyPair(t), "blueeconomy-tax-stamps-0")
	event, err := fixtureVerifier(t).VerifyAndParse(raw, "stamps.assessed")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if event.MappingError == "" {
		t.Fatal("expected a mapping error")
	}
	if event.TotalDutyKobo != nil {
		t.Fatalf("amount must never be guessed: %+v", event.TotalDutyKobo)
	}
}

func TestNonIntegralMoneyMappingError(t *testing.T) {
	// A non-integral money value can never be guessed; the mapper reports a
	// mapping error. (The repo JCS encoder refuses to sign such a value, so
	// the mapper is exercised directly with the raw FHIR carriage.)
	fhir := []byte(`{"resourceType":"Bundle","type":"message","entry":[{"resource":{` +
		`"@type":"type.googleapis.com/blueeconomy.contracts.v1.TaxStampAssessed",` +
		`"assessmentId":"a-1","declarationRef":"D-1","totalDutyKobo":12.5,"stampsRequired":10}}]}`)
	_, _, _, duty, _, mappingError := mapResource(EventTypeAssessed, fhir)
	if mappingError == "" || duty != nil {
		t.Fatalf("expected mapping error, got duty=%v err=%q", duty, mappingError)
	}
}

func TestVerifierFromEnvFailClosed(t *testing.T) {
	t.Setenv(TrustedKeysEnv, "")
	if _, err := VerifierFromEnv(); err == nil {
		t.Fatal("expected fail-closed on missing trusted keys")
	}
	t.Setenv(TrustedKeysEnv, "not-a-pair")
	if _, err := VerifierFromEnv(); err == nil {
		t.Fatal("expected fail-closed on malformed trusted keys")
	}
	public := ed25519.NewKeyFromSeed(fixtureSeed()).Public().(ed25519.PublicKey)
	t.Setenv(TrustedKeysEnv, "blueeconomy-tax-stamps-0="+base64.RawURLEncoding.EncodeToString(public))
	if _, err := VerifierFromEnv(); err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}
}
