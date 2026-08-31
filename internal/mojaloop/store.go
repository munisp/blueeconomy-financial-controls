package mojaloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CallbackStore struct{ pool *pgxpool.Pool }

func NewCallbackStore(pool *pgxpool.Pool) *CallbackStore { return &CallbackStore{pool: pool} }

// SweepReservedTimeouts marks every RESERVED callback that has waited longer
// than ttl as timed out and records one audit row per transfer in
// mojaloop_callback_timeout_events. The sweep is idempotent (a transfer is
// marked once) and honest: it never fabricates a Hub terminal state — the
// callback stays RESERVED, the timeout is local operational evidence that
// the locked funds need intervention. A later Hub-signed COMMITTED/ABORTED
// remains acceptable terminal truth. Returns the number of newly timed-out
// transfers.
func (store *CallbackStore) SweepReservedTimeouts(ctx context.Context, ttl time.Duration, now time.Time) (int64, error) {
	if ttl <= 0 {
		return 0, errors.New("reserved timeout ttl must be positive")
	}
	cutoff := now.UTC().Add(-ttl)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin reserved-timeout sweep: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT transfer_id, updated_at FROM mojaloop_transfer_callbacks WHERE transfer_state = $1 AND timed_out_at IS NULL AND updated_at < $2 ORDER BY transfer_id FOR UPDATE`, TransferReserved, cutoff)
	if err != nil {
		return 0, fmt.Errorf("find expired reserved callbacks: %w", err)
	}
	type expiredTransfer struct {
		transferID    string
		reservedSince time.Time
	}
	expired := make([]expiredTransfer, 0)
	for rows.Next() {
		var candidate expiredTransfer
		if err := rows.Scan(&candidate.transferID, &candidate.reservedSince); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired reserved callback: %w", err)
		}
		expired = append(expired, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate expired reserved callbacks: %w", err)
	}
	markedAt := now.UTC()
	for _, candidate := range expired {
		if _, err := tx.Exec(ctx, `INSERT INTO mojaloop_callback_timeout_events (event_id, transfer_id, reserved_at, timed_out_at) VALUES ($1,$2,$3,$4)`, uuid.New(), candidate.transferID, candidate.reservedSince, markedAt); err != nil {
			return 0, fmt.Errorf("write reserved-timeout audit: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE mojaloop_transfer_callbacks SET timed_out_at = $1, version = version + 1 WHERE transfer_id = $2 AND timed_out_at IS NULL`, markedAt, candidate.transferID); err != nil {
			return 0, fmt.Errorf("mark reserved callback timed out: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit reserved-timeout sweep: %w", err)
	}
	return int64(len(expired)), nil
}

func (store *CallbackStore) ApplyCallback(ctx context.Context, callback TransferCallback, body []byte) (TransferCallback, bool, error) {
	// Entry validation is fields-only: the first-seen RESERVED policy of
	// ValidateTransferCallback applies solely when no durable record exists
	// (below). Applying it here rejected every follow-up COMMITTED/ABORTED
	// callback — the happy path — because `previous` was always nil at this
	// point. Caught by the Phase-6 Postgres integration tests.
	if err := validateCallbackIdentity(callback); err != nil {
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
		// No durable record: the first-seen policy applies — the rail must
		// observe the reservation (funds locked) before any settlement or
		// abandonment callback may be recorded.
		if err := ValidateTransferCallback(nil, callback); err != nil {
			return TransferCallback{}, false, err
		}
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
