package mojaloop

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
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
