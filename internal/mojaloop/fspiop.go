package mojaloop

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const signatureHeader = "FSPIOP-Signature"

var (
	ErrInvalidSignature       = errors.New("invalid FSPIOP signature")
	ErrInvalidTransferState   = errors.New("invalid transfer state transition")
	ErrTransferIdentityChange = errors.New("transfer identity changed in callback")
)

type signatureProtectedHeader struct {
	Alg         string `json:"alg"`
	KeyID       string `json:"kid,omitempty"`
	URI         string `json:"FSPIOP-URI"`
	HTTPMethod  string `json:"FSPIOP-HTTP-Method"`
	Source      string `json:"FSPIOP-Source"`
	Destination string `json:"FSPIOP-Destination,omitempty"`
}

type signatureHeaderValue struct {
	ProtectedHeader string `json:"protectedHeader"`
	Signature       string `json:"signature"`
}

// SignRequest creates the standard Mojaloop FSPIOP-Signature header for a full-body request.
// The caller must supply the exact path and query string used on the wire.
func SignRequest(method, uri, source, destination string, body []byte, key *rsa.PrivateKey, algorithm string) (string, error) {
	return SignRequestWithKeyID(method, uri, source, destination, body, key, algorithm, "")
}

// SignRequestWithKeyID creates a FSPIOP-Signature header and, when supplied by an approved Hub manifest, binds the registered JOSE KID into the protected header.
func SignRequestWithKeyID(method, uri, source, destination string, body []byte, key *rsa.PrivateKey, algorithm, keyID string) (string, error) {
	if key == nil {
		return "", errors.New("signing key is required")
	}
	if keyID != "" {
		if err := canonicalReference("FSPIOP signing kid", keyID); err != nil {
			return "", err
		}
	}
	protected := signatureProtectedHeader{Alg: algorithm, KeyID: keyID, URI: uri, HTTPMethod: strings.ToUpper(method), Source: source, Destination: destination}
	protectedBytes, err := json.Marshal(protected)
	if err != nil {
		return "", fmt.Errorf("marshal protected header: %w", err)
	}
	protectedEncoded := base64.RawURLEncoding.EncodeToString(protectedBytes)
	payloadEncoded := base64.RawURLEncoding.EncodeToString(body)
	cryptoHash, err := signatureHash(algorithm)
	if err != nil {
		return "", err
	}
	digest := hashForAlgorithm(payloadEncoded, protectedEncoded, cryptoHash)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoHash, digest)
	if err != nil {
		return "", fmt.Errorf("sign FSPIOP request: %w", err)
	}
	value, err := json.Marshal(signatureHeaderValue{ProtectedHeader: protectedEncoded, Signature: base64.RawURLEncoding.EncodeToString(signature)})
	if err != nil {
		return "", fmt.Errorf("marshal FSPIOP signature: %w", err)
	}
	return string(value), nil
}

// VerifyRequest validates the FSPIOP-Signature header, protected HTTP metadata and full request body.
func VerifyRequest(method, uri, source, destination string, body []byte, headerValue string, key *rsa.PublicKey) error {
	return VerifyRequestWithKeyID(method, uri, source, destination, body, headerValue, key, "")
}

// VerifyRequestWithKeyID validates the FSPIOP signature and, when a Hub/participant manifest declares it, requires the signed JOSE KID to match the approved verification-key identity.
func VerifyRequestWithKeyID(method, uri, source, destination string, body []byte, headerValue string, key *rsa.PublicKey, expectedKeyID string) error {
	if key == nil {
		return errors.New("verification key is required")
	}
	if expectedKeyID != "" {
		if err := canonicalReference("FSPIOP verification kid", expectedKeyID); err != nil {
			return err
		}
	}
	var value signatureHeaderValue
	if err := json.Unmarshal([]byte(headerValue), &value); err != nil {
		return fmt.Errorf("decode FSPIOP-Signature: %w", err)
	}
	protectedBytes, err := base64.RawURLEncoding.DecodeString(value.ProtectedHeader)
	if err != nil {
		return fmt.Errorf("decode protected header: %w", err)
	}
	var protected signatureProtectedHeader
	if err := json.Unmarshal(protectedBytes, &protected); err != nil {
		return fmt.Errorf("decode protected header JSON: %w", err)
	}
	if protected.HTTPMethod != strings.ToUpper(method) || protected.URI != uri || protected.Source != source || (destination != "" && protected.Destination != destination) {
		return ErrInvalidSignature
	}
	if expectedKeyID != "" && protected.KeyID != expectedKeyID {
		return ErrInvalidSignature
	}
	cryptoHash, err := signatureHash(protected.Alg)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	payloadEncoded := base64.RawURLEncoding.EncodeToString(body)
	digest := hashForAlgorithm(payloadEncoded, value.ProtectedHeader, cryptoHash)
	if err := rsa.VerifyPKCS1v15(key, cryptoHash, digest, signature); err != nil {
		return fmt.Errorf("verify FSPIOP signature: %w", ErrInvalidSignature)
	}
	return nil
}

func signatureHash(algorithm string) (crypto.Hash, error) {
	switch algorithm {
	case "RS256":
		return crypto.SHA256, nil
	case "RS384":
		return crypto.SHA384, nil
	case "RS512":
		return crypto.SHA512, nil
	default:
		return 0, fmt.Errorf("unsupported FSPIOP signature algorithm %q", algorithm)
	}
}

func hashForAlgorithm(payloadEncoded, protectedEncoded string, hash crypto.Hash) []byte {
	input := []byte(protectedEncoded + "." + payloadEncoded)
	switch hash {
	case crypto.SHA256:
		digest := sha256.Sum256(input)
		return digest[:]
	case crypto.SHA384:
		digest := sha512.Sum384(input)
		return digest[:]
	case crypto.SHA512:
		digest := sha512.Sum512(input)
		return digest[:]
	default:
		return nil
	}
}

func AddSignatureHeaders(request *http.Request, signature string, source, destination string) {
	request.Header.Set(signatureHeader, signature)
	request.Header.Set("FSPIOP-Source", source)
	if destination != "" {
		request.Header.Set("FSPIOP-Destination", destination)
	}
}

type TransferState string

const (
	TransferReserved  TransferState = "RESERVED"
	TransferCommitted TransferState = "COMMITTED"
	TransferAborted   TransferState = "ABORTED"
)

type TransferIdentity struct {
	TransferID string `json:"transferId"`
	PayerFSP   string `json:"payerFsp"`
	PayeeFSP   string `json:"payeeFsp"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
}

type TransferCallback struct {
	TransferIdentity
	TransferState      TransferState `json:"transferState"`
	Fulfilment         string        `json:"fulfilment,omitempty"`
	CompletedTimestamp string        `json:"completedTimestamp,omitempty"`
}

// validateCallbackIdentity enforces the presence of the transfer identity
// and amount fields. It is the entry validation for every callback,
// regardless of whether a durable record already exists.
func validateCallbackIdentity(callback TransferCallback) error {
	if callback.TransferID == "" || callback.PayerFSP == "" || callback.PayeeFSP == "" || callback.Amount == "" || callback.Currency == "" {
		return errors.New("transfer callback identity and amount are required")
	}
	return nil
}

func ValidateTransferCallback(previous *TransferCallback, callback TransferCallback) error {
	if err := validateCallbackIdentity(callback); err != nil {
		return err
	}
	if previous != nil && (previous.TransferID != callback.TransferID || previous.PayerFSP != callback.PayerFSP || previous.PayeeFSP != callback.PayeeFSP || previous.Amount != callback.Amount || previous.Currency != callback.Currency) {
		return ErrTransferIdentityChange
	}
	if previous == nil {
		// First-seen policy (fail-closed): the durable record of a transfer
		// always begins at RESERVED, the state in which funds are locked.
		// A first-seen COMMITTED/ABORTED would mean money settled (or was
		// abandoned) without this rail ever observing the reservation, so it
		// is rejected as an invalid transition; the Hub must deliver the
		// RESERVED callback first (replays are idempotent on the body hash).
		if callback.TransferState != TransferReserved {
			return ErrInvalidTransferState
		}
		return nil
	}
	if previous.TransferState == callback.TransferState && (callback.TransferState == TransferCommitted || callback.TransferState == TransferAborted) {
		return nil
	}
	switch previous.TransferState {
	case TransferReserved:
		if callback.TransferState == TransferCommitted || callback.TransferState == TransferAborted {
			return nil
		}
	case TransferCommitted, TransferAborted:
		return ErrInvalidTransferState
	}
	return ErrInvalidTransferState
}
