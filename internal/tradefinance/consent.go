// Package tradefinance implements the WP-6 trade-finance rail: a
// CamelONE-style (Singapore NTP Trade Finance Compliance) multi-bank data
// sharing and trade-finance product capability.
//
// The consent registry is the trust boundary: a trader grants one bank
// access to explicitly scoped datasets (declaration digests, duty payment
// history, tax-stamp status refs), time-boxed and revocable. Every consent
// grant is maker-checker, envelope-signed (envelope v1.0: FHIR R4 Bundle +
// JWS EdDSA over JCS-canonical payload) and hash-chained into an immutable
// audit trail. The bank-facing data-sharing API is fail-closed: it exposes
// only consented scopes, only to env-registered banks, and signs every
// response.
//
// The product workflow offers the four standardized starter products
// (import LC facilitation, export pre-shipment finance, invoice/receivables
// finance, duty deferral guarantee) through the lifecycle
// APPLICATION→KYC_CONSENT_CHECK→BANK_REVIEW→REGULATORY_CLEARANCE→APPROVED→
// DISBURSEMENT_PENDING→DISBURSED→SETTLED (DECLINED on any rejection),
// reusing the CVFF four-party approval machinery pattern and TigerBeetle
// double-entry for the disbursement ledger.
package tradefinance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Scope identifies one consented dataset class. The bank-facing API exposes
// only the scopes listed on an active consent; anything else fails closed.
type Scope string

const (
	// ScopeDeclarationDigests covers customs declaration digest references.
	ScopeDeclarationDigests Scope = "DECLARATION_DIGESTS"
	// ScopeDutyPaymentHistory covers duty/tax payment history digests.
	ScopeDutyPaymentHistory Scope = "DUTY_PAYMENT_HISTORY"
	// ScopeTaxStampStatus covers tax-stamp status references (shared once
	// the tax-stamps rail publishes them).
	ScopeTaxStampStatus Scope = "TAX_STAMP_STATUS"
)

// Scopes is the closed set of shareable dataset classes.
var Scopes = []Scope{ScopeDeclarationDigests, ScopeDutyPaymentHistory, ScopeTaxStampStatus}

// ConsentState is the maker-checker lifecycle of one consent.
type ConsentState string

const (
	// ConsentPendingChecker awaits the compliance checker; no data sharing.
	ConsentPendingChecker ConsentState = "PENDING_CHECKER"
	// ConsentActive is the only state in which scoped sharing is permitted.
	ConsentActive ConsentState = "ACTIVE"
	// ConsentRejected is terminal: the checker declined the grant.
	ConsentRejected ConsentState = "REJECTED"
	// ConsentRevocationPending suspends sharing immediately on the trader's
	// revocation request, pending checker confirmation (fail-closed toward
	// the data subject).
	ConsentRevocationPending ConsentState = "REVOCATION_PENDING"
	// ConsentRevoked is terminal.
	ConsentRevoked ConsentState = "REVOKED"
)

// Consent is one trader→bank scoped data-sharing grant.
type Consent struct {
	ConsentID        string              `json:"consent_id"`
	TraderID         string              `json:"trader_id"`
	BankID           string              `json:"bank_id"`
	Scopes           []Scope             `json:"scopes"`
	DatasetRefs      map[string][]string `json:"dataset_refs"`
	State            ConsentState        `json:"state"`
	ExpiresAt        time.Time           `json:"expires_at"`
	MakerPrincipal   string              `json:"maker_principal"`
	CheckerPrincipal string              `json:"checker_principal,omitempty"`
	EnvelopeJWS      string              `json:"envelope_jws,omitempty"`
	EnvelopeBundle   []byte              `json:"envelope_bundle,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
	Version          int64               `json:"version"`
}

var (
	ErrConsentNotFound        = errors.New("tradefinance consent not found")
	ErrConsentConflict        = errors.New("tradefinance consent changed concurrently or an open consent exists")
	ErrConsentState           = errors.New("invalid tradefinance consent state transition")
	ErrMakerChecker           = errors.New("tradefinance consent maker-checker separation violated")
	ErrScopeNotConsented      = errors.New("dataset scope is not covered by an active consent")
	ErrConsentExpired         = errors.New("tradefinance consent is expired")
	ErrConsentScopeInvalid    = errors.New("tradefinance consent scopes are invalid")
	ErrDatasetRefMissing      = errors.New("every consented scope requires at least one dataset digest reference")
	ErrConsentNotShareable    = errors.New("tradefinance consent is not in a shareable state")
	ErrConsentTerminalState   = errors.New("tradefinance consent is in a terminal state")
	ErrConsentSignatureAbsent = errors.New("activated consent lacks its envelope signature")
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidateIdentifier constrains identifiers to canonical approved text.
func ValidateIdentifier(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 256 || !idPattern.MatchString(value) {
		return fmt.Errorf("%s is not canonical approved identifier text", name)
	}
	return nil
}

func validScope(scope Scope) bool {
	for _, approved := range Scopes {
		if scope == approved {
			return true
		}
	}
	return false
}

// MaxConsentLifetime bounds the time-boxing of any grant: 366 days.
const MaxConsentLifetime = 366 * 24 * time.Hour

// digestPattern constrains dataset references to sha256 hex digests (or
// digest-bearing URNs), never raw data.
var digestPattern = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$`)

// NewConsent builds a validated PENDING_CHECKER consent. The maker is the
// trader-side principal requesting the grant.
func NewConsent(consentID, traderID, bankID string, scopes []Scope, datasetRefs map[string][]string, expiresAt time.Time, makerPrincipal string, now time.Time) (Consent, error) {
	for name, value := range map[string]string{
		"consent_id": consentID, "trader_id": traderID, "bank_id": bankID, "maker_principal": makerPrincipal,
	} {
		if err := ValidateIdentifier(name, value); err != nil {
			return Consent{}, err
		}
	}
	if len(scopes) == 0 {
		return Consent{}, ErrConsentScopeInvalid
	}
	seen := map[Scope]bool{}
	for _, scope := range scopes {
		if !validScope(scope) || seen[scope] {
			return Consent{}, ErrConsentScopeInvalid
		}
		seen[scope] = true
		refs := datasetRefs[string(scope)]
		if len(refs) == 0 {
			return Consent{}, fmt.Errorf("%w: %s", ErrDatasetRefMissing, scope)
		}
		for _, ref := range refs {
			if !digestPattern.MatchString(ref) {
				return Consent{}, fmt.Errorf("dataset ref for %s is not a digest reference", scope)
			}
		}
	}
	// References for scopes outside the grant are rejected: the consent
	// must describe exactly the shareable surface, nothing more.
	for key := range datasetRefs {
		if !seen[Scope(key)] {
			return Consent{}, fmt.Errorf("%w: dataset refs carry unconsented scope %s", ErrConsentScopeInvalid, key)
		}
	}
	if !expiresAt.After(now) {
		return Consent{}, fmt.Errorf("consent expiry must be in the future")
	}
	if expiresAt.After(now.Add(MaxConsentLifetime)) {
		return Consent{}, fmt.Errorf("consent expiry exceeds the %d-day maximum", 366)
	}
	return Consent{
		ConsentID: consentID, TraderID: traderID, BankID: bankID,
		Scopes: append([]Scope(nil), scopes...), DatasetRefs: copyDatasetRefs(datasetRefs),
		State: ConsentPendingChecker, ExpiresAt: expiresAt.UTC(), MakerPrincipal: makerPrincipal,
	}, nil
}

func copyDatasetRefs(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, refs := range in {
		out[key] = append([]string(nil), refs...)
	}
	return out
}

// Shareable reports whether scoped sharing is permitted at the given time.
// Strictly ACTIVE and unexpired; every other state fails closed.
func (consent Consent) Shareable(now time.Time) bool {
	return consent.State == ConsentActive && now.Before(consent.ExpiresAt)
}

// Covers reports whether the consent grants the given scope.
func (consent Consent) Covers(scope Scope) bool {
	for _, granted := range consent.Scopes {
		if granted == scope {
			return true
		}
	}
	return false
}

// Activate applies the checker's approval. The checker must differ from the
// maker and the activated consent must carry its envelope signature.
func Activate(current Consent, checkerPrincipal string, envelopeJWS string, bundle []byte) (Consent, error) {
	if current.State == ConsentRejected || current.State == ConsentRevoked {
		return Consent{}, ErrConsentTerminalState
	}
	if current.State != ConsentPendingChecker {
		return Consent{}, ErrConsentState
	}
	if err := ValidateIdentifier("checker_principal", checkerPrincipal); err != nil {
		return Consent{}, err
	}
	if checkerPrincipal == current.MakerPrincipal {
		return Consent{}, ErrMakerChecker
	}
	if envelopeJWS == "" || len(bundle) == 0 {
		return Consent{}, ErrConsentSignatureAbsent
	}
	current.State = ConsentActive
	current.CheckerPrincipal = checkerPrincipal
	current.EnvelopeJWS = envelopeJWS
	current.EnvelopeBundle = bundle
	return current, nil
}

// RejectGrant applies the checker's decline of a pending grant.
func RejectGrant(current Consent, checkerPrincipal string) (Consent, error) {
	if current.State != ConsentPendingChecker {
		return Consent{}, ErrConsentState
	}
	if err := ValidateIdentifier("checker_principal", checkerPrincipal); err != nil {
		return Consent{}, err
	}
	if checkerPrincipal == current.MakerPrincipal {
		return Consent{}, ErrMakerChecker
	}
	current.State = ConsentRejected
	current.CheckerPrincipal = checkerPrincipal
	return current, nil
}

// RequestRevocation suspends sharing immediately, pending checker confirm.
// The trader-side maker of the original grant (or its delegate principal)
// requests; the checker confirms.
func RequestRevocation(current Consent, makerPrincipal string) (Consent, error) {
	if current.State == ConsentRejected || current.State == ConsentRevoked {
		return Consent{}, ErrConsentTerminalState
	}
	if current.State != ConsentActive {
		return Consent{}, ErrConsentState
	}
	if err := ValidateIdentifier("maker_principal", makerPrincipal); err != nil {
		return Consent{}, err
	}
	if makerPrincipal != current.MakerPrincipal {
		return Consent{}, fmt.Errorf("only the consenting trader principal may request revocation")
	}
	current.State = ConsentRevocationPending
	return current, nil
}

// ConfirmRevocation finalizes a requested revocation; the checker must
// differ from the requesting maker.
func ConfirmRevocation(current Consent, checkerPrincipal string) (Consent, error) {
	if current.State != ConsentRevocationPending {
		return Consent{}, ErrConsentState
	}
	if err := ValidateIdentifier("checker_principal", checkerPrincipal); err != nil {
		return Consent{}, err
	}
	if checkerPrincipal == current.MakerPrincipal {
		return Consent{}, ErrMakerChecker
	}
	current.State = ConsentRevoked
	current.CheckerPrincipal = checkerPrincipal
	return current, nil
}

// RejectRevocation returns a REVOCATION_PENDING consent to ACTIVE when the
// checker finds the revocation request defective (e.g. raised against the
// wrong consent). Sharing resumes only through this explicit decision.
func RejectRevocation(current Consent, checkerPrincipal string) (Consent, error) {
	if current.State != ConsentRevocationPending {
		return Consent{}, ErrConsentState
	}
	if err := ValidateIdentifier("checker_principal", checkerPrincipal); err != nil {
		return Consent{}, err
	}
	if checkerPrincipal == current.MakerPrincipal {
		return Consent{}, ErrMakerChecker
	}
	current.State = ConsentActive
	current.CheckerPrincipal = checkerPrincipal
	return current, nil
}

// AuditAction identifies one consent audit-chain entry.
type AuditAction string

const (
	AuditRequested           AuditAction = "REQUESTED"
	AuditActivated           AuditAction = "ACTIVATED"
	AuditRejected            AuditAction = "REJECTED"
	AuditRevocationRequested AuditAction = "REVOCATION_REQUESTED"
	AuditRevoked             AuditAction = "REVOKED"
	AuditRevocationRejected  AuditAction = "REVOCATION_REJECTED"
)

// AuditEntry is one hash-chained, immutable consent audit record.
type AuditEntry struct {
	AuditID        string       `json:"audit_id"`
	ConsentID      string       `json:"consent_id"`
	Seq            int64        `json:"seq"`
	ActorPrincipal string       `json:"actor_principal"`
	Action         AuditAction  `json:"action"`
	FromState      ConsentState `json:"from_state"`
	ToState        ConsentState `json:"to_state"`
	EntryHash      string       `json:"entry_hash"`
	PrevHash       string       `json:"prev_hash"`
	CreatedAt      time.Time    `json:"created_at"`
}

// chainHash computes the entry hash: sha256 over the canonical entry fields
// concatenated with the previous hash. The chain is verified by recomputing
// from seq 1 with the zero prev hash.
func chainHash(consentID string, seq int64, actor string, action AuditAction, from, to ConsentState, prevHash string, createdAt time.Time) string {
	payload := strings.Join([]string{
		consentID, fmt.Sprint(seq), actor, string(action), string(from), string(to), prevHash, createdAt.UTC().Format(time.RFC3339Nano),
	}, "|")
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

// GenesisPrevHash is the prev hash of the first audit entry of a consent.
const GenesisPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

// NewAuditEntry builds one chain entry.
func NewAuditEntry(auditID, consentID string, seq int64, actor string, action AuditAction, from, to ConsentState, prevHash string, now time.Time) AuditEntry {
	// timestamptz persists microseconds; truncate so the stored hash and any
	// recompute from a database read are byte-identical.
	created := now.UTC().Truncate(time.Microsecond)
	return AuditEntry{
		AuditID: auditID, ConsentID: consentID, Seq: seq, ActorPrincipal: actor, Action: action,
		FromState: from, ToState: to, PrevHash: prevHash, CreatedAt: created,
		EntryHash: chainHash(consentID, seq, actor, action, from, to, prevHash, created),
	}
}

// VerifyAuditChain recomputes the chain over ordered entries; any gap or
// rewrite fails closed.
func VerifyAuditChain(entries []AuditEntry) error {
	prev := GenesisPrevHash
	for index, entry := range entries {
		if entry.Seq != int64(index+1) {
			return fmt.Errorf("consent audit chain gap at seq %d", entry.Seq)
		}
		if entry.PrevHash != prev {
			return fmt.Errorf("consent audit chain broken at seq %d", entry.Seq)
		}
		expected := chainHash(entry.ConsentID, entry.Seq, entry.ActorPrincipal, entry.Action, entry.FromState, entry.ToState, entry.PrevHash, entry.CreatedAt)
		if expected != entry.EntryHash {
			return fmt.Errorf("consent audit entry %d hash mismatch", entry.Seq)
		}
		prev = entry.EntryHash
	}
	return nil
}
