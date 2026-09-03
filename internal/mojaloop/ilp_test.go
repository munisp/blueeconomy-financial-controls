package mojaloop

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewILPFulfilmentSatisfiesDerivedCondition(t *testing.T) {
	fulfilment, condition, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyILPFulfilment(condition, fulfilment); err != nil {
		t.Fatalf("fresh fulfilment must satisfy its condition: %v", err)
	}
	preimage, err := base64.RawURLEncoding.DecodeString(fulfilment)
	if err != nil || len(preimage) != 32 {
		t.Fatalf("fulfilment must be a base64url 32-byte preimage: %v", err)
	}
	digest := sha256.Sum256(preimage)
	if want := base64.RawURLEncoding.EncodeToString(digest[:]); condition != want {
		t.Fatalf("condition = %q, want sha256(preimage) %q", condition, want)
	}
}

func TestNewILPFulfilmentIsRandomPerCall(t *testing.T) {
	first, _, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two fulfilments must never collide")
	}
}

func TestVerifyILPFulfilmentRejectsWrongPreimage(t *testing.T) {
	fulfilment, condition, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyILPFulfilment(condition, other); err == nil {
		t.Fatal("a different preimage must not satisfy the condition")
	}
	if err := VerifyILPFulfilment(condition, fulfilment); err != nil {
		t.Fatalf("honest fulfilment rejected: %v", err)
	}
}

func TestValidateILPConditionRejectsMalformed(t *testing.T) {
	for _, condition := range []string{"", "not-base64!!!", base64.RawURLEncoding.EncodeToString([]byte("short")), strings.Repeat("A", 44)} {
		if err := ValidateILPCondition(condition); err == nil && condition != strings.Repeat("A", 44) {
			t.Fatalf("condition %q must be rejected", condition)
		}
	}
	_, condition, err := NewILPFulfilment()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateILPCondition(condition); err != nil {
		t.Fatalf("real condition rejected: %v", err)
	}
}

func TestConditionForFulfilmentMatchesSHA256(t *testing.T) {
	preimage := []byte("phase13-mojaloop-outbound-test-vector!")
	fulfilment := base64.RawURLEncoding.EncodeToString(preimage)
	digest := sha256.Sum256(preimage)
	want := base64.RawURLEncoding.EncodeToString(digest[:])
	if got := ConditionForFulfilment(fulfilment); got != want {
		t.Fatalf("condition = %q, want %q", got, want)
	}
}
