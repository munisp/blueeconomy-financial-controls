package mojaloop

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// ILP v4 condition/fulfilment handling (RFC: Interledger Protocol v4). The
// fulfilment is a 32-byte preimage; the condition is the base64url-encoded
// SHA-256 of that preimage. Both travel base64url-encoded (no padding) on
// the wire. Nothing here is ever fabricated: conditions are derived from
// real preimages and fulfilments are verified cryptographically.

var (
	ErrInvalidFulfilment  = errors.New("ILP fulfilment does not satisfy the condition")
	ErrMalformedCondition = errors.New("ILP condition is not a base64url SHA-256 digest")
)

// ilpFulfilmentSize is the fixed 32-byte preimage size of an ILP v4
// SHA-256 condition.
const ilpFulfilmentSize = 32

// NewILPFulfilment generates a cryptographically random 32-byte fulfilment
// preimage and returns it base64url-encoded, together with the condition it
// satisfies (also base64url-encoded). This is the only sanctioned way to
// mint a fulfilment — the preimage is never reconstructed, stored plaintext
// beyond the caller, or reused.
func NewILPFulfilment() (fulfilment string, condition string, err error) {
	preimage := make([]byte, ilpFulfilmentSize)
	if _, err := rand.Read(preimage); err != nil {
		return "", "", fmt.Errorf("generate ILP fulfilment preimage: %w", err)
	}
	fulfilment = base64.RawURLEncoding.EncodeToString(preimage)
	return fulfilment, ConditionForFulfilment(fulfilment), nil
}

// ConditionForFulfilment derives the ILP condition from a base64url-encoded
// fulfilment: base64url(SHA-256(preimage)).
func ConditionForFulfilment(fulfilment string) string {
	preimage, err := base64.RawURLEncoding.DecodeString(fulfilment)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(preimage)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// ValidateILPCondition rejects anything that is not a well-formed base64url
// SHA-256 digest. The rail fails closed: a malformed condition from a payee
// FSP quote response is never trusted.
func ValidateILPCondition(condition string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(condition)
	if err != nil || len(decoded) != sha256.Size {
		return ErrMalformedCondition
	}
	return nil
}

// VerifyILPFulfilment checks that a base64url-encoded fulfilment satisfies
// the given condition: SHA-256(preimage) must equal the condition digest.
// The comparison is exact byte equality on the digests.
func VerifyILPFulfilment(condition, fulfilment string) error {
	if err := ValidateILPCondition(condition); err != nil {
		return err
	}
	if ConditionForFulfilment(fulfilment) != condition {
		return ErrInvalidFulfilment
	}
	return nil
}
