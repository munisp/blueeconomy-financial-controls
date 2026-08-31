package tradefinance

import (
	"strings"
	"testing"
	"time"
)

func digest(suffix string) string {
	return strings.Repeat("a", 63) + suffix
}

func validScopes() []Scope { return []Scope{ScopeDeclarationDigests, ScopeDutyPaymentHistory} }

func validRefs() map[string][]string {
	return map[string][]string{
		string(ScopeDeclarationDigests): {digest("1"), digest("2")},
		string(ScopeDutyPaymentHistory): {digest("3")},
	}
}

func validConsent(t *testing.T) Consent {
	t.Helper()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	consent, err := NewConsent("tf-con-001", "trader-001", "bank-gtb", validScopes(), validRefs(), now.Add(90*24*time.Hour), "trader-001", now)
	if err != nil {
		t.Fatal(err)
	}
	return consent
}

func TestNewConsentValidation(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		mutate  func(*[]Scope, *map[string][]string, *time.Time)
		wantErr error
	}{
		{"no scopes", func(s *[]Scope, r *map[string][]string, e *time.Time) { *s = nil }, ErrConsentScopeInvalid},
		{"unknown scope", func(s *[]Scope, r *map[string][]string, e *time.Time) { *s = []Scope{"RAW_DECLARATIONS"} }, ErrConsentScopeInvalid},
		{"duplicate scope", func(s *[]Scope, r *map[string][]string, e *time.Time) {
			*s = []Scope{ScopeDeclarationDigests, ScopeDeclarationDigests}
		}, ErrConsentScopeInvalid},
		{"missing refs", func(s *[]Scope, r *map[string][]string, e *time.Time) {
			delete(*r, string(ScopeDutyPaymentHistory))
		}, ErrDatasetRefMissing},
		{"non-digest ref", func(s *[]Scope, r *map[string][]string, e *time.Time) {
			(*r)[string(ScopeDeclarationDigests)] = []string{"declaration-123-raw"}
		}, nil},
		{"extra refs for unconsented scope", func(s *[]Scope, r *map[string][]string, e *time.Time) {
			(*r)[string(ScopeTaxStampStatus)] = []string{digest("9")}
		}, ErrConsentScopeInvalid},
		{"past expiry", func(s *[]Scope, r *map[string][]string, e *time.Time) { *e = now.Add(-time.Hour) }, nil},
		{"excessive lifetime", func(s *[]Scope, r *map[string][]string, e *time.Time) { *e = now.Add(800 * 24 * time.Hour) }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scopes := validScopes()
			refs := validRefs()
			expiry := now.Add(90 * 24 * time.Hour)
			tc.mutate(&scopes, &refs, &expiry)
			_, err := NewConsent("tf-con-002", "trader-001", "bank-gtb", scopes, refs, expiry, "trader-001", now)
			if err == nil {
				t.Fatalf("expected validation failure for %s", tc.name)
			}
			if tc.wantErr != nil && !strings.Contains(err.Error(), tc.wantErr.Error()) && err != tc.wantErr {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestConsentMakerCheckerSeparation(t *testing.T) {
	consent := validConsent(t)
	// Maker cannot check its own grant.
	if _, err := Activate(consent, "trader-001", "jws", []byte("{}")); err != ErrMakerChecker {
		t.Fatalf("self-check error = %v", err)
	}
	// Activation requires the envelope signature (fail-closed).
	if _, err := Activate(consent, "checker-001", "", nil); err != ErrConsentSignatureAbsent {
		t.Fatalf("unsigned activation error = %v", err)
	}
	activated, err := Activate(consent, "checker-001", "jws", []byte("{}"))
	if err != nil || activated.State != ConsentActive {
		t.Fatalf("activate: %+v %v", activated, err)
	}
	// Only the original trader maker may request revocation.
	if _, err := RequestRevocation(activated, "intruder"); err == nil {
		t.Fatal("revocation by non-maker accepted")
	}
	pending, err := RequestRevocation(activated, "trader-001")
	if err != nil || pending.State != ConsentRevocationPending {
		t.Fatalf("request revocation: %+v %v", pending, err)
	}
	// Sharing is suspended immediately while revocation pends.
	if pending.Shareable(time.Now()) {
		t.Fatal("revocation-pending consent is still shareable")
	}
	if _, err := ConfirmRevocation(pending, "trader-001"); err != ErrMakerChecker {
		t.Fatalf("self-confirm revocation error = %v", err)
	}
	revoked, err := ConfirmRevocation(pending, "checker-001")
	if err != nil || revoked.State != ConsentRevoked {
		t.Fatalf("confirm revocation: %+v %v", revoked, err)
	}
	if _, err := Activate(revoked, "checker-001", "jws", []byte("{}")); err != ErrConsentTerminalState {
		t.Fatalf("reactivation of revoked consent error = %v", err)
	}
}

func TestConsentShareableFailsClosed(t *testing.T) {
	consent := validConsent(t)
	now := time.Now().UTC()
	if consent.Shareable(now) {
		t.Fatal("PENDING_CHECKER consent is shareable")
	}
	activated, err := Activate(consent, "checker-001", "jws", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if !activated.Shareable(now) {
		t.Fatal("active unexpired consent is not shareable")
	}
	if activated.Shareable(activated.ExpiresAt.Add(time.Second)) {
		t.Fatal("expired consent is shareable")
	}
	if activated.Covers(ScopeTaxStampStatus) {
		t.Fatal("unconsented scope reported covered")
	}
}

func TestAuditChainVerification(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	e1 := NewAuditEntry("a1", "c1", 1, "maker", AuditRequested, "", ConsentPendingChecker, GenesisPrevHash, now)
	e2 := NewAuditEntry("a2", "c1", 2, "checker", AuditActivated, ConsentPendingChecker, ConsentActive, e1.EntryHash, now.Add(time.Minute))
	if err := VerifyAuditChain([]AuditEntry{e1, e2}); err != nil {
		t.Fatal(err)
	}
	// Tampered entry must fail.
	tampered := e2
	tampered.ActorPrincipal = "attacker"
	if err := VerifyAuditChain([]AuditEntry{e1, tampered}); err == nil {
		t.Fatal("tampered audit entry verified")
	}
	// Broken chain link must fail.
	broken := e2
	broken.PrevHash = GenesisPrevHash
	broken = NewAuditEntry("a2", "c1", 2, "checker", AuditActivated, ConsentPendingChecker, ConsentActive, GenesisPrevHash, now.Add(time.Minute))
	if err := VerifyAuditChain([]AuditEntry{e1, broken}); err == nil {
		t.Fatal("broken audit chain verified")
	}
	// Sequence gap must fail.
	if err := VerifyAuditChain([]AuditEntry{e2}); err == nil {
		t.Fatal("gapped audit chain verified")
	}
}

func TestEnvelopeSignerSealsActivatedConsent(t *testing.T) {
	// The envelope artifact of an activated consent must verify against the
	// signer's public key and fail on any tamper.
	signer, err := newTestSigner()
	if err != nil {
		t.Fatal(err)
	}
	consent := validConsent(t)
	consent.CheckerPrincipal = "checker-001"
	bundle, jws, err := signer.Sign("tradefinance.consent", consent.ConsentID, consentArtifactPayload(consent), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if jws == "" || len(bundle) == 0 {
		t.Fatal("empty envelope artifact")
	}
	verifier := newTestVerifier(t, signer)
	verified, err := verifier.Verify(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if verified.ArtifactKind != "tradefinance.consent" || verified.ArtifactID != consent.ConsentID {
		t.Fatalf("verified artifact = %+v", verified)
	}
	// Tampered bundle must fail closed.
	bundle[10] ^= 0xFF
	if _, err := verifier.Verify(bundle); err == nil {
		t.Fatal("tampered consent envelope verified")
	}
}
