//go:build integration

package mojaloop

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvffapi"
	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
)

// loopbackPeerFSP is a TEST double for the payee FSP / switch: it answers
// signed POST /quotes and POST /transfers over TLS and calls the rail's
// signed callback endpoints with its own FSPIOP signatures. Production code
// paths never see it — it exists only so the integration test can drive the
// full outbound leg end to end against real PostgreSQL.
type loopbackPeerFSP struct {
	t           *testing.T
	key         *rsa.PrivateKey
	sourceID    string // peer FSPIOP id (PARTNER01)
	destID      string // rail FSPIOP id (FMMBE)
	server      *httptest.Server
	fulfilment  string
	condition   string
	callbacks   *httptest.Server // rail callback surface under test
	sawQuote    bool
	sawTransfer bool
}

func newLoopbackPeerFSP(t *testing.T) *loopbackPeerFSP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fulfilment, condition, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	peer := &loopbackPeerFSP{t: t, key: key, sourceID: "PARTNER01", destID: "FMMBE", fulfilment: fulfilment, condition: condition}
	mux := http.NewServeMux()
	mux.HandleFunc("/quotes/", peer.handlePostQuote)
	mux.HandleFunc("/transfers", peer.handlePostTransfer)
	peer.server = httptest.NewTLSServer(mux)
	t.Cleanup(peer.server.Close)
	return peer
}

// postSigned issues one signed FSPIOP call from the peer to the rail's
// callback surface.
func (peer *loopbackPeerFSP) postSigned(method, uri string, body []byte) *http.Response {
	peer.t.Helper()
	request, err := http.NewRequest(method, peer.callbacks.URL+uri, strings.NewReader(string(body)))
	if err != nil {
		peer.t.Fatal(err)
	}
	signature, err := SignRequestWithKeyID(method, uri, peer.sourceID, peer.destID, body, peer.key, "RS256", "partner-kid-1")
	if err != nil {
		peer.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	AddSignatureHeaders(request, signature, peer.sourceID, peer.destID)
	response, err := peer.callbacks.Client().Do(request)
	if err != nil {
		peer.t.Fatalf("peer callback %s %s: %v", method, uri, err)
	}
	return response
}

func (peer *loopbackPeerFSP) handlePostQuote(response http.ResponseWriter, request *http.Request) {
	quoteID := strings.TrimPrefix(request.URL.Path, "/quotes/")
	body, err := io.ReadAll(request.Body)
	if err != nil {
		peer.t.Fatalf("read quote: %v", err)
	}
	if err := VerifyRequestWithKeyID(request.Method, request.URL.RequestURI(), request.Header.Get("FSPIOP-Source"), request.Header.Get("FSPIOP-Destination"), body, request.Header.Get(signatureHeader), &peer.key.PublicKey, "rail-kid-1"); err != nil {
		// The rail signs with its own key; this harness reuses one keypair for
		// simplicity, so rail requests are verified against the same public key.
		http.Error(response, "bad signature", http.StatusUnauthorized)
		return
	}
	var quote QuoteRequestBody
	if err := json.Unmarshal(body, &quote); err != nil || quote.QuoteID != quoteID {
		http.Error(response, "bad quote", http.StatusBadRequest)
		return
	}
	peer.sawQuote = true
	response.WriteHeader(http.StatusAccepted)
	// Deliver the signed quote response callback with the ILP terms.
	quoteResponse := QuoteResponse{
		TransferAmount: quote.Amount,
		Expiration:     time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		ILPPacket:      "AYICbQAAAAAAA",
		Condition:      peer.condition,
	}
	callbackBody, err := json.Marshal(quoteResponse)
	if err != nil {
		peer.t.Fatal(err)
	}
	callbackResponse := peer.postSigned(http.MethodPut, "/quotes/"+quoteID, callbackBody)
	io.Copy(io.Discard, callbackResponse.Body)
	callbackResponse.Body.Close()
	if callbackResponse.StatusCode != http.StatusAccepted {
		peer.t.Fatalf("quote callback answered %d", callbackResponse.StatusCode)
	}
}

func (peer *loopbackPeerFSP) handlePostTransfer(response http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		peer.t.Fatalf("read transfer: %v", err)
	}
	if err := VerifyRequestWithKeyID(request.Method, request.URL.RequestURI(), request.Header.Get("FSPIOP-Source"), request.Header.Get("FSPIOP-Destination"), body, request.Header.Get(signatureHeader), &peer.key.PublicKey, "rail-kid-1"); err != nil {
		http.Error(response, "bad signature", http.StatusUnauthorized)
		return
	}
	var transfer TransferRequestBody
	if err := json.Unmarshal(body, &transfer); err != nil {
		http.Error(response, "bad transfer", http.StatusBadRequest)
		return
	}
	if transfer.Condition != peer.condition {
		http.Error(response, "condition mismatch", http.StatusBadRequest)
		return
	}
	peer.sawTransfer = true
	response.WriteHeader(http.StatusAccepted)
	// Fulfil: the Hub's signed COMMITTED callback carries the real preimage.
	callback := TransferCallback{
		TransferIdentity: TransferIdentity{TransferID: transfer.TransferID, PayerFSP: transfer.PayerFSP, PayeeFSP: transfer.PayeeFSP, Amount: transfer.Amount.Amount, Currency: transfer.Amount.Currency},
		TransferState:    TransferCommitted,
		Fulfilment:       peer.fulfilment,
	}
	callbackBody, err := json.Marshal(callback)
	if err != nil {
		peer.t.Fatal(err)
	}
	callbackResponse := peer.postSigned(http.MethodPut, "/transfers/"+transfer.TransferID, callbackBody)
	io.Copy(io.Discard, callbackResponse.Body)
	callbackResponse.Body.Close()
	if callbackResponse.StatusCode != http.StatusAccepted {
		peer.t.Fatalf("transfer callback answered %d", callbackResponse.StatusCode)
	}
}

// railClient builds the production FSPIOP client pointed at the peer's TLS
// server (the only test-only seam: the peer's loopback certificate).
func railClient(t *testing.T, peer *loopbackPeerFSP, key *rsa.PrivateKey) Client {
	t.Helper()
	baseURL, err := url.Parse(peer.server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	httpClient := peer.server.Client()
	httpClient.Timeout = 10 * time.Second
	return Client{
		BaseURL: baseURL, HTTPClient: httpClient,
		Source: peer.destID, Destination: peer.sourceID,
		SigningKey: key, SigningKeyID: "rail-kid-1", SignatureAlgorithm: "RS256",
		MaxAttempts: 1,
	}
}

func outboundMigrationPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(filepath.Clean(os.Getenv("MOJALOOP_MIGRATION_PATH"))), "0015_mojaloop_outbound.sql")
}

// TestRealPostgresMojaloopOutboundFullLeg drives the complete outbound flow
// against real PostgreSQL: POST /payouts -> POST /quotes -> signed PUT
// /quotes/{id} -> POST /transfers -> signed PUT /transfers/{id} COMMITTED,
// with quote-response replay idempotency and forged-fulfilment rejection.
func TestRealPostgresMojaloopOutboundFullLeg(t *testing.T) {
	ctx := context.Background()
	store, err := intent.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resetPublicSchema(t, ctx, store)
	for _, path := range []string{filepath.Clean(os.Getenv("MOJALOOP_MIGRATION_PATH")), outboundMigrationPath(t)} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	peer := newLoopbackPeerFSP(t)
	railKey := peer.key // one loopback keypair; production uses distinct keys
	outbound := NewOutboundStore(store.Pool())
	client := railClient(t, peer, railKey)
	callbackHandler := CallbackHandler{
		Store: NewCallbackStore(store.Pool()), Outbound: outbound,
		VerificationKey: &peer.key.PublicKey, ExpectedSource: peer.sourceID, ExpectedDestination: peer.destID,
		ExpectedVerificationKeyID: "partner-kid-1",
	}
	quoteHandler := QuoteCallbackHandler{
		Store: outbound, Client: &client, PayerFSP: peer.destID, PayeeFSP: peer.sourceID,
		VerificationKey: &peer.key.PublicKey, ExpectedSource: peer.sourceID, ExpectedDestination: peer.destID,
		ExpectedVerificationKeyID: "partner-kid-1",
	}
	mux := http.NewServeMux()
	mux.Handle("/transfers/", callbackHandler)
	mux.Handle("/quotes/", quoteHandler)
	peer.callbacks = httptest.NewServer(mux)
	defer peer.callbacks.Close()

	// 1. Initiate the payout through the gated endpoint.
	payout := PayoutHandler{
		Store: outbound, Client: &client, PayerFSP: peer.destID, PayeeFSP: peer.sourceID,
		Authenticator: stubAuthenticator{principal: cvffapi.Principal{Subject: "payout-officer-1", Roles: []string{"payout-officer"}}},
		Policy:        payoutTestPolicy(t, true),
		NewQuoteID:    func() string { return "quote-integration-001" },
	}
	payoutResponse := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/payouts", strings.NewReader(`{"payerPartyIdType":"MSISDN","payerPartyIdentifier":"payer-1","payeePartyIdType":"MSISDN","payeePartyIdentifier":"payee-1","amount":"100","currency":"NGN"}`))
	payout.ServeHTTP(payoutResponse, request)
	if payoutResponse.Code != http.StatusAccepted {
		t.Fatalf("payout answered %d: %s", payoutResponse.Code, payoutResponse.Body)
	}
	if !peer.sawQuote || !peer.sawTransfer {
		t.Fatalf("peer must have observed POST /quotes and POST /transfers: quote=%v transfer=%v", peer.sawQuote, peer.sawTransfer)
	}

	// 2. Durable terminal truth: quote RESPONSE_RECEIVED, transfer COMMITTED
	// with the fulfilment that satisfies the quote's ILP condition.
	quote, err := outbound.GetQuote(ctx, "quote-integration-001")
	if err != nil {
		t.Fatal(err)
	}
	if quote.State != QuoteResponseReceived || quote.Response == nil || quote.Response.Condition != peer.condition {
		t.Fatalf("quote state = %q response=%+v", quote.State, quote.Response)
	}
	var transfer OutboundTransfer
	var fulfilment string
	err = store.Pool().QueryRow(ctx, `SELECT transfer_id, fulfilment FROM mojaloop_outbound_transfers WHERE quote_id = $1`, quote.QuoteID).Scan(&transfer.TransferID, &fulfilment)
	if err != nil {
		t.Fatal(err)
	}
	transfer, err = outbound.LookupOutboundTransfer(ctx, transfer.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	if transfer.State != TransferCommitted || transfer.Fulfilment != peer.fulfilment {
		t.Fatalf("transfer state = %q fulfilment = %q", transfer.State, transfer.Fulfilment)
	}
	if err := VerifyILPFulfilment(transfer.Condition, transfer.Fulfilment); err != nil {
		t.Fatalf("stored fulfilment must satisfy stored condition: %v", err)
	}

	// 3. Fail-closed replay guard: the quote row retains the callback body
	// hash only (by design), so a follow-up callback with any different body
	// after RESPONSE_RECEIVED conflicts instead of mutating terminal state.
	if _, _, err := outbound.ApplyQuoteCallback(ctx, quote.QuoteID, *quote.Response, []byte("different-body")); err == nil {
		t.Fatal("a quote callback with a different body after RESPONSE_RECEIVED must fail closed")
	}

	// 4. Unsolicited quote callback: no correlating quote, fail closed 404.
	orphanBody := []byte(`{"transferAmount":{"amount":"1","currency":"NGN"},"ilpPacket":"AAAA","condition":"` + peer.condition + `"}`)
	orphanResponse := peer.postSigned(http.MethodPut, "/quotes/quote-never-requested", orphanBody)
	io.Copy(io.Discard, orphanResponse.Body)
	orphanResponse.Body.Close()
	if orphanResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("orphan quote callback answered %d, want 404", orphanResponse.StatusCode)
	}

	// 5. Forged fulfilment: a COMMITTED callback whose preimage does not
	// satisfy the condition must be rejected and never recorded.
	forgedFulfilment, forgedCondition, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	forgedTransfer := OutboundTransfer{
		TransferID: "transfer-forged-001", QuoteID: quote.QuoteID,
		PayerFSP: peer.destID, PayeeFSP: peer.sourceID, Amount: "5", Currency: "NGN",
		ILPPacket: "AYICbQAAAAAAA", Condition: forgedCondition, State: TransferPrepared,
	}
	forgedBody, err := json.Marshal(TransferRequestBody{TransferID: forgedTransfer.TransferID, PayerFSP: forgedTransfer.PayerFSP, PayeeFSP: forgedTransfer.PayeeFSP, Amount: QuoteAmount{Amount: "5", Currency: "NGN"}, ILPPacket: forgedTransfer.ILPPacket, Condition: forgedCondition})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outbound.CreateTransfer(ctx, forgedTransfer, forgedBody); err != nil {
		t.Fatal(err)
	}
	wrongFulfilment, _, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	forgedCallback := TransferCallback{
		TransferIdentity: TransferIdentity{TransferID: forgedTransfer.TransferID, PayerFSP: forgedTransfer.PayerFSP, PayeeFSP: forgedTransfer.PayeeFSP, Amount: "5", Currency: "NGN"},
		TransferState:    TransferCommitted, Fulfilment: wrongFulfilment,
	}
	forgedCallbackBody, err := json.Marshal(forgedCallback)
	if err != nil {
		t.Fatal(err)
	}
	forgedResponse := peer.postSigned(http.MethodPut, "/transfers/"+forgedTransfer.TransferID, forgedCallbackBody)
	io.Copy(io.Discard, forgedResponse.Body)
	forgedResponse.Body.Close()
	if forgedResponse.StatusCode != http.StatusConflict {
		t.Fatalf("forged fulfilment answered %d, want 409", forgedResponse.StatusCode)
	}
	reloaded, err := outbound.LookupOutboundTransfer(ctx, forgedTransfer.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != TransferPrepared {
		t.Fatalf("forged commit must never advance state: %q", reloaded.State)
	}
	// The honest fulfilment still completes it.
	_ = forgedFulfilment
	honestCallback := forgedCallback
	honestCallback.Fulfilment = forgedFulfilment
	honestBody, err := json.Marshal(honestCallback)
	if err != nil {
		t.Fatal(err)
	}
	honestResponse := peer.postSigned(http.MethodPut, "/transfers/"+forgedTransfer.TransferID, honestBody)
	io.Copy(io.Discard, honestResponse.Body)
	honestResponse.Body.Close()
	if honestResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("honest fulfilment answered %d, want 202", honestResponse.StatusCode)
	}
	reloaded, err = outbound.LookupOutboundTransfer(ctx, forgedTransfer.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != TransferCommitted {
		t.Fatalf("honest commit state = %q", reloaded.State)
	}
}
