package mojaloop

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type CallbackHandler struct {
	Store                      *CallbackStore
	VerificationKey            *rsa.PublicKey
	ExpectedSource             string
	ExpectedDestination        string
	ExpectedTransferPathPrefix string
}

func (handler CallbackHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	pathPrefix := handler.ExpectedTransferPathPrefix
	if pathPrefix == "" {
		pathPrefix = "/transfers/"
	}
	if request.Method != http.MethodPut || !strings.HasPrefix(request.URL.Path, pathPrefix) {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	transferID := strings.TrimPrefix(request.URL.Path, pathPrefix)
	if transferID == "" || strings.Contains(transferID, "/") {
		http.Error(response, "invalid transfer path", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		http.Error(response, "request body unavailable", http.StatusBadRequest)
		return
	}
	if err := VerifyRequest(request.Method, request.URL.RequestURI(), request.Header.Get("FSPIOP-Source"), request.Header.Get("FSPIOP-Destination"), body, request.Header.Get(signatureHeader), handler.VerificationKey); err != nil {
		http.Error(response, "invalid FSPIOP signature", http.StatusUnauthorized)
		return
	}
	if handler.ExpectedSource != "" && request.Header.Get("FSPIOP-Source") != handler.ExpectedSource {
		http.Error(response, "unexpected source", http.StatusForbidden)
		return
	}
	if handler.ExpectedDestination != "" && request.Header.Get("FSPIOP-Destination") != handler.ExpectedDestination {
		http.Error(response, "unexpected destination", http.StatusForbidden)
		return
	}
	var callback TransferCallback
	if err := json.Unmarshal(body, &callback); err != nil {
		http.Error(response, "invalid callback JSON", http.StatusBadRequest)
		return
	}
	if callback.TransferID != transferID {
		http.Error(response, "transfer path and body differ", http.StatusConflict)
		return
	}
	if handler.Store == nil {
		http.Error(response, "callback store unavailable", http.StatusServiceUnavailable)
		return
	}
	_, duplicate, err := handler.Store.ApplyCallback(request.Context(), callback, body)
	if err != nil {
		if errors.Is(err, ErrTransferIdentityChange) || errors.Is(err, ErrInvalidTransferState) || strings.Contains(err.Error(), "terminal callback") {
			http.Error(response, "callback conflicts with retained transfer state", http.StatusConflict)
			return
		}
		http.Error(response, "callback persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(struct {
		TransferID string `json:"transferId"`
		Duplicate  bool   `json:"duplicate"`
	}{TransferID: callback.TransferID, Duplicate: duplicate})
}
