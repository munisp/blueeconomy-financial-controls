package mojaloop

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type CallbackHandler struct {
	Store *CallbackStore
	// Outbound, when set (full mode), lets the handler route callbacks for
	// transfers this rail initiated: PREPARED → COMMITTED (fulfilment
	// verified against the quote's ILP condition) / ABORTED. Transfers not
	// found in the outbound store fall through to the inbound policy.
	Outbound                   *OutboundStore
	VerificationKey            *rsa.PublicKey
	ExpectedSource             string
	ExpectedDestination        string
	ExpectedVerificationKeyID  string
	ExpectedTransferPathPrefix string
}

// QuoteCallbackHandler terminates the inbound PUT /quotes/{id} leg of the
// outbound flow: the payee FSP's signed quote response is verified
// (FSPIOP JWS signature, key ID, source and destination — forged or
// misrouted callbacks fail closed with 401/403), validated against the
// locally REQUESTED quote (exact amount/currency match, well-formed ILP
// condition) and durably persisted. Exact replays are idempotent; an
// unsolicited quote response (no correlating local quote) fails closed with
// 404 rather than inventing state. On a fresh response the handler posts
// POST /transfers with the ILP packet and condition from the quote.
type QuoteCallbackHandler struct {
	Store                     *OutboundStore
	Client                    *Client
	PayerFSP                  string
	PayeeFSP                  string
	VerificationKey           *rsa.PublicKey
	ExpectedSource            string
	ExpectedDestination       string
	ExpectedVerificationKeyID string
	ExpectedQuotePathPrefix   string
	// NewTransferID mints transfer IDs for the POST /transfers leg; nil uses
	// a random UUID. Tests inject a deterministic generator.
	NewTransferID func() string
}

func (handler QuoteCallbackHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	pathPrefix := handler.ExpectedQuotePathPrefix
	if pathPrefix == "" {
		pathPrefix = "/quotes/"
	}
	if request.Method != http.MethodPut || !strings.HasPrefix(request.URL.Path, pathPrefix) {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	quoteID := strings.TrimPrefix(request.URL.Path, pathPrefix)
	if quoteID == "" || strings.Contains(quoteID, "/") {
		http.Error(response, "invalid quote path", http.StatusBadRequest)
		return
	}
	spanCtx, span := fspiopSpan(request.Context(), request.Header, "mojaloop.callback.quote", "fspiop.quote_id", quoteID)
	defer span.End()
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		http.Error(response, "request body unavailable", http.StatusBadRequest)
		return
	}
	if err := verifySpan(spanCtx, func() error {
		return VerifyRequestWithKeyID(request.Method, request.URL.RequestURI(), request.Header.Get("FSPIOP-Source"), request.Header.Get("FSPIOP-Destination"), body, request.Header.Get(signatureHeader), handler.VerificationKey, handler.ExpectedVerificationKeyID)
	}); err != nil {
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
	if handler.Store == nil {
		http.Error(response, "quote store unavailable", http.StatusServiceUnavailable)
		return
	}
	var quoteResponse QuoteResponse
	if err := json.Unmarshal(body, &quoteResponse); err != nil {
		http.Error(response, "invalid quote response JSON", http.StatusBadRequest)
		return
	}
	quote, duplicate, err := handler.Store.ApplyQuoteCallback(request.Context(), quoteID, quoteResponse, body)
	if err != nil {
		if errors.Is(err, ErrUnknownQuote) {
			http.Error(response, "quote callback does not correlate with a local quote", http.StatusNotFound)
			return
		}
		if strings.Contains(err.Error(), "already recorded") || strings.Contains(err.Error(), "does not match") || errors.Is(err, ErrMalformedCondition) {
			http.Error(response, "quote response conflicts with retained quote state", http.StatusConflict)
			return
		}
		http.Error(response, "quote persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(struct {
		QuoteID   string `json:"quoteId"`
		Duplicate bool   `json:"duplicate"`
	}{QuoteID: quoteID, Duplicate: duplicate})
	// A fresh quote response triggers the transfer leg: POST /transfers with
	// the payee's ILP packet and condition, after the durable PREPARED row is
	// in place. The Hub's PUT /transfers/{id} callback then commits or aborts.
	if !duplicate && quote.Response != nil && handler.Client != nil {
		if err := handler.initiateTransfer(request.Context(), quote); err != nil {
			span.RecordError(err)
			log.Printf("mojaloop: quote %s response persisted but POST /transfers failed: %v", quoteID, err)
		}
	}
}

// initiateTransfer derives the POST /transfers leg from a received quote:
// the ILP packet and condition come verbatim from the validated payee
// response (never fabricated), the amount is the quote's transfer amount.
func (handler QuoteCallbackHandler) initiateTransfer(ctx context.Context, quote OutboundQuote) error {
	if quote.Response == nil {
		return errors.New("quote response is required")
	}
	transferID := uuid.NewString()
	if handler.NewTransferID != nil {
		transferID = handler.NewTransferID()
	}
	requestBody := TransferRequestBody{
		TransferID: transferID,
		PayerFSP:   handler.PayerFSP,
		PayeeFSP:   handler.PayeeFSP,
		Amount:     quote.Response.TransferAmount,
		ILPPacket:  quote.Response.ILPPacket,
		Condition:  quote.Response.Condition,
		Expiration: quote.Response.Expiration,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("marshal transfer request: %w", err)
	}
	transfer := OutboundTransfer{
		TransferID: transferID,
		QuoteID:    quote.QuoteID,
		PayerFSP:   handler.PayerFSP,
		PayeeFSP:   handler.PayeeFSP,
		Amount:     quote.Response.TransferAmount.Amount,
		Currency:   quote.Response.TransferAmount.Currency,
		ILPPacket:  quote.Response.ILPPacket,
		Condition:  quote.Response.Condition,
		State:      TransferPrepared,
	}
	duplicate, err := handler.Store.CreateTransfer(ctx, transfer, body)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	response, err := handler.Client.PostTransfers(ctx, body)
	if err != nil {
		return fmt.Errorf("post transfer: %w", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20)); err != nil {
		return fmt.Errorf("drain transfer response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("POST /transfers answered status %d", response.StatusCode)
	}
	return nil
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
	spanCtx, span := fspiopSpan(request.Context(), request.Header, "mojaloop.callback.transfer", "fspiop.transfer_id", transferID)
	defer span.End()
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		http.Error(response, "request body unavailable", http.StatusBadRequest)
		return
	}
	if err := verifySpan(spanCtx, func() error {
		return VerifyRequestWithKeyID(request.Method, request.URL.RequestURI(), request.Header.Get("FSPIOP-Source"), request.Header.Get("FSPIOP-Destination"), body, request.Header.Get(signatureHeader), handler.VerificationKey, handler.ExpectedVerificationKeyID)
	}); err != nil {
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
	if handler.Outbound != nil {
		if _, err := handler.Outbound.LookupOutboundTransfer(request.Context(), callback.TransferID); err == nil {
			handler.serveOutboundTransferCallback(response, request, callback, body)
			return
		} else if !errors.Is(err, ErrUnknownTransfer) {
			http.Error(response, "callback persistence unavailable", http.StatusServiceUnavailable)
			return
		}
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

// serveOutboundTransferCallback finalizes a transfer this rail initiated.
// The transition policy (PREPARED → COMMITTED/ABORTED, fulfilment verified
// against the ILP condition, terminal regression fail-closed) lives in
// ApplyOutboundTransferCallback.
func (handler CallbackHandler) serveOutboundTransferCallback(response http.ResponseWriter, request *http.Request, callback TransferCallback, body []byte) {
	_, duplicate, err := handler.Outbound.ApplyOutboundTransferCallback(request.Context(), callback, body)
	if err != nil {
		if errors.Is(err, ErrTransferIdentityChange) || errors.Is(err, ErrInvalidTransferState) || errors.Is(err, ErrInvalidPreparedState) || errors.Is(err, ErrInvalidFulfilment) || errors.Is(err, ErrMalformedCondition) || strings.Contains(err.Error(), "terminal") || strings.Contains(err.Error(), "fulfilment") {
			http.Error(response, "callback conflicts with retained outbound transfer state", http.StatusConflict)
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
