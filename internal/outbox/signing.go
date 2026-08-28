package outbox

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Envelope signing configuration. The signing key is delivered through the
// environment (ExternalSecret in GitOps) and the process refuses to start
// without a valid key and key epoch: an unsigned fiduciary envelope is never
// publishable.
const (
	// EnvSigningPrivateKey carries the Ed25519 private key as PKCS#8 PEM or
	// base64 (standard or URL encoding, 64-byte private key or 32-byte seed).
	EnvSigningPrivateKey = "OUTBOX_SIGNING_PRIVATE_KEY"
	// EnvSigningKeyEpoch identifies the active key generation. The JWS key ID
	// is "financial-controls-<epoch>".
	EnvSigningKeyEpoch = "OUTBOX_SIGNING_KEY_EPOCH"
)

// SigningAlgorithm is the only approved envelope signature algorithm.
const SigningAlgorithm = "EdDSA"

var keyEpochPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

var (
	// ErrSigningKeyInvalid marks absent or malformed key material.
	ErrSigningKeyInvalid = errors.New("envelope signing key is absent or invalid")
	// ErrSignatureInvalid marks an envelope whose provenance signature does
	// not verify against the expected key and key ID.
	ErrSignatureInvalid = errors.New("envelope provenance signature is invalid")
)

// EnvelopeSigner signs platform envelopes with the service Ed25519 key.
// provenance.signature is a JWS compact serialization (EdDSA) over the
// RFC 8785 (JCS) canonical JSON of the full envelope excluding the signature
// field; the JWS protected header is {"alg":"EdDSA","kid":"financial-controls-<epoch>"}.
type EnvelopeSigner struct {
	privateKey ed25519.PrivateKey
	kid        string
}

// NewEnvelopeSigner binds a parsed Ed25519 private key to its key epoch and
// fails closed on any gap.
func NewEnvelopeSigner(privateKey ed25519.PrivateKey, keyEpoch string) (*EnvelopeSigner, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: Ed25519 private key is required", ErrSigningKeyInvalid)
	}
	if !keyEpochPattern.MatchString(keyEpoch) {
		return nil, fmt.Errorf("%w: key epoch %q is not canonical identifier text", ErrSigningKeyInvalid, keyEpoch)
	}
	return &EnvelopeSigner{privateKey: privateKey, kid: ProducerName + "-" + keyEpoch}, nil
}

// NewEnvelopeSignerFromEnv loads the signing key and epoch from the
// environment, failing closed when either is absent or malformed.
func NewEnvelopeSignerFromEnv(lookup func(string) string) (*EnvelopeSigner, error) {
	if lookup == nil {
		return nil, fmt.Errorf("%w: environment lookup is required", ErrSigningKeyInvalid)
	}
	material := lookup(EnvSigningPrivateKey)
	epoch := lookup(EnvSigningKeyEpoch)
	if strings.TrimSpace(material) == "" {
		return nil, fmt.Errorf("%w: %s is required", ErrSigningKeyInvalid, EnvSigningPrivateKey)
	}
	if strings.TrimSpace(epoch) == "" {
		return nil, fmt.Errorf("%w: %s is required", ErrSigningKeyInvalid, EnvSigningKeyEpoch)
	}
	privateKey, err := ParseEnvelopeSigningKey(material)
	if err != nil {
		return nil, err
	}
	return NewEnvelopeSigner(privateKey, epoch)
}

// ParseEnvelopeSigningKey decodes PKCS#8 PEM or base64 Ed25519 key material
// (64-byte private key or 32-byte seed; standard or URL base64, padded or
// raw). Anything else is rejected.
func ParseEnvelopeSigningKey(material string) (ed25519.PrivateKey, error) {
	trimmed := strings.TrimSpace(material)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty key material", ErrSigningKeyInvalid)
	}
	if block, _ := pem.Decode([]byte(trimmed)); block != nil {
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: PEM block is not PKCS#8", ErrSigningKeyInvalid)
		}
		privateKey, ok := parsed.(ed25519.PrivateKey)
		if !ok || len(privateKey) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("%w: PKCS#8 key is not Ed25519", ErrSigningKeyInvalid)
		}
		return privateKey, nil
	}
	var decoded []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err = encoding.DecodeString(trimmed); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: key material is neither PEM nor base64", ErrSigningKeyInvalid)
	}
	switch len(decoded) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	default:
		return nil, fmt.Errorf("%w: decoded key is %d bytes, want %d or %d", ErrSigningKeyInvalid, len(decoded), ed25519.PrivateKeySize, ed25519.SeedSize)
	}
}

// KeyID is the JWS key ID placed in the protected header.
func (signer *EnvelopeSigner) KeyID() string { return signer.kid }

// Public returns the public half of the signing key for verifier
// distribution.
func (signer *EnvelopeSigner) Public() ed25519.PublicKey {
	return signer.privateKey.Public().(ed25519.PublicKey)
}

// SignJWS emits the JWS compact serialization of payload: BASE64URL(header)
// || "." || BASE64URL(payload) || "." || BASE64URL(Ed25519 signature).
func (signer *EnvelopeSigner) SignJWS(payload []byte) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"` + SigningAlgorithm + `","kid":"` + signer.kid + `"}`))
	input := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(signer.privateKey, []byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// VerifyJWS verifies one JWS compact serialization against the expected
// public key and key ID. The protected header must be exactly alg=EdDSA with
// the expected kid; anything else fails closed.
func VerifyJWS(publicKey ed25519.PublicKey, keyID string, jws string, payload *[]byte) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: Ed25519 public key is required", ErrSignatureInvalid)
	}
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%w: not a JWS compact serialization", ErrSignatureInvalid)
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("%w: undecodable protected header", ErrSignatureInvalid)
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return fmt.Errorf("%w: malformed protected header", ErrSignatureInvalid)
	}
	if header.Algorithm != SigningAlgorithm || header.KeyID != keyID {
		return fmt.Errorf("%w: unexpected JWS alg or kid", ErrSignatureInvalid)
	}
	signedPayload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("%w: undecodable payload", ErrSignatureInvalid)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("%w: undecodable signature", ErrSignatureInvalid)
	}
	if !ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return fmt.Errorf("%w: Ed25519 verification failed", ErrSignatureInvalid)
	}
	if payload != nil {
		*payload = signedPayload
	}
	return nil
}

// SignEnvelope computes the provenance signature for one envelope: the JWS
// compact serialization over the RFC 8785 canonical JSON of the full envelope
// excluding the signature field. The signature field is set on the returned
// copy.
func (signer *EnvelopeSigner) SignEnvelope(envelope Envelope) (Envelope, error) {
	payload, err := canonicalEnvelopePayload(envelope)
	if err != nil {
		return Envelope{}, err
	}
	envelope.Provenance.Signature = signer.SignJWS(payload)
	return envelope, nil
}

// VerifyEnvelope checks the provenance signature of one received envelope
// against the expected public key and key ID: the signature field is removed,
// the remainder is canonicalized per RFC 8785 and the JWS is verified.
func VerifyEnvelope(publicKey ed25519.PublicKey, keyID string, envelope Envelope) error {
	if envelope.Provenance.Signature == "" {
		return fmt.Errorf("%w: signature is absent", ErrSignatureInvalid)
	}
	expected, err := canonicalEnvelopePayload(envelope)
	if err != nil {
		return err
	}
	var signed []byte
	if err := VerifyJWS(publicKey, keyID, envelope.Provenance.Signature, &signed); err != nil {
		return err
	}
	// The embedded JWS payload must be exactly the canonical form of the
	// received envelope; otherwise a valid signature from another envelope
	// would transplant.
	if !bytes.Equal(signed, expected) {
		return fmt.Errorf("%w: signed payload does not match the envelope", ErrSignatureInvalid)
	}
	return nil
}

// canonicalEnvelopePayload renders the envelope signing input: the full
// envelope JSON with provenance.signature removed, canonicalized per
// RFC 8785. Removal (not blanking) keeps the signing domain identical for
// signer and verifier.
func canonicalEnvelopePayload(envelope Envelope) ([]byte, error) {
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode envelope for signing: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode envelope for signing: %w", err)
	}
	document, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("envelope is not a JSON object")
	}
	provenance, ok := document["provenance"].(map[string]any)
	if !ok {
		return nil, errors.New("envelope provenance is not a JSON object")
	}
	delete(provenance, "signature")
	canonical, err := Canonicalize(document)
	if err != nil {
		return nil, fmt.Errorf("canonicalize envelope for signing: %w", err)
	}
	return canonical, nil
}
