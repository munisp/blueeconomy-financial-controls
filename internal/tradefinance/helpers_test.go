package tradefinance

import (
	"strings"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/envelope"
)

func newTestSigner() (*envelope.Signer, error) {
	// Deterministic test-only key material (never a production path).
	return envelope.NewSigner("financial-controls-test", []byte(strings.Repeat("s", 32)))
}

func newTestVerifier(t *testing.T, signer *envelope.Signer) *envelope.Verifier {
	t.Helper()
	verifier, err := envelope.NewVerifier(map[string]string{signer.KeyID(): signer.PublicKeyBase64()})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}
