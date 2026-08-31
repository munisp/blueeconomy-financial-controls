-- FC-3: stranded minor states.
-- (a) FX rates pending dual-control confirmation gain an EXPIRED terminal
--     state; the TTL sweep (cvff-worker, FX_PENDING_CONFIRMATION_TTL) moves
--     unconfirmed rates there and an expired rate is never usable.
ALTER TABLE fx_rates
    DROP CONSTRAINT fx_rates_state_check;
ALTER TABLE fx_rates
    ADD CONSTRAINT fx_rates_state_check
    CHECK (state IN ('PENDING_CONFIRMATION', 'CONFIRMED', 'REJECTED', 'EXPIRED'));

-- (c) Mojaloop RESERVED callbacks gain a local timeout marker plus an audit
--     trail. The marker is local operational evidence only: the callback
--     state stays RESERVED (Hub truth is never fabricated) and a later
--     Hub-signed COMMITTED/ABORTED remains acceptable.
ALTER TABLE mojaloop_transfer_callbacks
    ADD COLUMN IF NOT EXISTS timed_out_at timestamptz;

CREATE TABLE IF NOT EXISTS mojaloop_callback_timeout_events (
    event_id uuid PRIMARY KEY,
    transfer_id text NOT NULL REFERENCES mojaloop_transfer_callbacks(transfer_id),
    reserved_at timestamptz NOT NULL,
    timed_out_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mojaloop_callback_timeout_events_transfer
    ON mojaloop_callback_timeout_events (transfer_id);
