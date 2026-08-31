package cvffapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envLookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func localGatedEnv(root string) map[string]string {
	return map[string]string{
		envStorageBackend: "local-gated",
		envAllowLocal:     "true",
		envLocalRoot:      root,
	}
}

func TestNewBlobStoreFailClosedWithoutBackend(t *testing.T) {
	if _, err := NewBlobStore(context.Background(), envLookup(map[string]string{})); err == nil {
		t.Fatal("absent backend accepted")
	}
	if _, err := NewBlobStore(context.Background(), envLookup(map[string]string{envStorageBackend: "ftp"})); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestLocalGatedRequiresExplicitOptIn(t *testing.T) {
	root := t.TempDir()
	if _, err := NewBlobStore(context.Background(), envLookup(map[string]string{
		envStorageBackend: "local-gated",
		envLocalRoot:      root,
	})); err == nil {
		t.Fatal("local backend accepted without BLUEECONOMY_ALLOW_LOCAL_STORAGE=true")
	}
	if _, err := NewBlobStore(context.Background(), envLookup(map[string]string{
		envStorageBackend: "local-gated",
		envAllowLocal:     "true",
		envLocalRoot:      "relative/path",
	})); err == nil {
		t.Fatal("relative local root accepted")
	}
}

func TestLocalGatedPutAndIdempotency(t *testing.T) {
	root := t.TempDir()
	store, err := NewBlobStore(context.Background(), envLookup(localGatedEnv(root)))
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	if store.Backend() != "local-gated" {
		t.Fatalf("backend = %q", store.Backend())
	}
	key := DocumentStorageKey("cvff-app-001", sha256.Sum256([]byte("pdf-bytes")))
	if err := store.Put(context.Background(), key, "application/pdf", strings.NewReader("pdf-bytes"), 9); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Identical retry is a successful no-op (content-addressed idempotency).
	if err := store.Put(context.Background(), key, "application/pdf", strings.NewReader("pdf-bytes"), 9); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if err := store.Put(context.Background(), key, "application/pdf", strings.NewReader("other-bytes"), 11); err == nil {
		t.Fatal("conflicting content at an existing key accepted")
	}
	retained, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read stored object: %v", err)
	}
	if string(retained) != "pdf-bytes" {
		t.Fatalf("stored content = %q", retained)
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("stat stored object: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("stored object mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestLocalGatedRejectsEscapingKeys(t *testing.T) {
	store, err := NewBlobStore(context.Background(), envLookup(localGatedEnv(t.TempDir())))
	if err != nil {
		t.Fatalf("new local store: %v", err)
	}
	for _, key := range []string{"../escape", "a//b", "a/./b", "/absolute", "a/\x01b"} {
		if err := store.Put(context.Background(), key, "application/pdf", strings.NewReader("x"), 1); err == nil {
			t.Fatalf("escaping key %q accepted", key)
		}
	}
}

func TestS3ConfigFailClosed(t *testing.T) {
	base := map[string]string{
		envStorageBackend: "s3",
		envS3Bucket:       "cvff-documents-prod",
		envS3Region:       "us-east-1",
		envS3Secure:       "true",
		envAWSAccessKey:   "AKIAEXAMPLE",
		envAWSSecretKey:   "secret",
	}
	if _, err := NewBlobStore(context.Background(), envLookup(base)); err != nil {
		t.Fatalf("valid s3 config rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"no bucket":        func(env map[string]string) { delete(env, envS3Bucket) },
		"bucket like ip":   func(env map[string]string) { env[envS3Bucket] = "192.168.0.1" },
		"bad region":       func(env map[string]string) { env[envS3Region] = "Lagos" },
		"insecure aws":     func(env map[string]string) { env[envS3Secure] = "false" },
		"no credentials":   func(env map[string]string) { delete(env, envAWSAccessKey) },
		"credentialed url": func(env map[string]string) { env[envS3EndpointURL] = "https://user:pass@minio.example" },
	} {
		env := map[string]string{}
		for key, value := range base {
			env[key] = value
		}
		mutate(env)
		if _, err := NewBlobStore(context.Background(), envLookup(env)); err == nil {
			t.Fatalf("case %s: invalid s3 config accepted", name)
		}
	}
	// Insecure transport is permitted only against an explicit endpoint.
	insecure := map[string]string{}
	for key, value := range base {
		insecure[key] = value
	}
	insecure[envS3Secure] = "false"
	insecure[envS3EndpointURL] = "http://minio.cluster.local:9000"
	if _, err := NewBlobStore(context.Background(), envLookup(insecure)); err != nil {
		t.Fatalf("gated MinIO config rejected: %v", err)
	}
}

func TestS3PutSignsAndStores(t *testing.T) {
	var captured struct {
		method        string
		path          string
		authorization string
		amzDate       string
		payloadHash   string
		body          string
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured.method = request.Method
		captured.path = request.URL.Path
		captured.authorization = request.Header.Get("Authorization")
		captured.amzDate = request.Header.Get("X-Amz-Date")
		captured.payloadHash = request.Header.Get("X-Amz-Content-Sha256")
		body, _ := io.ReadAll(request.Body)
		captured.body = string(body)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	lookup := envLookup(map[string]string{
		envStorageBackend: "s3",
		envS3Bucket:       "cvff-docs",
		envS3Region:       "us-east-1",
		envS3Secure:       "false",
		envS3EndpointURL:  server.URL,
		envAWSAccessKey:   "AKIAEXAMPLE",
		envAWSSecretKey:   "secret",
	})
	resolved, err := NewBlobStore(context.Background(), lookup)
	if err != nil {
		t.Fatalf("new s3 store: %v", err)
	}
	store := resolved.(*s3Store)
	store.now = func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) }
	key := DocumentStorageKey("cvff-app-001", sha256.Sum256([]byte("pdf-bytes")))
	if err := store.Put(context.Background(), key, "application/pdf", strings.NewReader("pdf-bytes"), 9); err != nil {
		t.Fatalf("s3 put: %v", err)
	}
	if captured.method != http.MethodPut || captured.path != "/cvff-docs/"+key {
		t.Fatalf("request = %s %s", captured.method, captured.path)
	}
	if captured.body != "pdf-bytes" {
		t.Fatalf("body = %q", captured.body)
	}
	if !strings.HasPrefix(captured.authorization, "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20260828/us-east-1/s3/aws4_request, SignedHeaders=") {
		t.Fatalf("authorization header malformed: %q", captured.authorization)
	}
	if captured.amzDate != "20260828T120000Z" {
		t.Fatalf("amz date = %q", captured.amzDate)
	}
	wantHash := sha256.Sum256([]byte("pdf-bytes"))
	if captured.payloadHash != hexEncodeLower(wantHash[:]) {
		t.Fatalf("payload hash = %q", captured.payloadHash)
	}
	// Deterministic signing: same coordinates and instant, same signature.
	signatureOf := func(header string) string {
		marker := ", Signature="
		index := strings.Index(header, marker)
		if index < 0 {
			t.Fatalf("authorization header missing signature: %q", header)
		}
		return header[index+len(marker):]
	}
	first := signatureOf(captured.authorization)
	if err := store.Put(context.Background(), key, "application/pdf", strings.NewReader("pdf-bytes"), 9); err != nil {
		t.Fatalf("second s3 put: %v", err)
	}
	if signatureOf(captured.authorization) != first {
		t.Fatal("SigV4 signing is not deterministic for identical inputs")
	}
}

func hexEncodeLower(bytes []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(bytes)*2)
	for _, octet := range bytes {
		out = append(out, digits[octet>>4], digits[octet&0x0F])
	}
	return string(out)
}

func TestS3Escape(t *testing.T) {
	if escaped := s3EscapeKey("a b/ç.pdf"); escaped != "a%20b/%C3%A7.pdf" {
		t.Fatalf("escape = %q", escaped)
	}
}

func TestADLSConfigFailClosed(t *testing.T) {
	validKey := base64.StdEncoding.EncodeToString([]byte("account-key-material-32bytes!!"))
	base := map[string]string{
		envStorageBackend:   "adls",
		envAzureCloud:       "AzureUSGovernment",
		envStorageAccount:   "cvffsegregrated",
		envStorageContainer: "documents",
		envStorageKey:       validKey,
	}
	if _, err := NewBlobStore(context.Background(), envLookup(base)); err != nil {
		t.Fatalf("valid adls config rejected: %v", err)
	}
	// Historical alias keeps resolving.
	alias := map[string]string{}
	for key, value := range base {
		alias[key] = value
	}
	alias[envStorageBackend] = "adls-gen2"
	if _, err := NewBlobStore(context.Background(), envLookup(alias)); err != nil {
		t.Fatalf("adls-gen2 alias rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"no cloud":      func(env map[string]string) { delete(env, envAzureCloud) },
		"bad cloud":     func(env map[string]string) { env[envAzureCloud] = "AzureChinaCloud" },
		"bad account":   func(env map[string]string) { env[envStorageAccount] = "UPPERCASE" },
		"bad container": func(env map[string]string) { env[envStorageContainer] = "-leading-hyphen" },
		"no key":        func(env map[string]string) { delete(env, envStorageKey) },
		"key not b64":   func(env map[string]string) { env[envStorageKey] = "!!!not-base64!!!" },
	} {
		env := map[string]string{}
		for key, value := range base {
			env[key] = value
		}
		mutate(env)
		if _, err := NewBlobStore(context.Background(), envLookup(env)); err == nil {
			t.Fatalf("case %s: invalid adls config accepted", name)
		}
	}
}

func TestADLSPutSequence(t *testing.T) {
	var calls []struct {
		method string
		query  string
		auth   string
		body   string
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		calls = append(calls, struct {
			method string
			query  string
			auth   string
			body   string
		}{request.Method, request.URL.RawQuery, request.Header.Get("Authorization"), string(body)})
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	validKey := base64.StdEncoding.EncodeToString([]byte("account-key-material-32bytes!!"))
	resolved, err := NewBlobStore(context.Background(), envLookup(map[string]string{
		envStorageBackend:   "adls",
		envAzureCloud:       "AzureUSGovernment",
		envStorageAccount:   "cvffsegregrated",
		envStorageContainer: "documents",
		envStorageKey:       validKey,
	}))
	if err != nil {
		t.Fatalf("new adls store: %v", err)
	}
	store := resolved.(*adlsStore)
	store.httpClient = server.Client()
	store.now = func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) }
	// Repoint the store at the test server while keeping the signed resource
	// path stable: the suffix only affects the host, not the signature input.
	parts := strings.Split(strings.TrimPrefix(server.URL, "http://"), ":")
	store.suffix = parts[0]
	if len(parts) == 2 {
		store.suffix = strings.TrimPrefix(server.URL, "http://")
	}
	key := DocumentStorageKey("cvff-app-001", sha256.Sum256([]byte("pdf-bytes")))
	// blobURL builds https URLs; the test server is plain HTTP. Swap the
	// scheme through a transport that rewrites requests.
	store.httpClient = &http.Client{Transport: rewriteTransport{base: http.DefaultTransport, target: server.URL}}
	if err := store.Put(context.Background(), key, "application/pdf", strings.NewReader("pdf-bytes"), 9); err != nil {
		t.Fatalf("adls put: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("adls calls = %d, want create/append/flush", len(calls))
	}
	if calls[0].method != http.MethodPut || !strings.Contains(calls[0].query, "resource=file") {
		t.Fatalf("create call malformed: %+v", calls[0])
	}
	if calls[1].method != http.MethodPatch || !strings.Contains(calls[1].query, "action=append") || calls[1].body != "pdf-bytes" {
		t.Fatalf("append call malformed: %+v", calls[1])
	}
	if calls[2].method != http.MethodPatch || !strings.Contains(calls[2].query, "action=flush") || !strings.Contains(calls[2].query, "position=9") {
		t.Fatalf("flush call malformed: %+v", calls[2])
	}
	for index, call := range calls {
		if !strings.HasPrefix(call.auth, "SharedKey cvffsegregrated:") {
			t.Fatalf("call %d missing shared-key authorization: %q", index, call.auth)
		}
	}
}

// rewriteTransport rewrites https requests to the plain-HTTP test server.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (transport rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(transport.target, "http://")
	clone.Host = clone.URL.Host
	return transport.base.RoundTrip(clone)
}
