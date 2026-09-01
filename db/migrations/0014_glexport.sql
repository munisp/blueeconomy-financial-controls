-- Phase 12 GL export: feed-based ERP integration layer.
--
-- The GL-export module turns the platform's authoritative postings
-- (settlement_records collections + cvff disbursement legs, mirrored from the
-- TigerBeetle ledger) into ISO 20022 feeds for external ERPs (SAP, Dynamics,
-- etc.). Integration is strictly feed-based: this schema records what was
-- exported, who exported it, and closes accounting periods — no external ERP
-- artifact is ever embedded.
--
-- All statements are idempotent (IF NOT EXISTS).

-- ---------------------------------------------------------------------------
-- Export audit trail. Every ISO 20022 feed emitted by the platform is
-- recorded exactly once with the sha256 of the payload, the signed envelope
-- v1.0 wrapping it (JWS-EdDSA over the JCS payload, like every other
-- artifact that crosses a trust boundary) and the exporting subject.
-- payload_sha256 is UNIQUE: re-export of byte-identical content is a replay.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS export_batches (
    batch_id            TEXT PRIMARY KEY,
    export_type         TEXT NOT NULL CHECK (export_type IN (
        'CAMT053_STATEMENT',
        'PAIN001_CREDIT_TRANSFER'
    )),
    account_ref         TEXT NOT NULL,          -- GL account or instruction scope
    period_start        DATE NOT NULL,
    period_end          DATE NOT NULL,
    currency            TEXT NOT NULL CHECK (currency IN ('USD', 'NGN')),
    entry_count         INTEGER NOT NULL CHECK (entry_count >= 0),
    total_debit_minor   BIGINT NOT NULL CHECK (total_debit_minor >= 0),
    total_credit_minor  BIGINT NOT NULL CHECK (total_credit_minor >= 0),
    payload_sha256      TEXT NOT NULL UNIQUE,   -- sha256 of the XML payload
    payload             TEXT NOT NULL,          -- the exported ISO 20022 XML
    envelope            JSONB NOT NULL,         -- envelope v1.0 FHIR Bundle
    envelope_jws        TEXT NOT NULL,
    signer_kid          TEXT NOT NULL,
    exported_by         TEXT NOT NULL,          -- verified bearer subject
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (period_end >= period_start)
);
CREATE INDEX IF NOT EXISTS export_batches_account_period_idx
    ON export_batches (account_ref, period_start, period_end);
CREATE INDEX IF NOT EXISTS export_batches_type_idx
    ON export_batches (export_type, created_at);

-- ---------------------------------------------------------------------------
-- Period close with maker-checker dual control. A close is requested by one
-- officer (maker) and approved by a different officer (checker); the trial
-- balance snapshot at approval is frozen for downstream audit. Exports are
-- not blocked after close — reconciliation reports flag any drift instead
-- (fail-closed on identity, fail-visible on drift).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS period_close (
    period_close_id     TEXT PRIMARY KEY,
    period_start        DATE NOT NULL,
    period_end          DATE NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('PENDING_APPROVAL', 'CLOSED')),
    requested_by        TEXT NOT NULL,
    approved_by         TEXT,
    trial_balance       JSONB,                  -- frozen trial balance at close
    requested_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    approved_at         TIMESTAMPTZ,
    CHECK (period_end >= period_start),
    -- One close per accounting period, regardless of state.
    UNIQUE (period_start, period_end)
);
CREATE INDEX IF NOT EXISTS period_close_status_idx ON period_close (status);

-- ---------------------------------------------------------------------------
-- Reconciliation runs: journals vs exported statements per period. The run
-- stores both sides' totals plus the per-account difference detail so the
-- report is replayable evidence rather than a recomputed view.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS reconciliation_runs (
    run_id                  TEXT PRIMARY KEY,
    period_start            DATE NOT NULL,
    period_end              DATE NOT NULL,
    journal_entry_count     INTEGER NOT NULL,
    journal_total_minor     BIGINT NOT NULL,
    exported_entry_count    INTEGER NOT NULL,
    exported_total_minor    BIGINT NOT NULL,
    balanced                BOOLEAN NOT NULL,
    differences             JSONB NOT NULL,     -- per-account drift detail
    run_by                  TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (period_end >= period_start)
);
CREATE INDEX IF NOT EXISTS reconciliation_runs_period_idx
    ON reconciliation_runs (period_start, period_end);
