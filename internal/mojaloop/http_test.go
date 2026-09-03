package mojaloop

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallbackHandlerRejectsUnsignedCallbackBeforeStore(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/transfers/transfer-001", nil)
	request.Header.Set("FSPIOP-Source", "PARTNER01")
	request.Header.Set("FSPIOP-Destination", "FMMBE")
	response := httptest.NewRecorder()
	CallbackHandler{VerificationKey: &key.PublicKey, ExpectedSource: "PARTNER01", ExpectedDestination: "FMMBE"}.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestCallbackHandlerRejectsWrongMethodAndPath(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/transfers/transfer-001", nil)
	response := httptest.NewRecorder()
	CallbackHandler{}.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestCallbackHandlerRejectsRouteOutsideConfiguredProfile(t *testing.T) {
	handler := CallbackHandler{ExpectedTransferPathPrefix: "/callbacks/transfers/"}
	request := httptest.NewRequest(http.MethodPut, "/transfers/transfer-0001", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func signedQuoteRequest(t *testing.T, key *rsa.PrivateKey, quoteID string, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPut, "/quotes/"+quoteID, strings.NewReader(string(body)))
	signature, err := SignRequestWithKeyID(request.Method, request.URL.RequestURI(), "PARTNER01", "FMMBE", body, key, "RS256", "partner-kid-1")
	if err != nil {
		t.Fatalf("sign quote callback: %v", err)
	}
	AddSignatureHeaders(request, signature, "PARTNER01", "FMMBE")
	return request
}

func quoteTestHandler(key *rsa.PublicKey) QuoteCallbackHandler {
	return QuoteCallbackHandler{VerificationKey: key, ExpectedSource: "PARTNER01", ExpectedDestination: "FMMBE", ExpectedVerificationKeyID: "partner-kid-1"}
}

func TestQuoteCallbackHandlerRejectsUnsignedCallback(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/quotes/quote-001", strings.NewReader(`{"quoteId":"quote-001"}`))
	request.Header.Set("FSPIOP-Source", "PARTNER01")
	request.Header.Set("FSPIOP-Destination", "FMMBE")
	response := httptest.NewRecorder()
	quoteTestHandler(&key.PublicKey).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestQuoteCallbackHandlerRejectsWrongMethodAndPath(t *testing.T) {
	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/quotes/quote-001"},
		{http.MethodPut, "/transfers/transfer-001"},
	} {
		request := httptest.NewRequest(target.method, target.path, nil)
		response := httptest.NewRecorder()
		QuoteCallbackHandler{}.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status = %d, want %d", target.method, target.path, response.Code, http.StatusNotFound)
		}
	}
}

func TestQuoteCallbackHandlerRejectsRouteOutsideConfiguredProfile(t *testing.T) {
	handler := QuoteCallbackHandler{ExpectedQuotePathPrefix: "/callbacks/quotes/"}
	request := httptest.NewRequest(http.MethodPut, "/quotes/quote-001", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestQuoteCallbackHandlerRejectsUnexpectedSource(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"quoteId":"quote-001"}`)
	request := signedQuoteRequest(t, key, "quote-001", body)
	request.Header.Set("FSPIOP-Source", "INTRUDER")
	// Re-sign so the signature itself is valid for the mutated headers.
	signature, err := SignRequestWithKeyID(request.Method, request.URL.RequestURI(), "INTRUDER", "FMMBE", body, key, "RS256", "partner-kid-1")
	if err != nil {
		t.Fatal(err)
	}
	AddSignatureHeaders(request, signature, "INTRUDER", "FMMBE")
	response := httptest.NewRecorder()
	quoteTestHandler(&key.PublicKey).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestQuoteCallbackHandlerFailsClosedWithoutStore(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	request := signedQuoteRequest(t, key, "quote-001", []byte(`{"quoteId":"quote-001"}`))
	response := httptest.NewRecorder()
	quoteTestHandler(&key.PublicKey).ServeHTTP(response, request)
	// With no durable store the handler cannot correlate the callback with a
	// local quote; it fails closed rather than inventing state.
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body)
	}
}

func TestQuoteCallbackHandlerRejectsTamperedBody(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	request := signedQuoteRequest(t, key, "quote-001", []byte(`{"quoteId":"quote-001"}`))
	// Replace the body after signing: the signature no longer binds.
	request = httptest.NewRequest(http.MethodPut, "/quotes/quote-001", strings.NewReader(`{"quoteId":"quote-999"}`))
	signature, signErr := SignRequestWithKeyID(request.Method, request.URL.RequestURI(), "PARTNER01", "FMMBE", []byte(`{"quoteId":"quote-001"}`), key, "RS256", "partner-kid-1")
	if signErr != nil {
		t.Fatal(signErr)
	}
	AddSignatureHeaders(request, signature, "PARTNER01", "FMMBE")
	response := httptest.NewRecorder()
	quoteTestHandler(&key.PublicKey).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}
