package mojaloop

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"
)

func TestClientBuildsSignedFSPIOPRequest(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	baseURL, err := url.Parse("https://partner.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client := Client{BaseURL: baseURL, Source: "FMMBE", Destination: "PARTNER01", SigningKey: key, SigningKeyID: "fmmbe-sandbox-2026-01", SignatureAlgorithm: "RS256"}
	body := []byte(`{"transferId":"transfer-001"}`)
	request, err := client.NewSignedRequest(context.Background(), "POST", "/transfers?mode=universal", body)
	if err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("FSPIOP-Source") != "FMMBE" || request.Header.Get("FSPIOP-Destination") != "PARTNER01" {
		t.Fatal("FSPIOP source/destination headers missing")
	}
	if err := VerifyRequest(request.Method, request.URL.RequestURI(), "FMMBE", "PARTNER01", body, request.Header.Get(signatureHeader), &key.PublicKey); err != nil {
		t.Fatal(err)
	}
	var envelope signatureHeaderValue
	if err := json.Unmarshal([]byte(request.Header.Get(signatureHeader)), &envelope); err != nil {
		t.Fatal(err)
	}
	protectedBytes, err := base64.RawURLEncoding.DecodeString(envelope.ProtectedHeader)
	if err != nil {
		t.Fatal(err)
	}
	var protected signatureProtectedHeader
	if err := json.Unmarshal(protectedBytes, &protected); err != nil {
		t.Fatal(err)
	}
	if protected.KeyID != client.SigningKeyID {
		t.Fatalf("protected KID = %q, want %q", protected.KeyID, client.SigningKeyID)
	}
}

func TestClientRejectsNonHTTPSAndTraversal(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	httpURL, _ := url.Parse("http://partner.example.invalid")
	client := Client{BaseURL: httpURL, Source: "FMMBE", Destination: "PARTNER01", SigningKey: key, SignatureAlgorithm: "RS256"}
	if _, err := client.NewSignedRequest(context.Background(), "POST", "/transfers", nil); err == nil {
		t.Fatal("non-HTTPS endpoint accepted")
	}
	secureURL, _ := url.Parse("https://partner.example.invalid")
	client.BaseURL = secureURL
	if _, err := client.NewSignedRequest(context.Background(), "POST", "/../secrets", nil); err == nil {
		t.Fatal("path traversal accepted")
	}
}
