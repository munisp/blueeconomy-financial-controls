package mojaloop

import "errors"

// Outbound quote/transfer leg types and state machines. The payer FSP (this
// rail) initiates POST /quotes, receives a signed PUT /quotes/{id} response
// from the payee FSP carrying the transfer terms and ILP condition, then
// posts POST /transfers with the ILP packet and waits for the Hub's PUT
// /transfers/{id} callback to commit or abort. Both state machines are
// fail-closed and immutable once terminal.

var (
	ErrInvalidQuoteState    = errors.New("invalid quote state transition")
	ErrQuoteIdentityChange  = errors.New("quote identity changed in callback")
	ErrUnknownQuote         = errors.New("quote callback does not correlate with a local quote")
	ErrUnknownTransfer      = errors.New("transfer callback does not correlate with a local outbound transfer")
	ErrInvalidPreparedState = errors.New("invalid outbound transfer state transition")
)

// QuoteState is the outbound quote lifecycle.
type QuoteState string

const (
	// QuoteRequested: POST /quotes was signed and sent; no response yet.
	QuoteRequested QuoteState = "REQUESTED"
	// QuoteResponseReceived: the payee FSP's signed PUT /quotes/{id}
	// callback was validated and persisted. Terminal for the quote row.
	QuoteResponseReceived QuoteState = "RESPONSE_RECEIVED"
)

// QuoteParty identifies one participant party in a quote.
type QuoteParty struct {
	PartyIDType  string `json:"partyIdType"`
	PartyIDValue string `json:"partyIdentifier"`
	FSPID        string `json:"fspId,omitempty"`
}

// QuoteAmount is an FSPIOP money value.
type QuoteAmount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// QuoteRequestBody is the POST /quotes payload this rail sends.
type QuoteRequestBody struct {
	QuoteID         string      `json:"quoteId"`
	TransactionID   string      `json:"transactionId"`
	Payer           QuoteParty  `json:"payer"`
	Payee           QuoteParty  `json:"payee"`
	Amount          QuoteAmount `json:"amount"`
	TransactionType struct {
		Scenario      string `json:"scenario"`
		Initiator     string `json:"initiator"`
		InitiatorType string `json:"initiatorType"`
	} `json:"transactionType"`
}

// QuoteResponse is the payee FSP's PUT /quotes/{id} callback body: the
// transfer terms and the ILP condition the transfer must satisfy.
type QuoteResponse struct {
	TransferAmount     QuoteAmount  `json:"transferAmount"`
	Expiration         string       `json:"expiration"`
	ILPPacket          string       `json:"ilpPacket"`
	Condition          string       `json:"condition"`
	PayeeReceiveAmount *QuoteAmount `json:"payeeReceiveAmount,omitempty"`
	PayeeFSPFee        *QuoteAmount `json:"payeeFspFee,omitempty"`
	PayeeFSPCommission *QuoteAmount `json:"payeeFspCommission,omitempty"`
}

// OutboundQuote is the durable quote row.
type OutboundQuote struct {
	QuoteID  string
	PayerFSP string
	PayeeFSP string
	Amount   string
	Currency string
	State    QuoteState
	Response *QuoteResponse
}

// ValidateQuoteResponse enforces the fail-closed acceptance policy for a
// payee FSP quote response: the transfer amount must exactly match the
// requested amount and currency (no silent re-pricing on the money path),
// the ILP packet and condition must be present, and the condition must be a
// well-formed base64url SHA-256 digest.
func ValidateQuoteResponse(quote OutboundQuote, response QuoteResponse) error {
	if response.TransferAmount.Amount == "" || response.TransferAmount.Currency == "" {
		return errors.New("quote response transfer amount is required")
	}
	if response.TransferAmount.Amount != quote.Amount || response.TransferAmount.Currency != quote.Currency {
		return errors.New("quote response transfer amount does not match the requested amount")
	}
	if response.ILPPacket == "" || response.Condition == "" {
		return errors.New("quote response ILP packet and condition are required")
	}
	if err := ValidateILPCondition(response.Condition); err != nil {
		return err
	}
	return nil
}

// ValidateOutboundTransferCallback enforces the outbound transfer callback
// transition policy: the durable row begins at PREPARED (set when POST
// /transfers was accepted by the switch) and may advance to COMMITTED or
// ABORTED exactly once. A COMMITTED callback must carry a fulfilment that
// cryptographically satisfies the quote's ILP condition — a commit without
// a valid fulfilment is never accepted. Terminal states never change.
func ValidateOutboundTransferCallback(transfer OutboundTransfer, callback TransferCallback) error {
	if err := validateCallbackIdentity(callback); err != nil {
		return err
	}
	if transfer.TransferID != callback.TransferID || transfer.PayerFSP != callback.PayerFSP || transfer.PayeeFSP != callback.PayeeFSP || transfer.Amount != callback.Amount || transfer.Currency != callback.Currency {
		return ErrTransferIdentityChange
	}
	switch transfer.State {
	case TransferPrepared:
		switch callback.TransferState {
		case TransferCommitted:
			if callback.Fulfilment == "" {
				return errors.New("committed transfer callback requires a fulfilment")
			}
			return VerifyILPFulfilment(transfer.Condition, callback.Fulfilment)
		case TransferAborted:
			return nil
		default:
			return ErrInvalidPreparedState
		}
	case TransferCommitted, TransferAborted:
		return ErrInvalidTransferState
	}
	return ErrInvalidPreparedState
}

// Outbound transfer lifecycle states. PREPARED mirrors the FSPIOP prepare
// leg (POST /transfers accepted); COMMITTED/ABORTED are Hub-signed terminal
// truth delivered by PUT /transfers/{id}.
const (
	TransferPrepared TransferState = "PREPARED"
)

// OutboundTransfer is the durable outbound transfer row.
type OutboundTransfer struct {
	TransferID string
	QuoteID    string
	PayerFSP   string
	PayeeFSP   string
	Amount     string
	Currency   string
	ILPPacket  string
	Condition  string
	State      TransferState
	Fulfilment string
}

// TransferRequestBody is the POST /transfers payload: the transfer amount,
// the ILP packet from the quote response and the condition derived from the
// fulfilment preimage this rail generated.
type TransferRequestBody struct {
	TransferID string      `json:"transferId"`
	PayerFSP   string      `json:"payerFsp"`
	PayeeFSP   string      `json:"payeeFsp"`
	Amount     QuoteAmount `json:"amount"`
	ILPPacket  string      `json:"ilpPacket"`
	Condition  string      `json:"condition"`
	Expiration string      `json:"expiration"`
}
