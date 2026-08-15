//go:build integration

package mojaloop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/intent"
)

func TestRealPostgresMojaloopCallbackStore(t *testing.T) {
	ctx := context.Background()
	store, err := intent.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	migration, err := os.ReadFile(filepath.Clean(os.Getenv("MOJALOOP_MIGRATION_PATH")))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	callbacks := NewCallbackStore(store.Pool())
	reserved := TransferCallback{TransferIdentity: TransferIdentity{TransferID: "transfer-integration-001", PayerFSP: "FMMBE", PayeeFSP: "PARTNER01", Amount: "100", Currency: "NGN"}, TransferState: TransferReserved}
	reservedBody := []byte(`{"transferId":"transfer-integration-001","transferState":"RESERVED"}`)
	created, duplicate, err := callbacks.ApplyCallback(ctx, reserved, reservedBody)
	if err != nil || duplicate || created.TransferState != TransferReserved {
		t.Fatalf("reserve callback failed: %+v duplicate=%v err=%v", created, duplicate, err)
	}
	_, duplicate, err = callbacks.ApplyCallback(ctx, reserved, reservedBody)
	if err != nil || !duplicate {
		t.Fatalf("reserve replay failed: duplicate=%v err=%v", duplicate, err)
	}
	committed := reserved
	committed.TransferState = TransferCommitted
	committed.Fulfilment = "fulfilment-001"
	committedBody := []byte(`{"transferId":"transfer-integration-001","transferState":"COMMITTED","fulfilment":"fulfilment-001"}`)
	updated, duplicate, err := callbacks.ApplyCallback(ctx, committed, committedBody)
	if err != nil || duplicate || updated.TransferState != TransferCommitted {
		t.Fatalf("commit callback failed: %+v duplicate=%v err=%v", updated, duplicate, err)
	}
	_, duplicate, err = callbacks.ApplyCallback(ctx, committed, committedBody)
	if err != nil || !duplicate {
		t.Fatalf("commit replay failed: duplicate=%v err=%v", duplicate, err)
	}
	regressed := committed
	regressed.TransferState = TransferReserved
	if _, _, err := callbacks.ApplyCallback(ctx, regressed, []byte(`{"transferId":"transfer-integration-001","transferState":"RESERVED"}`)); !errors.Is(err, ErrInvalidTransferState) {
		t.Fatalf("regression error = %v, want invalid transition", err)
	}
}
