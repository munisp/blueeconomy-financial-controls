package mojaloop

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
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

func newTestClient(t *testing.T, baseURL string, httpClient *http.Client) Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return Client{
		BaseURL:            parsed,
		HTTPClient:         httpClient,
		Source:             "FMMBE",
		Destination:        "PARTNER01",
		SigningKey:         key,
		SigningKeyID:       "test-key-1",
		SignatureAlgorithm: "RS256",
		MaxAttempts:        3,
		RetryBackoff:       time.Millisecond,
	}
}

func TestDoFailClosedWithoutHTTPClient(t *testing.T) {
	client := newTestClient(t, "https://partner.example.invalid", nil)
	if _, err := client.Do(context.Background(), "POST", "/transfers", nil); err == nil {
		t.Fatal("nil HTTP client fell back instead of failing closed")
	}
}

func TestDoRetriesTransient5xx(t *testing.T) {
	var calls int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client())
	response, err := client.Do(context.Background(), "POST", "/transfers", []byte(`{"transferId":"t-1"}`))
	if err != nil {
		t.Fatalf("transient 5xx not retried to success: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestDoExhaustsAttemptsOnPersistent5xx(t *testing.T) {
	var calls int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client())
	response, err := client.Do(context.Background(), "GET", "/transfers/t-1", nil)
	if err != nil {
		t.Fatalf("final 5xx response must be returned, got error: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestDoNeverRetries4xx(t *testing.T) {
	var calls int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client())
	response, err := client.Do(context.Background(), "POST", "/transfers", []byte(`{}`))
	if err != nil {
		t.Fatalf("4xx must be returned, not retried: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on 4xx)", got)
	}
}

func TestDoRetriesNetworkError(t *testing.T) {
	// Start a TLS server to learn a free port, then close it so every dial
	// fails with a network error.
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	client := newTestClient(t, address, server.Client())
	if _, err := client.Do(context.Background(), "GET", "/transfers/t-1", nil); err == nil {
		t.Fatal("network error against an unreachable peer succeeded")
	}
}

func TestDoHonorsRequestTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	slow := server.Client()
	slow.Timeout = 50 * time.Millisecond
	client := newTestClient(t, server.URL, slow)
	start := time.Now()
	if _, err := client.Do(context.Background(), "GET", "/transfers/t-1", nil); err == nil {
		t.Fatal("request past the client timeout succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout-bound client waited %v", elapsed)
	}
}

func TestDoHonorsContextCancellation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client())
	client.RetryBackoff = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.Do(ctx, "GET", "/transfers/t-1", nil); err == nil {
		t.Fatal("cancelled context did not stop the retry loop")
	}
}

func TestNewClientFromConfig(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	baseURL, _ := url.Parse("https://partner.example.invalid")
	config := Config{BaseURL: baseURL, Source: "FMMBE", Destination: "PARTNER01", SigningKeyID: "kid-1", SignatureAlgorithm: "RS256", RequestTimeout: 10 * time.Second}
	client, err := NewClient(config, key)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if client.HTTPClient == nil || client.HTTPClient.Timeout != 10*time.Second {
		t.Fatalf("explicit timeout-bound HTTP client not constructed: %+v", client.HTTPClient)
	}
	if client.MaxAttempts < 1 || client.RetryBackoff <= 0 {
		t.Fatal("retry policy defaults not applied")
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.BaseURL = nil },
		func(c *Config) { c.RequestTimeout = 0 },
	} {
		broken := config
		mutate(&broken)
		if _, err := NewClient(broken, key); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	if _, err := NewClient(config, nil); err == nil {
		t.Fatal("nil signing key accepted")
	}
}
