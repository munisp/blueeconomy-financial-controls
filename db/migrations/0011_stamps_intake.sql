-- WP-1: stamps intake (tax-stamps excise stamp lifecycle events).
--
-- Doctrine (mirrors 0010): money is integer minor units, replay is
-- idempotency-key safe, and authentic-but-unmappable intake is recorded
-- with mapping_error instead of being guessed into a money record.

-- ---------------------------------------------------------------------------
-- Stamps intake events: verified stamps.assessed.v1 / stamps.approved.v1 /
-- stamps.issued.v1 / stamps.activated.v1 envelopes consumed from the
-- stamps.* topics (published by blueeconomy-tax-stamps). The event id is
-- the idempotency key — a replay is a no-op. mapping_error records why an
-- authentic event could not be deterministically mapped; such rows surface
-- to operators instead of guessing amounts.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS stamps_intake_events (
    event_id        TEXT PRIMARY KEY,        -- platform envelope eventId (idempotency key)
    topic           TEXT NOT NULL
                  CHECK (topic IN ('stamps.assessed', 'stamps.approved', 'stamps.issued', 'stamps.activated')),
    event_type      TEXT NOT NULL
                  CHECK (event_type IN ('stamps.assessed.v1', 'stamps.approved.v1', 'stamps.issued.v1', 'stamps.activated.v1')),
    producer        TEXT NOT NULL,
    signer_kid      TEXT NOT NULL,           -- verified JWS kid
    occurred_at     TIMESTAMPTZ NOT NULL,
    correlation_id  TEXT NOT NULL,
    assessment_id   TEXT NOT NULL DEFAULT '',
    declaration_ref TEXT NOT NULL DEFAULT '',
    batch_id        TEXT NOT NULL DEFAULT '',
    total_duty_kobo BIGINT CHECK (total_duty_kobo IS NULL OR total_duty_kobo >= 0),
    quantity        BIGINT CHECK (quantity IS NULL OR quantity >= 0),
    mapping_error   TEXT NOT NULL DEFAULT '', -- '' = mapped record
    payload         JSONB NOT NULL,          -- full verified envelope as received
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS stamps_intake_assessment_idx ON stamps_intake_events (assessment_id);
CREATE INDEX IF NOT EXISTS stamps_intake_mapping_error_idx ON stamps_intake_events (mapping_error) WHERE mapping_error <> '';
