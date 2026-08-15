package mojaloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CallbackStore struct{ pool *pgxpool.Pool }

func NewCallbackStore(pool *pgxpool.Pool) *CallbackStore { return &CallbackStore{pool: pool} }

func (store *CallbackStore) ApplyCallback(ctx context.Context, callback TransferCallback, body []byte) (TransferCallback, bool, error) {
	if err := ValidateTransferCallback(nil, callback); err != nil {
		return TransferCallback{}, false, err
	}
	bodyDigest := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(bodyDigest[:])
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return TransferCallback{}, false, fmt.Errorf("begin callback: %w", err)
	}
	defer tx.Rollback(ctx)
	var current TransferCallback
	var currentHash string
	err = tx.QueryRow(ctx, `SELECT transfer_id, payer_fsp, payee_fsp, amount, currency, transfer_state, fulfilment, body_sha256 FROM mojaloop_transfer_callbacks WHERE transfer_id = $1 FOR UPDATE`, callback.TransferID).Scan(&current.TransferID, &current.PayerFSP, &current.PayeeFSP, &current.Amount, &current.Currency, &current.TransferState, &current.Fulfilment, &currentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO mojaloop_transfer_callbacks (transfer_id, payer_fsp, payee_fsp, amount, currency, transfer_state, fulfilment, body_sha256, created_at, updated_at, version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,1)`, callback.TransferID, callback.PayerFSP, callback.PayeeFSP, callback.Amount, callback.Currency, callback.TransferState, callback.Fulfilment, bodyHash, now); err != nil {
			return TransferCallback{}, false, fmt.Errorf("insert callback: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return TransferCallback{}, false, fmt.Errorf("commit callback: %w", err)
		}
		return callback, false, nil
	}
	if err != nil {
		return TransferCallback{}, false, fmt.Errorf("lookup callback: %w", err)
	}
	if current.TransferID != callback.TransferID || current.PayerFSP != callback.PayerFSP || current.PayeeFSP != callback.PayeeFSP || current.Amount != callback.Amount || current.Currency != callback.Currency {
		return TransferCallback{}, false, ErrTransferIdentityChange
	}
	if currentHash == bodyHash {
		if err := tx.Commit(ctx); err != nil {
			return TransferCallback{}, false, fmt.Errorf("commit callback replay: %w", err)
		}
		return current, true, nil
	}
	if err := ValidateTransferCallback(&current, callback); err != nil {
		return TransferCallback{}, false, err
	}
	if current.TransferState == TransferCommitted || current.TransferState == TransferAborted {
		return TransferCallback{}, false, errors.New("terminal callback identity replay has conflicting body")
	}
	now := time.Now().UTC()
	var updated TransferCallback
	if err := tx.QueryRow(ctx, `UPDATE mojaloop_transfer_callbacks SET transfer_state = $1, fulfilment = $2, body_sha256 = $3, updated_at = $4, version = version + 1 WHERE transfer_id = $5 RETURNING transfer_id, payer_fsp, payee_fsp, amount, currency, transfer_state, fulfilment`, callback.TransferState, callback.Fulfilment, bodyHash, now, callback.TransferID).Scan(&updated.TransferID, &updated.PayerFSP, &updated.PayeeFSP, &updated.Amount, &updated.Currency, &updated.TransferState, &updated.Fulfilment); err != nil {
		return TransferCallback{}, false, fmt.Errorf("update callback: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TransferCallback{}, false, fmt.Errorf("commit callback update: %w", err)
	}
	return updated, false, nil
}
