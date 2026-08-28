package cvffapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func validConfigEnv() map[string]string {
	return map[string]string{
		EnvListenAddr:        "0.0.0.0:8443",
		EnvDatabaseURL:       "postgres://cvff:secret@postgres:5432/cvff",
		EnvKeycloakIssuer:    "https://keycloak.example/realms/blueeconomy-cvff",
		EnvKeycloakJWKS:      "https://keycloak.example/realms/blueeconomy-cvff/protocol/openid-connect/certs",
		EnvJWTAudience:       "beneficiary-portal",
		EnvAVScanURL:         "https://avscan.cluster.local/scan",
		EnvMaxDocBytes:       "10485760",
		EnvMaxDocsPerApp:     "12",
		EnvDocContentTypes:   "application/pdf,image/png,image/jpeg",
		EnvTemporalHostPort:  "temporal:7233",
		EnvTemporalNamespace: "blueeconomy",
		EnvTemporalTaskQueue: "cvff-disbursement",
	}
}

func TestConfigFromEnvAcceptsCompleteEnvironment(t *testing.T) {
	config, err := ConfigFromEnv(envLookup(validConfigEnv()))
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if config.Limits.MaxDocumentBytes != 10485760 || config.Limits.MaxDocumentsPerApplication != 12 ||
		len(config.Limits.DocumentContentTypes) != 3 {
		t.Fatalf("limits mismatch: %+v", config.Limits)
	}
	if config.Keycloak.Issuer != "https://keycloak.example/realms/blueeconomy-cvff" || config.Keycloak.Audience != "beneficiary-portal" {
		t.Fatalf("keycloak config mismatch: %+v", config.Keycloak)
	}
}

func TestConfigFromEnvFailClosed(t *testing.T) {
	for _, name := range []string{
		EnvListenAddr, EnvDatabaseURL, EnvKeycloakIssuer, EnvKeycloakJWKS, EnvJWTAudience,
		EnvAVScanURL, EnvMaxDocBytes, EnvMaxDocsPerApp, EnvDocContentTypes,
		EnvTemporalHostPort, EnvTemporalNamespace, EnvTemporalTaskQueue,
	} {
		env := validConfigEnv()
		delete(env, name)
		if _, err := ConfigFromEnv(envLookup(env)); err == nil {
			t.Fatalf("missing %s accepted", name)
		}
		env = validConfigEnv()
		env[name] = "  padded  "
		if _, err := ConfigFromEnv(envLookup(env)); err == nil {
			t.Fatalf("non-canonical %s accepted", name)
		}
	}
}

func TestConfigFromEnvRejectsInvalidValues(t *testing.T) {
	cases := map[string]func(map[string]string){
		"bytes not int":  func(env map[string]string) { env[EnvMaxDocBytes] = "ten-mb" },
		"bytes zero":     func(env map[string]string) { env[EnvMaxDocBytes] = "0" },
		"quota not int":  func(env map[string]string) { env[EnvMaxDocsPerApp] = "many" },
		"empty type":     func(env map[string]string) { env[EnvDocContentTypes] = "application/pdf,,image/png" },
		"malformed type": func(env map[string]string) { env[EnvDocContentTypes] = "PDF" },
		"insecure issuer": func(env map[string]string) {
			env[EnvKeycloakIssuer] = "http://keycloak.example/realms/blueeconomy-cvff"
		},
	}
	for name, mutate := range cases {
		env := validConfigEnv()
		mutate(env)
		if _, err := ConfigFromEnv(envLookup(env)); err == nil {
			t.Fatalf("case %s accepted", name)
		}
	}
}

func TestNewHTTPScannerFailClosed(t *testing.T) {
	for _, rawURL := range []string{"", "  ", "ftp://scanner/scan", " https://padded.example/scan "} {
		if _, err := NewHTTPScanner(rawURL); err == nil {
			t.Fatalf("scanner URL %q accepted", rawURL)
		}
	}
}

func TestHTTPScannerVerdicts(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := make([]byte, request.ContentLength)
		_, _ = request.Body.Read(body)
		received = string(body)
		switch request.Header.Get("X-Content-Name") {
		case "clean.pdf":
			writer.WriteHeader(http.StatusOK)
		case "infected.pdf":
			writer.WriteHeader(http.StatusNotAcceptable)
		default:
			writer.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer server.Close()
	scanner, err := NewHTTPScanner(server.URL)
	if err != nil {
		t.Fatalf("new scanner: %v", err)
	}
	if err := scanner.Scan(context.Background(), "clean.pdf", []byte("pdf-bytes")); err != nil {
		t.Fatalf("clean content rejected: %v", err)
	}
	if received != "pdf-bytes" {
		t.Fatalf("scanner received %q", received)
	}
	if err := scanner.Scan(context.Background(), "infected.pdf", []byte("eicar")); !errors.Is(err, ErrDocumentInfected) {
		t.Fatalf("infected verdict = %v", err)
	}
	if err := scanner.Scan(context.Background(), "broken.pdf", []byte("x")); !errors.Is(err, ErrScannerUnavailable) {
		t.Fatalf("unavailable verdict = %v", err)
	}
	// Unreachable endpoint is a fail-closed unavailable verdict, never a skip.
	unreachable, err := NewHTTPScanner("http://127.0.0.1:1/scan")
	if err != nil {
		t.Fatalf("new unreachable scanner: %v", err)
	}
	if err := unreachable.Scan(context.Background(), "clean.pdf", []byte("x")); !errors.Is(err, ErrScannerUnavailable) {
		t.Fatalf("unreachable scanner verdict = %v", err)
	}
}

func sha256TestDigest(content string) [sha256.Size]byte {
	return sha256.Sum256([]byte(content))
}

func TestDocumentStorageKeyIsContentAddressed(t *testing.T) {
	first := DocumentStorageKey("cvff-app-001", sha256TestDigest("same-bytes"))
	second := DocumentStorageKey("cvff-app-001", sha256TestDigest("same-bytes"))
	third := DocumentStorageKey("cvff-app-001", sha256TestDigest("other-bytes"))
	if first != second {
		t.Fatal("identical content produced different keys")
	}
	if first == third {
		t.Fatal("different content produced identical keys")
	}
	if !strings.HasPrefix(first, "cvff-documents/cvff-app-001/") || len(first) != len("cvff-documents/cvff-app-001/")+64 {
		t.Fatalf("key shape = %q", first)
	}
	if err := validateObjectKey(first); err != nil {
		t.Fatalf("content-addressed key fails validation: %v", err)
	}
}
