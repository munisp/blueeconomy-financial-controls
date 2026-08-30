// Package envelope implements the platform's signed artifact envelope v1.0:
// a FHIR R4 Bundle wrapping a JWS-EdDSA (compact serialization) over the
// RFC 8785 JCS-canonical JSON payload. It carries every artifact that
// crosses a trust boundary in the revenue-assurance chain — debit notes,
// TSA remittance advices, and inbound bank statements.
//
// Fail-closed posture: signing requires an explicit Ed25519 key (env-only
// secrets, never files or defaults); verification requires the signer's key
// id to be present in the trusted-key set. Payloads must be JCS-safe —
// objects, arrays, strings, booleans, null and integers only (non-integer
// numbers are rejected rather than silently reformatted).
package envelope

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version is the envelope format version carried on the Bundle meta tag.
const Version = "1.0"

// ProfileURL is the FHIR profile every envelope Bundle declares.
const ProfileURL = "https://blueeconomy.gov.ng/fhir/StructureDefinition/revenue-envelope-1.0"

const (
	artifactTypeSystem = "https://blueeconomy.gov.ng/artifact-types"
	jwsExtensionURL    = "https://blueeconomy.gov.ng/fhir/StructureDefinition/artifact-jws"
	versionTagSystem   = "https://blueeconomy.gov.ng/envelope"
	actorSystem        = "https://blueeconomy.gov.ng/actors"
)

var (
	// ErrUntrustedKey rejects a JWS whose kid is not in the trusted set.
	ErrUntrustedKey = errors.New("envelope signer key is not trusted")
	// ErrSignature rejects a JWS whose EdDSA signature does not verify.
	ErrSignature = errors.New("envelope signature does not verify")
	// ErrMalformed rejects an envelope that is not a well-formed v1.0 Bundle.
	ErrMalformed = errors.New("envelope is not a well-formed v1.0 bundle")
	// ErrNonCanonical rejects a payload that is not JCS-canonical.
	ErrNonCanonical = errors.New("envelope payload is not JCS-canonical")
)

// Signer seals envelopes with one Ed25519 key.
type Signer struct {
	kid string
	key ed25519.PrivateKey
}

// NewSigner fails closed on a malformed key or missing key id. The key is
// the raw 32-byte Ed25519 seed.
func NewSigner(kid string, seed []byte) (*Signer, error) {
	if strings.TrimSpace(kid) == "" || len(kid) > 128 {
		return nil, errors.New("envelope key id is required")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("envelope signing seed must be 32 bytes")
	}
	return &Signer{kid: kid, key: ed25519.NewKeyFromSeed(seed)}, nil
}

// KeyID returns the signer's key id.
func (signer *Signer) KeyID() string { return signer.kid }

// PublicKeyBase64 returns the base64url public half for trust distribution.
func (signer *Signer) PublicKeyBase64() string {
	public := signer.key.Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(public)
}

// Verifier checks envelopes against a trusted Ed25519 key set.
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewVerifier fails closed on an empty trust set or malformed key. Keys are
// base64url-encoded 32-byte Ed25519 public keys.
func NewVerifier(trusted map[string]string) (*Verifier, error) {
	if len(trusted) == 0 {
		return nil, errors.New("envelope trusted key set is required")
	}
	keys := make(map[string]ed25519.PublicKey, len(trusted))
	for kid, encoded := range trusted {
		if strings.TrimSpace(kid) == "" || len(kid) > 128 {
			return nil, errors.New("envelope trusted key id is invalid")
		}
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("envelope trusted key %q is not a base64url Ed25519 public key", kid)
		}
		keys[kid] = ed25519.PublicKey(raw)
	}
	return &Verifier{keys: keys}, nil
}

// JCS serializes a JSON value per RFC 8785. Only JCS-safe values are
// accepted: nil, bool, string, json.Number/int/int64/uint64 (integers
// only), []any and map[string]any. Structs must be marshaled and decoded
// first — Canonicalize handles that round trip.
func JCS(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeJCS(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// Canonicalize marshals a typed value to JSON and re-serializes it under
// RFC 8785 (sorted keys, minimal strings, integer numbers only).
func Canonicalize(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	return JCS(decoded)
}

func writeJCS(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		writeJCSString(buffer, typed)
	case json.Number:
		return writeJCSNumber(buffer, typed.String())
	case int:
		buffer.WriteString(strconv.Itoa(typed))
	case int64:
		buffer.WriteString(strconv.FormatInt(typed, 10))
	case uint64:
		buffer.WriteString(strconv.FormatUint(typed, 10))
	case float64:
		if typed != float64(int64(typed)) {
			return fmt.Errorf("non-integer number %v is not JCS-safe", typed)
		}
		buffer.WriteString(strconv.FormatInt(int64(typed), 10))
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeJCS(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			writeJCSString(buffer, key)
			buffer.WriteByte(':')
			if err := writeJCS(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		return fmt.Errorf("value of type %T is not JCS-safe", value)
	}
	return nil
}

func writeJCSNumber(buffer *bytes.Buffer, literal string) error {
	if strings.ContainsAny(literal, ".eE") {
		// Numbers must serialize as ES6 shortest form; integers only here —
		// money is minor units, so any fractional/exponential literal fails
		// closed rather than risking a non-canonical rendering.
		if parsed, err := strconv.ParseFloat(literal, 64); err == nil && parsed == float64(int64(parsed)) {
			buffer.WriteString(strconv.FormatInt(int64(parsed), 10))
			return nil
		}
		return fmt.Errorf("non-integer number %q is not JCS-safe", literal)
	}
	if _, err := strconv.ParseInt(literal, 10, 64); err != nil {
		return fmt.Errorf("number %q is not JCS-safe", literal)
	}
	buffer.WriteString(literal)
	return nil
}

// writeJCSString emits a string with the RFC 8785 minimal escape set.
func writeJCSString(buffer *bytes.Buffer, value string) {
	buffer.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buffer, `\u%04x`, r)
			} else {
				buffer.WriteRune(r)
			}
		}
	}
	buffer.WriteByte('"')
}

// Sign seals a payload into an envelope v1.0 Bundle. The returned bundle is
// the canonical JSON representation of the artifact; the JWS compact
// serialization is also returned separately for indexing.
func (signer *Signer) Sign(artifactKind, artifactID string, payload any, now time.Time) (bundle []byte, jws string, err error) {
	if strings.TrimSpace(artifactKind) == "" || strings.TrimSpace(artifactID) == "" {
		return nil, "", errors.New("artifact kind and id are required")
	}
	canonical, err := Canonicalize(payload)
	if err != nil {
		return nil, "", err
	}
	header, err := JCS(map[string]any{"alg": "EdDSA", "kid": signer.kid, "typ": "JWS"})
	if err != nil {
		return nil, "", err
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(canonical)
	signingInput := encodedHeader + "." + encodedPayload
	signature := ed25519.Sign(signer.key, []byte(signingInput))
	jws = signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)

	document := map[string]any{
		"resourceType": "Bundle",
		"id":           artifactID,
		"meta": map[string]any{
			"profile": []any{ProfileURL},
			"tag": []any{map[string]any{
				"system":  versionTagSystem,
				"code":    "version",
				"display": Version,
			}},
		},
		"type":      "document",
		"timestamp": now.UTC().Format(time.RFC3339),
		"entry": []any{map[string]any{
			"fullUrl": "urn:uuid:" + artifactID,
			"resource": map[string]any{
				"resourceType": "Basic",
				"id":           artifactID,
				"code": map[string]any{
					"coding": []any{map[string]any{
						"system": artifactTypeSystem,
						"code":   artifactKind,
					}},
					"text": artifactKind,
				},
				"created": now.UTC().Format("2006-01-02"),
				"author": map[string]any{
					"identifier": map[string]any{
						"system": actorSystem,
						"value":  signer.kid,
					},
				},
				"extension": []any{map[string]any{
					"url":        jwsExtensionURL,
					"valueString": jws,
				}},
			},
		}},
	}
	bundle, err = json.Marshal(document)
	if err != nil {
		return nil, "", fmt.Errorf("marshal envelope bundle: %w", err)
	}
	return bundle, jws, nil
}

// Verified is the result of a successful envelope verification: the
// JCS-canonical payload bytes, the artifact metadata and the signer key id.
type Verified struct {
	Payload       []byte
	ArtifactKind  string
	ArtifactID    string
	SignerKeyID   string
	PayloadSHA256 string
}

// Verify checks an envelope v1.0 Bundle end to end: Bundle shape, version
// tag, JWS well-formedness, trusted kid, EdDSA signature, and payload
// canonicality (the payload must re-serialize to itself under JCS).
func (verifier *Verifier) Verify(bundle []byte) (Verified, error) {
	var document struct {
		ResourceType string `json:"resourceType"`
		ID           string `json:"id"`
		Meta         struct {
			Profile []string `json:"profile"`
			Tag     []struct {
				System  string `json:"system"`
				Code    string `json:"code"`
				Display string `json:"display"`
			} `json:"tag"`
		} `json:"meta"`
		Type  string `json:"type"`
		Entry []struct {
			Resource struct {
				ResourceType string `json:"resourceType"`
				ID           string `json:"id"`
				Code         struct {
					Text string `json:"text"`
				} `json:"code"`
				Extension []struct {
					URL         string `json:"url"`
					ValueString string `json:"valueString"`
				} `json:"extension"`
			} `json:"resource"`
		} `json:"entry"`
	}
	decoder := json.NewDecoder(bytes.NewReader(bundle))
	if err := decoder.Decode(&document); err != nil {
		return Verified{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if document.ResourceType != "Bundle" || document.Type != "document" || len(document.Entry) != 1 {
		return Verified{}, ErrMalformed
	}
	profileOK := false
	for _, profile := range document.Meta.Profile {
		if profile == ProfileURL {
			profileOK = true
		}
	}
	versionOK := false
	for _, tag := range document.Meta.Tag {
		if tag.System == versionTagSystem && tag.Code == "version" && tag.Display == Version {
			versionOK = true
		}
	}
	if !profileOK || !versionOK {
		return Verified{}, ErrMalformed
	}
	resource := document.Entry[0].Resource
	if resource.ResourceType != "Basic" || resource.ID == "" {
		return Verified{}, ErrMalformed
	}
	var jws string
	for _, extension := range resource.Extension {
		if extension.URL == jwsExtensionURL {
			jws = extension.ValueString
		}
	}
	if jws == "" {
		return Verified{}, ErrMalformed
	}
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return Verified{}, ErrMalformed
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Verified{}, ErrMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil || header.Alg != "EdDSA" || header.Kid == "" {
		return Verified{}, ErrMalformed
	}
	key, trusted := verifier.keys[header.Kid]
	if !trusted {
		return Verified{}, ErrUntrustedKey
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Verified{}, ErrMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Verified{}, ErrMalformed
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return Verified{}, ErrSignature
	}
	// Canonicality: the payload must be byte-identical under JCS re-serialization.
	decoder = json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return Verified{}, ErrMalformed
	}
	recanonical, err := JCS(decoded)
	if err != nil || !bytes.Equal(recanonical, payload) {
		return Verified{}, ErrNonCanonical
	}
	digest := sha256.Sum256(payload)
	return Verified{
		Payload:       payload,
		ArtifactKind:  resource.Code.Text,
		ArtifactID:    resource.ID,
		SignerKeyID:   header.Kid,
		PayloadSHA256: hex.EncodeToString(digest[:]),
	}, nil
}
