package mojaloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/pbac"
)

// PayoutHandler is the operator-facing entry point of the outbound leg:
// POST /payouts initiates one FSPIOP quote (POST /quotes) toward the payee
// FSP. The endpoint is gated exactly like the repo's other money-path APIs:
// a verified Keycloak bearer token identifies the caller and the embedded
// PBAC policy pack (deny-by-default) authorizes resource mojaloop-payout /
// action initiate. The quote row is persisted before the wire call so a
// callback can never arrive without durable local state; the body hash makes
// operator retries idempotent.
type PayoutHandler struct {
	Store         *OutboundStore
	Client        *Client
	PayerFSP      string
	PayeeFSP      string
	Authenticator cvffapi.Authenticator
	Policy        *pbac.Enforcer
	// NewQuoteID mints quote IDs; nil uses a random UUID.
	NewQuoteID func() string
}

// PayoutRequest is the POST /payouts body.
type PayoutRequest struct {
	PayerPartyIDType  string `json:"payerPartyIdType"`
	PayerPartyIDValue string `json:"payerPartyIdentifier"`
	PayeePartyIDType  string `json:"payeePartyIdType"`
	PayeePartyIDValue string `json:"payeePartyIdentifier"`
	Amount            string `json:"amount"`
	Currency          string `json:"currency"`
}

func (handler PayoutHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/payouts" {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	if handler.Authenticator == nil || handler.Policy == nil {
		http.Error(response, "outbound rail unavailable", http.StatusServiceUnavailable)
		return
	}
	principal, err := handler.Authenticator.Authenticate(request.Context(), request.Header.Get("Authorization"))
	if err != nil {
		http.Error(response, "bearer token is absent or unverifiable", http.StatusUnauthorized)
		return
	}
	allowed := handler.Policy.Allow(request.Context(), pbac.Input{
		Principal:      pbac.Principal{Subject: principal.Subject, Roles: principal.Roles, Clearance: principal.Clearance, TenantID: principal.TenantID},
		TenantID:       principal.TenantID,
		Resource:       "mojaloop-payout",
		Action:         "initiate",
		Classification: "FIDUCIARY_SEGREGATED",
	})
	if !allowed {
		http.Error(response, "bearer token is not authorized to initiate payouts", http.StatusForbidden)
		return
	}
	if handler.Store == nil || handler.Client == nil {
		http.Error(response, "outbound rail unavailable", http.StatusServiceUnavailable)
		return
	}
	var payout PayoutRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&payout); err != nil {
		http.Error(response, "invalid payout JSON", http.StatusBadRequest)
		return
	}
	if payout.Amount == "" || payout.Currency == "" || payout.PayerPartyIDValue == "" || payout.PayeePartyIDValue == "" {
		http.Error(response, "payout amount, currency and party identifiers are required", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(payout.Amount+payout.Currency, " \t\n\r") || len(payout.Amount) > 64 || len(payout.Currency) != 3 {
		http.Error(response, "payout amount or currency is not canonical", http.StatusBadRequest)
		return
	}
	quoteID := uuid.NewString()
	if handler.NewQuoteID != nil {
		quoteID = handler.NewQuoteID()
	}
	quoteBody := QuoteRequestBody{
		QuoteID:       quoteID,
		TransactionID: quoteID,
		Payer:         QuoteParty{PartyIDType: payout.PayerPartyIDType, PartyIDValue: payout.PayerPartyIDValue, FSPID: handler.PayerFSP},
		Payee:         QuoteParty{PartyIDType: payout.PayeePartyIDType, PartyIDValue: payout.PayeePartyIDValue, FSPID: handler.PayeeFSP},
		Amount:        QuoteAmount{Amount: payout.Amount, Currency: payout.Currency},
	}
	quoteBody.TransactionType.Scenario = "TRANSFER"
	quoteBody.TransactionType.Initiator = "PAYER"
	quoteBody.TransactionType.InitiatorType = "CONSUMER"
	body, err := json.Marshal(quoteBody)
	if err != nil {
		http.Error(response, "marshal quote request", http.StatusInternalServerError)
		return
	}
	quote := OutboundQuote{
		QuoteID:  quoteID,
		PayerFSP: handler.PayerFSP,
		PayeeFSP: handler.PayeeFSP,
		Amount:   payout.Amount,
		Currency: payout.Currency,
		State:    QuoteRequested,
	}
	duplicate, err := handler.Store.CreateQuote(request.Context(), quote, body)
	if err != nil {
		if errors.Is(err, ErrQuoteIdentityChange) {
			http.Error(response, "quote identity conflicts with retained state", http.StatusConflict)
			return
		}
		http.Error(response, "quote persistence unavailable", http.StatusServiceUnavailable)
		return
	}
	if !duplicate {
		wireResponse, err := handler.Client.PostQuotes(request.Context(), quoteID, body)
		if err != nil {
			http.Error(response, fmt.Sprintf("POST /quotes failed: %v", err), http.StatusBadGateway)
			return
		}
		defer wireResponse.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(wireResponse.Body, 1<<20))
		if wireResponse.StatusCode < 200 || wireResponse.StatusCode >= 300 {
			http.Error(response, fmt.Sprintf("POST /quotes answered status %d", wireResponse.StatusCode), http.StatusBadGateway)
			return
		}
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(struct {
		QuoteID   string `json:"quoteId"`
		Duplicate bool   `json:"duplicate"`
	}{QuoteID: quoteID, Duplicate: duplicate})
}
