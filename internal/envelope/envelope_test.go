package envelope

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T, kid string) *Signer {
	t.Helper()
	_, _, seed := generateKey(t)
	signer, err := NewSigner(kid, seed)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer
}

func generateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	seed := make([]byte, ed25519.SeedSize)
	copy(seed, private.Seed())
	return public, private, seed
}

func verifierFor(t *testing.T, kid string, public ed25519.PublicKey) *Verifier {
	t.Helper()
	verifier, err := NewVerifier(map[string]string{
		kid: base64.RawURLEncoding.EncodeToString(public),
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return verifier
}

// TestJCSRFC8785Vectors checks the canonicalization against the RFC 8785
// worked examples (object key ordering, string escapes, array compaction).
func TestJCSRFC8785Vectors(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"ordered keys", map[string]any{"b": "2", "a": "1"}, `{"a":"1","b":"2"}`},
		{"nested", map[string]any{"x": []any{int64(1), true, nil}, "y": map[string]any{"k": "v"}},
			`{"x":[1,true,null],"y":{"k":"v"}}`},
		{"escape quote", map[string]any{"s": `a"b\c`}, `{"s":"a\"b\\c"}`},
		{"control chars", map[string]any{"s": "a\nb\t"}, `{"s":"a\nb\t"}`},
		{"unicode passthrough", map[string]any{"s": "₦"}, `{"s":"₦"}`},
		{"integers", map[string]any{"n": int64(3675000)}, `{"n":3675000}`},
	}
	for _, testCase := range cases {
		got, err := JCS(testCase.value)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if string(got) != testCase.want {
			t.Fatalf("%s: got %s want %s", testCase.name, got, testCase.want)
		}
	}
}

// TestJCSRejectsNonIntegerNumbers fails closed on fractional numbers.
func TestJCSRejectsNonIntegerNumbers(t *testing.T) {
	if _, err := JCS(map[string]any{"n": 1.5}); err == nil {
		t.Fatal("expected non-integer number to be rejected")
	}
	if _, err := JCS(map[string]any{"n": json.Number("0.2")}); err == nil {
		t.Fatal("expected fractional json.Number to be rejected")
	}
}

// TestSignVerifyRoundtrip seals and opens an envelope end to end.
func TestSignVerifyRoundtrip(t *testing.T) {
	public, _, seed := generateKey(t)
	signer, err := NewSigner("bank-gateway-1", seed)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"bank": "CBN", "amount": int64(1000000), "lines": []any{map[string]any{"ref": "BR-1"}}}
	bundle, jws, err := signer.Sign("BANK_STATEMENT", "stmt-1", payload, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if jws == "" || len(strings.Split(jws, ".")) != 3 {
		t.Fatalf("jws shape: %q", jws)
	}
	var document map[string]any
	if err := json.Unmarshal(bundle, &document); err != nil {
		t.Fatalf("bundle is not JSON: %v", err)
	}
	if document["resourceType"] != "Bundle" || document["type"] != "document" {
		t.Fatalf("not a FHIR document Bundle: %v", document["resourceType"])
	}
	verifier := verifierFor(t, "bank-gateway-1", public)
	verified, err := verifier.Verify(bundle)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.ArtifactKind != "BANK_STATEMENT" || verified.ArtifactID != "stmt-1" || verified.SignerKeyID != "bank-gateway-1" {
		t.Fatalf("verified metadata: %+v", verified)
	}
	canonical, err := Canonicalize(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(verified.Payload) != string(canonical) {
		t.Fatalf("payload mismatch:\n%s\n%s", verified.Payload, canonical)
	}
}

// TestVerifyRejectsTampering fails closed on a modified payload.
func TestVerifyRejectsTampering(t *testing.T) {
	public, _, seed := generateKey(t)
	signer, _ := NewSigner("kid-1", seed)
	bundle, jws, err := signer.Sign("DEBIT_NOTE", "note-1", map[string]any{"amount": int64(100)}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Replace the JWS payload with a forged one (signature now mismatches).
	parts := strings.Split(jws, ".")
	forgedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"amount":999}`))
	forged := parts[0] + "." + forgedPayload + "." + parts[2]
	tampered := strings.Replace(string(bundle), jws, forged, 1)
	verifier := verifierFor(t, "kid-1", public)
	if _, err := verifier.Verify([]byte(tampered)); err == nil {
		t.Fatal("tampered envelope verified")
	}
}

// TestVerifyRejectsUntrustedKey fails closed on an unknown kid.
func TestVerifyRejectsUntrustedKey(t *testing.T) {
	_, _, seed := generateKey(t)
	signer, _ := NewSigner("kid-unknown", seed)
	bundle, _, err := signer.Sign("DEBIT_NOTE", "note-2", map[string]any{"a": int64(1)}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	otherPublic, _, _ := generateKey(t)
	verifier := verifierFor(t, "kid-other", otherPublic)
	if _, err := verifier.Verify(bundle); err == nil {
		t.Fatal("untrusted signer verified")
	}
}

// TestNewSignerFailsClosed rejects malformed key material.
func TestNewSignerFailsClosed(t *testing.T) {
	if _, err := NewSigner("", make([]byte, ed25519.SeedSize)); err == nil {
		t.Fatal("empty kid accepted")
	}
	if _, err := NewSigner("kid", []byte("short")); err == nil {
		t.Fatal("short seed accepted")
	}
	if _, err := NewVerifier(map[string]string{}); err == nil {
		t.Fatal("empty trust set accepted")
	}
	if _, err := NewVerifier(map[string]string{"kid": "not-base64!!"}); err == nil {
		t.Fatal("malformed key accepted")
	}
}
