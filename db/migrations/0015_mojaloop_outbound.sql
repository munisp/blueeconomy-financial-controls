-- Outbound Mojaloop leg: durable quote/transfer state machine.
-- The rail initiates POST /quotes and POST /transfers; payee-FSP PUT /quotes/{id}
-- callbacks and Hub PUT /transfers/{id} callbacks advance these rows. Rows are
-- immutable once terminal; every mutation bumps version and is idempotent on
-- the exact callback body hash (sha256), mirroring mojaloop_transfer_callbacks.

CREATE TABLE IF NOT EXISTS mojaloop_outbound_quotes (
    quote_id text PRIMARY KEY,
    payer_fsp text NOT NULL,
    payee_fsp text NOT NULL,
    amount text NOT NULL,
    currency text NOT NULL,
    request_body_sha256 text NOT NULL,
    quote_state text NOT NULL CHECK (quote_state IN ('REQUESTED', 'RESPONSE_RECEIVED')),
    transfer_amount text,
    transfer_currency text,
    ilp_packet text,
    ilp_condition text,
    callback_body_sha256 text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    version bigint NOT NULL CHECK (version > 0)
);

CREATE INDEX IF NOT EXISTS idx_mojaloop_outbound_quotes_state
    ON mojaloop_outbound_quotes (quote_state);

CREATE TABLE IF NOT EXISTS mojaloop_outbound_transfers (
    transfer_id text PRIMARY KEY,
    quote_id text NOT NULL REFERENCES mojaloop_outbound_quotes (quote_id),
    payer_fsp text NOT NULL,
    payee_fsp text NOT NULL,
    amount text NOT NULL,
    currency text NOT NULL,
    ilp_packet text NOT NULL,
    ilp_condition text NOT NULL,
    request_body_sha256 text NOT NULL,
    transfer_state text NOT NULL CHECK (transfer_state IN ('PREPARED', 'COMMITTED', 'ABORTED')),
    fulfilment text,
    callback_body_sha256 text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    version bigint NOT NULL CHECK (version > 0)
);

CREATE INDEX IF NOT EXISTS idx_mojaloop_outbound_transfers_state
    ON mojaloop_outbound_transfers (transfer_state);
