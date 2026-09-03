package mojaloop

import (
	"errors"
	"testing"
)

func testQuote() OutboundQuote {
	return OutboundQuote{QuoteID: "quote-1", PayerFSP: "FMMBE", PayeeFSP: "PARTNER01", Amount: "100", Currency: "NGN", State: QuoteRequested}
}

func validQuoteResponse(t *testing.T) QuoteResponse {
	t.Helper()
	_, condition, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	return QuoteResponse{
		TransferAmount: QuoteAmount{Amount: "100", Currency: "NGN"},
		Expiration:     "2030-01-01T00:00:00Z",
		ILPPacket:      "AYICbQAAAAAAA",
		Condition:      condition,
	}
}

func TestValidateQuoteResponseHappyPath(t *testing.T) {
	if err := ValidateQuoteResponse(testQuote(), validQuoteResponse(t)); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
}

func TestValidateQuoteResponseRejectsRepricing(t *testing.T) {
	response := validQuoteResponse(t)
	response.TransferAmount.Amount = "101"
	if err := ValidateQuoteResponse(testQuote(), response); err == nil {
		t.Fatal("re-priced transfer amount must be rejected")
	}
	response = validQuoteResponse(t)
	response.TransferAmount.Currency = "USD"
	if err := ValidateQuoteResponse(testQuote(), response); err == nil {
		t.Fatal("currency swap must be rejected")
	}
}

func TestValidateQuoteResponseRejectsMissingILP(t *testing.T) {
	response := validQuoteResponse(t)
	response.ILPPacket = ""
	if err := ValidateQuoteResponse(testQuote(), response); err == nil {
		t.Fatal("missing ILP packet must be rejected")
	}
	response = validQuoteResponse(t)
	response.Condition = ""
	if err := ValidateQuoteResponse(testQuote(), response); err == nil {
		t.Fatal("missing condition must be rejected")
	}
	response = validQuoteResponse(t)
	response.Condition = "not-a-condition"
	if err := ValidateQuoteResponse(testQuote(), response); !errors.Is(err, ErrMalformedCondition) {
		t.Fatalf("malformed condition: err = %v", err)
	}
}

func outboundTransferFixture(t *testing.T) (OutboundTransfer, string) {
	t.Helper()
	fulfilment, condition, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	return OutboundTransfer{
		TransferID: "transfer-1", QuoteID: "quote-1",
		PayerFSP: "FMMBE", PayeeFSP: "PARTNER01",
		Amount: "100", Currency: "NGN",
		ILPPacket: "AYICbQAAAAAAA", Condition: condition,
		State: TransferPrepared,
	}, fulfilment
}

func TestOutboundTransferCallbackCommitRequiresValidFulfilment(t *testing.T) {
	transfer, fulfilment := outboundTransferFixture(t)
	callback := TransferCallback{
		TransferIdentity: TransferIdentity{TransferID: transfer.TransferID, PayerFSP: transfer.PayerFSP, PayeeFSP: transfer.PayeeFSP, Amount: transfer.Amount, Currency: transfer.Currency},
		TransferState:    TransferCommitted,
		Fulfilment:       fulfilment,
	}
	if err := ValidateOutboundTransferCallback(transfer, callback); err != nil {
		t.Fatalf("honest commit rejected: %v", err)
	}
	callback.Fulfilment = ""
	if err := ValidateOutboundTransferCallback(transfer, callback); err == nil {
		t.Fatal("commit without fulfilment must fail closed")
	}
	wrongFulfilment, _, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	callback.Fulfilment = wrongFulfilment
	if err := ValidateOutboundTransferCallback(transfer, callback); !errors.Is(err, ErrInvalidFulfilment) {
		t.Fatalf("forged fulfilment: err = %v", err)
	}
}

func TestOutboundTransferCallbackAbortAndRegression(t *testing.T) {
	transfer, _ := outboundTransferFixture(t)
	abort := TransferCallback{
		TransferIdentity: TransferIdentity{TransferID: transfer.TransferID, PayerFSP: transfer.PayerFSP, PayeeFSP: transfer.PayeeFSP, Amount: transfer.Amount, Currency: transfer.Currency},
		TransferState:    TransferAborted,
	}
	if err := ValidateOutboundTransferCallback(transfer, abort); err != nil {
		t.Fatalf("abort rejected: %v", err)
	}
	abort.TransferState = TransferReserved
	if err := ValidateOutboundTransferCallback(transfer, abort); !errors.Is(err, ErrInvalidPreparedState) {
		t.Fatalf("RESERVED on an outbound transfer must fail closed: err = %v", err)
	}
	// Terminal rows never transition again.
	transfer.State = TransferCommitted
	abort.TransferState = TransferAborted
	if err := ValidateOutboundTransferCallback(transfer, abort); !errors.Is(err, ErrInvalidTransferState) {
		t.Fatalf("post-commit regression: err = %v", err)
	}
}

func TestOutboundTransferCallbackIdentityBinding(t *testing.T) {
	transfer, fulfilment := outboundTransferFixture(t)
	callback := TransferCallback{
		TransferIdentity: TransferIdentity{TransferID: transfer.TransferID, PayerFSP: transfer.PayerFSP, PayeeFSP: transfer.PayeeFSP, Amount: "999", Currency: transfer.Currency},
		TransferState:    TransferCommitted,
		Fulfilment:       fulfilment,
	}
	if err := ValidateOutboundTransferCallback(transfer, callback); !errors.Is(err, ErrTransferIdentityChange) {
		t.Fatalf("amount change: err = %v", err)
	}
}
