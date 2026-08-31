package tradefinance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/http"
	"testing"
)

func TestBankRegistryFailsClosed(t *testing.T) {
	for _, registryJSON := range []string{
		"",
		"   ",
		"[]",
		"not-json",
		`[{"bank_id":"","oidc_subjects":["sub"]}]`,
		`[{"bank_id":"bank-gtb"}]`,
		`[{"bank_id":"bank gtb","oidc_subjects":["sub"]}]`,
	} {
		if _, err := NewBankRegistry(registryJSON); err == nil {
			t.Fatalf("registry %q accepted", registryJSON)
		}
	}
	registry, err := NewBankRegistry(`[{"bank_id":"bank-gtb","oidc_subjects":["kc-bank-gtb-client"],"mtls_common_names":["bank-gtb.blueeconomy.gov.ng"]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if registry.bySubject["kc-bank-gtb-client"] != "bank-gtb" || registry.byCN["bank-gtb.blueeconomy.gov.ng"] != "bank-gtb" {
		t.Fatal("registry bindings incomplete")
	}
}

type stubAuthenticator struct {
	subject string
	err     error
}

func (stub stubAuthenticator) AuthenticateSubject(ctx context.Context, header string) (string, error) {
	return stub.subject, stub.err
}

func mtlsRequest(cn string) *http.Request {
	request, _ := http.NewRequest(http.MethodGet, "/v1/tradefinance/bank/datasets/trader-001/DECLARATION_DIGESTS", nil)
	request.TLS = &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: cn}}}},
	}
	return request
}

func TestResolveBank(t *testing.T) {
	registry, err := NewBankRegistry(`[{"bank_id":"bank-gtb","oidc_subjects":["kc-bank-gtb-client"],"mtls_common_names":["bank-gtb.blueeconomy.gov.ng"]}]`)
	if err != nil {
		t.Fatal(err)
	}
	// mTLS happy path.
	bankID, err := registry.resolveBank(mtlsRequest("bank-gtb.blueeconomy.gov.ng"), nil)
	if err != nil || bankID != "bank-gtb" {
		t.Fatalf("mtls resolve: %s %v", bankID, err)
	}
	// Unknown mTLS CN is forbidden, never falls through to bearer.
	if _, err := registry.resolveBank(mtlsRequest("rogue-bank"), nil); !errors.Is(err, ErrBankForbidden) {
		t.Fatalf("unknown CN error = %v", err)
	}
	// Bearer subject path.
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer token")
	bankID, err = registry.resolveBank(request, stubAuthenticator{subject: "kc-bank-gtb-client"})
	if err != nil || bankID != "bank-gtb" {
		t.Fatalf("bearer resolve: %s %v", bankID, err)
	}
	// Unregistered subject is forbidden.
	if _, err := registry.resolveBank(request, stubAuthenticator{subject: "kc-other"}); !errors.Is(err, ErrBankForbidden) {
		t.Fatalf("unregistered subject error = %v", err)
	}
	// Verification failure is unauthenticated.
	if _, err := registry.resolveBank(request, stubAuthenticator{err: errors.New("bad token")}); !errors.Is(err, ErrBankUnauthenticated) {
		t.Fatalf("bad token error = %v", err)
	}
	// No authenticator configured (mTLS-only deployment) fails closed.
	if _, err := registry.resolveBank(request, nil); !errors.Is(err, ErrBankUnauthenticated) {
		t.Fatalf("no-authenticator error = %v", err)
	}
}

func TestNewBankAPIRequiresDependencies(t *testing.T) {
	if _, err := NewBankAPI(nil, nil, nil, nil); err == nil {
		t.Fatal("bank API built without dependencies")
	}
}
