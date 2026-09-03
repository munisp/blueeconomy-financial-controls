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

// OutboundStore persists the outbound quote/transfer state machine. Every
// mutation is transactional, optimistic-versioned and idempotent on the
// exact request/callback body hash, mirroring the inbound CallbackStore
// patterns.
type OutboundStore struct{ pool *pgxpool.Pool }

func NewOutboundStore(pool *pgxpool.Pool) *OutboundStore { return &OutboundStore{pool: pool} }

func bodyDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// CreateQuote records a REQUESTED quote exactly as sent (identity fields
// plus the request body hash). A re-send with the same body is an idempotent
// replay; the same quote ID with a different body fails closed.
func (store *OutboundStore) CreateQuote(ctx context.Context, quote OutboundQuote, requestBody []byte) (duplicate bool, err error) {
	if quote.QuoteID == "" || quote.PayerFSP == "" || quote.PayeeFSP == "" || quote.Amount == "" || quote.Currency == "" {
		return false, errors.New("quote identity and amount are required")
	}
	hash := bodyDigest(requestBody)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin create quote: %w", err)
	}
	defer tx.Rollback(ctx)
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT request_body_sha256 FROM mojaloop_outbound_quotes WHERE quote_id = $1 FOR UPDATE`, quote.QuoteID).Scan(&existingHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO mojaloop_outbound_quotes (quote_id, payer_fsp, payee_fsp, amount, currency, request_body_sha256, quote_state, created_at, updated_at, version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,1)`, quote.QuoteID, quote.PayerFSP, quote.PayeeFSP, quote.Amount, quote.Currency, hash, QuoteRequested, now); err != nil {
			return false, fmt.Errorf("insert quote: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit quote: %w", err)
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lookup quote: %w", err)
	}
	if existingHash != hash {
		return false, ErrQuoteIdentityChange
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit quote replay: %w", err)
	}
	return true, nil
}

// GetQuote loads one quote row (identity and any received response).
func (store *OutboundStore) GetQuote(ctx context.Context, quoteID string) (OutboundQuote, error) {
	var quote OutboundQuote
	var transferAmount, transferCurrency, ilpPacket, condition *string
	err := store.pool.QueryRow(ctx, `SELECT quote_id, payer_fsp, payee_fsp, amount, currency, quote_state, transfer_amount, transfer_currency, ilp_packet, ilp_condition FROM mojaloop_outbound_quotes WHERE quote_id = $1`, quoteID).Scan(&quote.QuoteID, &quote.PayerFSP, &quote.PayeeFSP, &quote.Amount, &quote.Currency, &quote.State, &transferAmount, &transferCurrency, &ilpPacket, &condition)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboundQuote{}, ErrUnknownQuote
	}
	if err != nil {
		return OutboundQuote{}, fmt.Errorf("load quote: %w", err)
	}
	if quote.State == QuoteResponseReceived {
		quote.Response = &QuoteResponse{
			TransferAmount: QuoteAmount{Amount: deref(transferAmount), Currency: deref(transferCurrency)},
			ILPPacket:      deref(ilpPacket),
			Condition:      deref(condition),
		}
	}
	return quote, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ApplyQuoteCallback persists a validated payee-FSP quote response. The
// callback must correlate with a locally REQUESTED quote (fail-closed:
// unsolicited quote responses are rejected, never invented). Exact replays
// (same body hash) are idempotent duplicates; a different body after the
// response was received fails closed.
func (store *OutboundStore) ApplyQuoteCallback(ctx context.Context, quoteID string, response QuoteResponse, body []byte) (quote OutboundQuote, duplicate bool, err error) {
	if quoteID == "" {
		return OutboundQuote{}, false, errors.New("quote id is required")
	}
	hash := bodyDigest(body)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return OutboundQuote{}, false, fmt.Errorf("begin quote callback: %w", err)
	}
	defer tx.Rollback(ctx)
	var callbackHash *string
	err = tx.QueryRow(ctx, `SELECT payer_fsp, payee_fsp, amount, currency, quote_state, callback_body_sha256 FROM mojaloop_outbound_quotes WHERE quote_id = $1 FOR UPDATE`, quoteID).Scan(&quote.PayerFSP, &quote.PayeeFSP, &quote.Amount, &quote.Currency, &quote.State, &callbackHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboundQuote{}, false, ErrUnknownQuote
	}
	if err != nil {
		return OutboundQuote{}, false, fmt.Errorf("lookup quote: %w", err)
	}
	quote.QuoteID = quoteID
	if quote.State == QuoteResponseReceived {
		if callbackHash != nil && *callbackHash == hash {
			if err := tx.Commit(ctx); err != nil {
				return OutboundQuote{}, false, fmt.Errorf("commit quote callback replay: %w", err)
			}
			return quote, true, nil
		}
		return OutboundQuote{}, false, errors.New("quote response already recorded with a different body")
	}
	if err := ValidateQuoteResponse(quote, response); err != nil {
		return OutboundQuote{}, false, err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE mojaloop_outbound_quotes SET quote_state = $1, transfer_amount = $2, transfer_currency = $3, ilp_packet = $4, ilp_condition = $5, callback_body_sha256 = $6, updated_at = $7, version = version + 1 WHERE quote_id = $8 AND quote_state = $9`, QuoteResponseReceived, response.TransferAmount.Amount, response.TransferAmount.Currency, response.ILPPacket, response.Condition, hash, now, quoteID, QuoteRequested); err != nil {
		return OutboundQuote{}, false, fmt.Errorf("record quote response: %w", err)
	}
	quote.State = QuoteResponseReceived
	quote.Response = &response
	if err := tx.Commit(ctx); err != nil {
		return OutboundQuote{}, false, fmt.Errorf("commit quote callback: %w", err)
	}
	return quote, false, nil
}

// CreateTransfer records a PREPARED outbound transfer built from a received
// quote. The ILP packet and condition come verbatim from the validated quote
// response — the condition is never derived locally in a way that could
// diverge from the payee's. Re-sends with the same body replay; a different
// body under the same transfer ID fails closed.
func (store *OutboundStore) CreateTransfer(ctx context.Context, transfer OutboundTransfer, requestBody []byte) (duplicate bool, err error) {
	if transfer.TransferID == "" || transfer.QuoteID == "" || transfer.PayerFSP == "" || transfer.PayeeFSP == "" || transfer.Amount == "" || transfer.Currency == "" || transfer.ILPPacket == "" || transfer.Condition == "" {
		return false, errors.New("outbound transfer identity, quote link, ILP packet and condition are required")
	}
	if err := ValidateILPCondition(transfer.Condition); err != nil {
		return false, err
	}
	hash := bodyDigest(requestBody)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin create transfer: %w", err)
	}
	defer tx.Rollback(ctx)
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT request_body_sha256 FROM mojaloop_outbound_transfers WHERE transfer_id = $1 FOR UPDATE`, transfer.TransferID).Scan(&existingHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO mojaloop_outbound_transfers (transfer_id, quote_id, payer_fsp, payee_fsp, amount, currency, ilp_packet, ilp_condition, request_body_sha256, transfer_state, created_at, updated_at, version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11,1)`, transfer.TransferID, transfer.QuoteID, transfer.PayerFSP, transfer.PayeeFSP, transfer.Amount, transfer.Currency, transfer.ILPPacket, transfer.Condition, hash, TransferPrepared, now); err != nil {
			return false, fmt.Errorf("insert outbound transfer: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit outbound transfer: %w", err)
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lookup outbound transfer: %w", err)
	}
	if existingHash != hash {
		return false, ErrTransferIdentityChange
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit transfer replay: %w", err)
	}
	return true, nil
}

// LookupOutboundTransfer reports whether the transfer ID belongs to the
// outbound leg and returns the row for callback validation.
func (store *OutboundStore) LookupOutboundTransfer(ctx context.Context, transferID string) (OutboundTransfer, error) {
	var transfer OutboundTransfer
	var fulfilment *string
	err := store.pool.QueryRow(ctx, `SELECT transfer_id, quote_id, payer_fsp, payee_fsp, amount, currency, ilp_packet, ilp_condition, transfer_state, fulfilment FROM mojaloop_outbound_transfers WHERE transfer_id = $1`, transferID).Scan(&transfer.TransferID, &transfer.QuoteID, &transfer.PayerFSP, &transfer.PayeeFSP, &transfer.Amount, &transfer.Currency, &transfer.ILPPacket, &transfer.Condition, &transfer.State, &fulfilment)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboundTransfer{}, ErrUnknownTransfer
	}
	if err != nil {
		return OutboundTransfer{}, fmt.Errorf("load outbound transfer: %w", err)
	}
	transfer.Fulfilment = deref(fulfilment)
	return transfer, nil
}

// ApplyOutboundTransferCallback advances a PREPARED outbound transfer to its
// Hub-signed terminal state. COMMITTED requires a fulfilment that satisfies
// the ILP condition (verified before any state change); ABORTED carries no
// fulfilment. Exact replays are idempotent; regression or conflicting
// terminal bodies fail closed.
func (store *OutboundStore) ApplyOutboundTransferCallback(ctx context.Context, callback TransferCallback, body []byte) (OutboundTransfer, bool, error) {
	if err := validateCallbackIdentity(callback); err != nil {
		return OutboundTransfer{}, false, err
	}
	hash := bodyDigest(body)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return OutboundTransfer{}, false, fmt.Errorf("begin outbound transfer callback: %w", err)
	}
	defer tx.Rollback(ctx)
	var transfer OutboundTransfer
	var fulfilment, callbackHash *string
	err = tx.QueryRow(ctx, `SELECT transfer_id, quote_id, payer_fsp, payee_fsp, amount, currency, ilp_packet, ilp_condition, transfer_state, fulfilment, callback_body_sha256 FROM mojaloop_outbound_transfers WHERE transfer_id = $1 FOR UPDATE`, callback.TransferID).Scan(&transfer.TransferID, &transfer.QuoteID, &transfer.PayerFSP, &transfer.PayeeFSP, &transfer.Amount, &transfer.Currency, &transfer.ILPPacket, &transfer.Condition, &transfer.State, &fulfilment, &callbackHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboundTransfer{}, false, ErrUnknownTransfer
	}
	if err != nil {
		return OutboundTransfer{}, false, fmt.Errorf("lookup outbound transfer: %w", err)
	}
	if transfer.State == TransferCommitted || transfer.State == TransferAborted {
		if callbackHash != nil && *callbackHash == hash {
			transfer.Fulfilment = deref(fulfilment)
			if err := tx.Commit(ctx); err != nil {
				return OutboundTransfer{}, false, fmt.Errorf("commit terminal replay: %w", err)
			}
			return transfer, true, nil
		}
		return OutboundTransfer{}, false, errors.New("terminal outbound transfer callback has conflicting body")
	}
	if err := ValidateOutboundTransferCallback(transfer, callback); err != nil {
		return OutboundTransfer{}, false, err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE mojaloop_outbound_transfers SET transfer_state = $1, fulfilment = $2, callback_body_sha256 = $3, updated_at = $4, version = version + 1 WHERE transfer_id = $5 AND transfer_state = $6`, callback.TransferState, callback.Fulfilment, hash, now, callback.TransferID, TransferPrepared); err != nil {
		return OutboundTransfer{}, false, fmt.Errorf("finalize outbound transfer: %w", err)
	}
	transfer.State = callback.TransferState
	transfer.Fulfilment = callback.Fulfilment
	if err := tx.Commit(ctx); err != nil {
		return OutboundTransfer{}, false, fmt.Errorf("commit outbound transfer callback: %w", err)
	}
	return transfer, false, nil
}
