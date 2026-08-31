package revenueintake

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

// TrustedKeysEnv carries the trusted assessment-authority keys as a
// comma-separated list of kid=base64url-Ed25519-public-key pairs. It is the
// ONLY source of verification key material (env-only secrets); an absent or
// malformed value fails closed at boot.
const TrustedKeysEnv = "REVENUE_INTAKE_TRUSTED_KEYS"

const signingAlgorithm = "EdDSA"

var (
	// ErrUntrustedKey rejects a JWS whose kid is not in the trusted set, and
	// a missing signature outright.
	ErrUntrustedKey = errors.New("revenue assessment signer key is not trusted")
	// ErrSignature rejects a JWS whose EdDSA signature does not verify or
	// whose payload is not the canonical envelope.
	ErrSignature = errors.New("revenue assessment signature does not verify")
)

// Verifier checks assessment-event provenance signatures against the
// trusted authority key set.
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewVerifier fails closed on an empty trust set or malformed key.
func NewVerifier(trusted map[string]ed25519.PublicKey) (*Verifier, error) {
	if len(trusted) == 0 {
		return nil, errors.New("revenue intake trusted key set is required (fail-closed)")
	}
	keys := make(map[string]ed25519.PublicKey, len(trusted))
	for kid, key := range trusted {
		if strings.TrimSpace(kid) == "" || len(kid) > 128 {
			return nil, errors.New("revenue intake trusted key id is invalid")
		}
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("revenue intake trusted key %q is not an Ed25519 public key", kid)
		}
		keys[kid] = ed25519.PublicKey(append([]byte(nil), key...))
	}
	return &Verifier{keys: keys}, nil
}

// VerifierFromEnv loads the trusted key set from TrustedKeysEnv. It fails
// closed when the variable is absent or any entry is malformed — an
// unverifiable intake pipeline must never start.
func VerifierFromEnv() (*Verifier, error) {
	raw := strings.TrimSpace(os.Getenv(TrustedKeysEnv))
	if raw == "" {
		return nil, fmt.Errorf("%s must be set (kid=base64url-public-key pairs, comma-separated)", TrustedKeysEnv)
	}
	trusted := map[string]ed25519.PublicKey{}
	for _, entry := range strings.Split(raw, ",") {
		kid, encoded, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found || kid == "" {
			return nil, fmt.Errorf("%s entry %q is not kid=base64url-public-key", TrustedKeysEnv, entry)
		}
		key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s key %q is not a base64url Ed25519 public key", TrustedKeysEnv, kid)
		}
		trusted[kid] = ed25519.PublicKey(key)
	}
	return NewVerifier(trusted)
}

// VerifyAndParse verifies the provenance JWS of one raw event message and
// returns the parsed event. Every failure mode fails closed: nothing is
// returned that may be landed.
//
// The scheme mirrors the producer (port-interoperability internal/events):
// EdDSA over the JCS-canonical envelope with provenance.signature excluded;
// the payload segment must reproduce byte-for-byte so no reformatting can
// alter the signed content.
func (verifier *Verifier) VerifyAndParse(raw []byte) (AssessmentEvent, error) {
	if verifier == nil {
		return AssessmentEvent{}, errors.New("revenue intake verifier is required (fail-closed)")
	}
	event, err := parseEvent(raw)
	if err != nil {
		return AssessmentEvent{}, err
	}
	kid, err := verifier.verifySignature(raw)
	if err != nil {
		return AssessmentEvent{}, err
	}
	event.SignerKeyID = kid
	return event, nil
}

// verifySignature returns the verified kid or the rejection reason.
func (verifier *Verifier) verifySignature(raw []byte) (string, error) {
	var view envelopeView
	if err := json.Unmarshal(raw, &view); err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	signature := view.Provenance.Signature
	if strings.TrimSpace(signature) == "" {
		return "", fmt.Errorf("%w: provenance signature is missing", ErrUntrustedKey)
	}
	parts := strings.Split(signature, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: provenance signature is not a JWS compact serialization", ErrSignature)
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("%w: decode JWS protected header", ErrSignature)
	}
	var parsed struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(header, &parsed); err != nil {
		return "", fmt.Errorf("%w: parse JWS protected header", ErrSignature)
	}
	if parsed.Algorithm != signingAlgorithm {
		return "", fmt.Errorf("%w: JWS alg %q is not %q", ErrSignature, parsed.Algorithm, signingAlgorithm)
	}
	key, trusted := verifier.keys[parsed.KeyID]
	if !trusted {
		return "", fmt.Errorf("%w: kid %q", ErrUntrustedKey, parsed.KeyID)
	}
	canonical, err := canonicalPayload(raw)
	if err != nil {
		return "", err
	}
	if base64.RawURLEncoding.EncodeToString(canonical) != parts[1] {
		return "", fmt.Errorf("%w: envelope does not match the signed canonical payload", ErrSignature)
	}
	signatureBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("%w: decode JWS signature", ErrSignature)
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signatureBytes) {
		return "", fmt.Errorf("%w: Ed25519 verification failed", ErrSignature)
	}
	return parsed.KeyID, nil
}

// canonicalPayload renders the full envelope minus provenance.signature as
// JCS-canonical (RFC 8785) JSON — byte-identical to the producer's signed
// payload. Numbers decode as literals so no float64 round-trip can alter
// the signed bytes.
func canonicalPayload(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic map[string]any
	if err := decoder.Decode(&generic); err != nil {
		return nil, fmt.Errorf("%w: decode envelope for verification", ErrMalformed)
	}
	provenance, ok := generic["provenance"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: provenance block is missing", ErrMalformed)
	}
	delete(provenance, "signature")
	canonical, err := envelope.JCS(generic)
	if err != nil {
		return nil, fmt.Errorf("%w: envelope payload is not JCS-canonicalizable", ErrMalformed)
	}
	return canonical, nil
}
