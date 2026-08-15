CREATE TABLE IF NOT EXISTS mojaloop_transfer_callbacks (
    transfer_id text PRIMARY KEY,
    payer_fsp text NOT NULL,
    payee_fsp text NOT NULL,
    amount text NOT NULL,
    currency text NOT NULL,
    transfer_state text NOT NULL CHECK (transfer_state IN ('RESERVED', 'COMMITTED', 'ABORTED')),
    fulfilment text NOT NULL DEFAULT '',
    body_sha256 text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    version bigint NOT NULL CHECK (version > 0)
);

CREATE INDEX IF NOT EXISTS idx_mojaloop_transfer_callbacks_state
    ON mojaloop_transfer_callbacks (transfer_state);
