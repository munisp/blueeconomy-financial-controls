package mojaloop

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestFSPIOPSignatureRoundTripAndBodyBinding(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"transferId":"transfer-001","amount":{"amount":"100","currency":"NGN"}}`)
	signature, err := SignRequest("POST", "/transfers", "FMMBE", "PARTNER01", body, key, "RS256")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRequest("POST", "/transfers", "FMMBE", "PARTNER01", body, signature, &key.PublicKey); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRequest("POST", "/transfers", "FMMBE", "PARTNER01", []byte(`{"transferId":"transfer-002"}`), signature, &key.PublicKey); err == nil {
		t.Fatal("tampered body was accepted")
	}
	if err := VerifyRequest("GET", "/transfers", "FMMBE", "PARTNER01", body, signature, &key.PublicKey); err == nil {
		t.Fatal("changed method was accepted")
	}
}

func TestFSPIOPSignatureSupportsApprovedAlgorithms(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, algorithm := range []string{"RS256", "RS384", "RS512"} {
		signature, signErr := SignRequest("PUT", "/transfers/transfer-001", "PARTNER01", "FMMBE", []byte(`{"transferState":"COMMITTED"}`), key, algorithm)
		if signErr != nil {
			t.Fatalf("%s signing failed: %v", algorithm, signErr)
		}
		if verifyErr := VerifyRequest("PUT", "/transfers/transfer-001", "PARTNER01", "FMMBE", []byte(`{"transferState":"COMMITTED"}`), signature, &key.PublicKey); verifyErr != nil {
			t.Fatalf("%s verification failed: %v", algorithm, verifyErr)
		}
	}
	if _, err := SignRequest("POST", "/transfers", "FMMBE", "PARTNER01", nil, key, "HS256"); err == nil {
		t.Fatal("unapproved algorithm was accepted")
	}
}

func TestFSPIOPSignatureBindsRegisteredKID(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"transferId":"transfer-001"}`)
	signature, err := SignRequestWithKeyID("POST", "/transfers", "FMMBE", "PARTNER01", body, key, "RS256", "fmmbe-sandbox-2026-01")
	if err != nil {
		t.Fatal(err)
	}
	var envelope signatureHeaderValue
	if err := json.Unmarshal([]byte(signature), &envelope); err != nil {
		t.Fatal(err)
	}
	protectedBytes, err := base64.RawURLEncoding.DecodeString(envelope.ProtectedHeader)
	if err != nil {
		t.Fatal(err)
	}
	var protected signatureProtectedHeader
	if err := json.Unmarshal(protectedBytes, &protected); err != nil {
		t.Fatal(err)
	}
	if protected.KeyID != "fmmbe-sandbox-2026-01" {
		t.Fatalf("protected KID = %q", protected.KeyID)
	}
	if err := VerifyRequest("POST", "/transfers", "FMMBE", "PARTNER01", body, signature, &key.PublicKey); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRequestWithKeyID("POST", "/transfers", "FMMBE", "PARTNER01", body, signature, &key.PublicKey, "fmmbe-sandbox-2026-01"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRequestWithKeyID("POST", "/transfers", "FMMBE", "PARTNER01", body, signature, &key.PublicKey, "hub-sandbox-2026-02"); err == nil {
		t.Fatal("mismatched protected KID was accepted")
	}
	if _, err := SignRequestWithKeyID("POST", "/transfers", "FMMBE", "PARTNER01", body, key, "RS256", "bad kid"); err == nil {
		t.Fatal("noncanonical KID was accepted")
	}
}

func TestTransferCallbackStateMachine(t *testing.T) {
	reserved := TransferCallback{TransferIdentity: TransferIdentity{TransferID: "transfer-001", PayerFSP: "FMMBE", PayeeFSP: "PARTNER01", Amount: "100", Currency: "NGN"}, TransferState: TransferReserved}
	committed := reserved
	committed.TransferState = TransferCommitted
	if err := ValidateTransferCallback(&reserved, committed); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransferCallback(&committed, reserved); err == nil {
		t.Fatal("committed transfer regressed to reserved")
	}
	changed := committed
	changed.Amount = "101"
	if err := ValidateTransferCallback(&committed, changed); err == nil {
		t.Fatal("changed immutable amount was accepted")
	}
	if err := ValidateTransferCallback(&committed, committed); err != nil {
		t.Fatal("identical committed replay rejected: ", err)
	}
}

func TestTransferCallbackFirstSeenPolicy(t *testing.T) {
	base := TransferCallback{TransferIdentity: TransferIdentity{TransferID: "transfer-002", PayerFSP: "FMMBE", PayeeFSP: "PARTNER01", Amount: "100", Currency: "NGN"}}
	reserved := base
	reserved.TransferState = TransferReserved
	if err := ValidateTransferCallback(nil, reserved); err != nil {
		t.Fatalf("first-seen RESERVED rejected: %v", err)
	}
	// Fail-closed ordering: money must never settle (or abort) before this
	// rail observed the reservation that locked the funds.
	committed := base
	committed.TransferState = TransferCommitted
	if err := ValidateTransferCallback(nil, committed); err != ErrInvalidTransferState {
		t.Fatalf("first-seen COMMITTED error = %v, want ErrInvalidTransferState", err)
	}
	aborted := base
	aborted.TransferState = TransferAborted
	if err := ValidateTransferCallback(nil, aborted); err != ErrInvalidTransferState {
		t.Fatalf("first-seen ABORTED error = %v, want ErrInvalidTransferState", err)
	}
	unknown := base
	unknown.TransferState = "PENDING"
	if err := ValidateTransferCallback(nil, unknown); err != ErrInvalidTransferState {
		t.Fatalf("first-seen unknown state error = %v, want ErrInvalidTransferState", err)
	}
}
