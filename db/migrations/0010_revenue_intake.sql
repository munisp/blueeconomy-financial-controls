-- W-CLOSE-FC: revenue intake (port-interoperability assessment events) and
-- the TigerBeetle -> settlement_records mirror sync.
--
-- Doctrine (mirrors 0009): money is integer minor units, replay is
-- idempotency-key safe, and authentic-but-unmappable intake is surfaced to
-- the reconciliation pipeline (never guessed into a money record).

-- ---------------------------------------------------------------------------
-- Revenue intake assessments: verified `revenue.assessment_issued` events
-- consumed from finance.revenue-assessments.v1 (published by the
-- port-interoperability tariff engine for offshore terminal fees and cruise
-- dues). The event id is the idempotency key — a replay is a no-op.
-- mapping_error records why an authentic event could not be mapped onto a
-- deterministic recon leg; the recon batch surfaces those rows as
-- UNMATCHED_STATEMENT-class exceptions instead of guessing amounts.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS revenue_intake_assessments (
    event_id        UUID PRIMARY KEY,        -- platform envelope eventId (idempotency key)
    topic           TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    producer        TEXT NOT NULL,
    signer_kid      TEXT NOT NULL,           -- verified JWS kid
    occurred_at     TIMESTAMPTZ NOT NULL,
    correlation_id  TEXT NOT NULL,
    domain          TEXT NOT NULL DEFAULT '',
    call_reference  TEXT NOT NULL DEFAULT '',
    schedule_id     TEXT NOT NULL DEFAULT '',
    assessment_id   TEXT NOT NULL DEFAULT '',
    total_minor     BIGINT CHECK (total_minor IS NULL OR total_minor >= 0),
    currency        TEXT NOT NULL DEFAULT '',
    mapping_error   TEXT NOT NULL DEFAULT '', -- '' = mapped assessment-side record
    payload         JSONB NOT NULL,          -- full verified envelope as received
    state           TEXT NOT NULL DEFAULT 'OPEN'
                    CHECK (state IN ('OPEN', 'SETTLEMENT_OBSERVED')),
    settlement_id   TEXT REFERENCES settlement_records (settlement_id),
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (state = 'OPEN' OR settlement_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS revenue_intake_call_ref_idx ON revenue_intake_assessments (call_reference);
CREATE INDEX IF NOT EXISTS revenue_intake_open_idx ON revenue_intake_assessments (state) WHERE state = 'OPEN';

-- ---------------------------------------------------------------------------
-- settlement_records is the materialized mirror of TigerBeetle collection
-- transfers; the link must be 1:1 so a mirror replay is a no-op.
-- ---------------------------------------------------------------------------
-- Manually recorded settlements carry '' (not NULL) when no TB link exists;
-- the mirror always writes a real hex id.
CREATE UNIQUE INDEX IF NOT EXISTS settlement_records_tb_transfer_idx
    ON settlement_records (tb_transfer_id)
    WHERE tb_transfer_id IS NOT NULL AND tb_transfer_id <> '';

-- ---------------------------------------------------------------------------
-- TB mirror sync cursor: one row per (sync key = currency ledger). The
-- cursor advances inside the same transaction as each mirrored settlement,
-- so a crash mid-batch replays idempotently (tb_transfer_id UNIQUE).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS settlement_sync_state (
    sync_key        TEXT PRIMARY KEY,        -- e.g. 'ledger:1'
    last_timestamp  BIGINT NOT NULL DEFAULT 0 CHECK (last_timestamp >= 0),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
