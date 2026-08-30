-- W-FEAT-7: revenue-assurance chain — debit notes, TSA remittance split,
-- three-way reconciliation.
--
-- Doctrine (mirrors 0008_tariff.sql): split rules are DATA — versioned,
-- effective-dated, maker/checker-approved rows, never code constants. All
-- money is integer minor units. Dual control is enforced in SQL
-- (checker <> maker on rule rows; issue/cancel require an actor distinct
-- from the note's maker via the store's guarded UPDATEs). Every artifact
-- that leaves the platform (debit note, remittance advice, bank statement
-- ingest) is carried as envelope v1.0: a FHIR R4 Bundle wrapping a
-- JWS-EdDSA signature over the RFC 8785 JCS-canonical payload.
--
-- Split-rule shares seeded below encode the engagement's documented
-- structure (CVFF fiduciary-segregated treatment as referenced in gitops);
-- percentage splits whose statutory basis is not gazetted in the research
-- are seeded provisional = TRUE — documented, never guessed silently.

-- ---------------------------------------------------------------------------
-- Debit notes: assessment -> payable instrument. DRAFT is the pre-issuance
-- state (created by the maker); issuance (DRAFT -> ISSUED) is the dual-
-- controlled act that assigns the per-agency document number and seals the
-- signed envelope. Lifecycle thereafter:
--   ISSUED -> ACKED -> SETTLED
--   ISSUED -> DISPUTED -> ACKED | CANCELLED(reason)
--   ISSUED -> CANCELLED(reason)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS revenue_doc_series (
    agency          TEXT NOT NULL CHECK (agency IN ('NPA', 'NIMASA', 'NIWA', 'FMMBE')),
    series_year     INTEGER NOT NULL CHECK (series_year >= 2000),
    next_seq        BIGINT NOT NULL DEFAULT 1 CHECK (next_seq > 0),
    PRIMARY KEY (agency, series_year)
);

CREATE TABLE IF NOT EXISTS revenue_debit_notes (
    debit_note_id   TEXT PRIMARY KEY,
    assessment_id   TEXT NOT NULL REFERENCES tariff_assessments (assessment_id),
    idempotency_key TEXT NOT NULL UNIQUE,
    request_hash    TEXT NOT NULL,          -- sha256 of the canonical issuance request
    agency          TEXT NOT NULL CHECK (agency IN ('NPA', 'NIMASA', 'NIWA', 'FMMBE')),
    entity_ref      TEXT NOT NULL,          -- debtor (vessel owner/charterer ref)
    document_number TEXT UNIQUE,            -- assigned at issuance, e.g. NPA-2026-000042
    amount_usd_minor BIGINT NOT NULL DEFAULT 0 CHECK (amount_usd_minor >= 0),
    amount_ngn_minor BIGINT NOT NULL DEFAULT 0 CHECK (amount_ngn_minor >= 0),
    effective_date  DATE NOT NULL,
    due_date        DATE NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('DRAFT', 'ISSUED', 'ACKED', 'DISPUTED', 'SETTLED', 'CANCELLED')),
    cancel_reason   TEXT,
    envelope        JSONB,                  -- envelope v1.0 FHIR Bundle (sealed at issuance)
    envelope_jws    TEXT,                   -- JWS-EdDSA compact serialization over JCS payload
    maker           TEXT NOT NULL,          -- verified token subject of the creator
    checker         TEXT,                   -- verified token subject of the issuer
    correlation_id  TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (checker IS NULL OR checker <> maker),
    CHECK (state <> 'CANCELLED' OR cancel_reason IS NOT NULL),
    CHECK (state NOT IN ('ISSUED', 'ACKED', 'DISPUTED', 'SETTLED') OR (document_number IS NOT NULL AND envelope IS NOT NULL AND envelope_jws IS NOT NULL))
);

-- Debit-note lines: the CHARGED assessment lines carried onto the note at
-- creation (immutable copy — assessments are immutable, notes survive
-- later tariff window changes).
CREATE TABLE IF NOT EXISTS revenue_debit_note_lines (
    debit_note_id   TEXT NOT NULL REFERENCES revenue_debit_notes (debit_note_id),
    line_no         INTEGER NOT NULL,
    instrument      TEXT NOT NULL,
    agency          TEXT NOT NULL,
    amount_minor    BIGINT NOT NULL DEFAULT 0 CHECK (amount_minor >= 0),
    currency        TEXT NOT NULL CHECK (currency IN ('USD', 'NGN')),
    statutory_reference TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (debit_note_id, line_no)
);

-- Transition audit: every state move, forever. Dual-controlled transitions
-- carry approver; SQL guarantees approver <> actor.
CREATE TABLE IF NOT EXISTS revenue_debit_note_transitions (
    transition_id   TEXT PRIMARY KEY,
    debit_note_id   TEXT NOT NULL REFERENCES revenue_debit_notes (debit_note_id),
    from_state      TEXT NOT NULL,
    to_state        TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    actor           TEXT NOT NULL,
    approver        TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (approver IS NULL OR approver <> actor)
);

-- ---------------------------------------------------------------------------
-- TSA split rules (Treasury Single Account): how one settled collection on
-- a revenue line divides between the agency's retained share, the FGN
-- consolidated revenue share (TSA at CBN) and fiduciary-segregated funds
-- (CVFF). Rules are VERSIONED DATA; the ACTIVE rows for a revenue line and
-- window must sum to exactly 10000 bps or the computation fails closed.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tsa_split_rules (
    rule_id         TEXT PRIMARY KEY,
    revenue_line    TEXT NOT NULL,          -- tariff instrument code; '*' is the catch-all
    agency          TEXT NOT NULL CHECK (agency IN ('NPA', 'NIMASA', 'NIWA', 'FMMBE')),
    beneficiary     TEXT NOT NULL CHECK (beneficiary IN ('AGENCY_RETAINED', 'FGN_CONSOLIDATED', 'CVFF_FIDUCIARY')),
    share_bps       BIGINT NOT NULL CHECK (share_bps > 0 AND share_bps <= 10000),
    statutory_reference TEXT NOT NULL,
    provisional     BOOLEAN NOT NULL DEFAULT FALSE,
    effective_from  DATE NOT NULL,
    effective_to    DATE,
    state           TEXT NOT NULL CHECK (state IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    maker           TEXT NOT NULL,
    checker         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (checker IS NULL OR checker <> maker)
);

-- Remittance advices: the signed artifact recording one deterministic split
-- of one settlement. Idempotent by key; replay returns the stored advice.
CREATE TABLE IF NOT EXISTS tsa_remittance_advices (
    advice_id       TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_hash    TEXT NOT NULL,
    settlement_id   TEXT NOT NULL,
    revenue_line    TEXT NOT NULL,
    agency          TEXT NOT NULL,
    as_of           DATE NOT NULL,
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    currency        TEXT NOT NULL CHECK (currency IN ('USD', 'NGN')),
    allocations     JSONB NOT NULL,         -- [{beneficiary, shareBps, amountMinor}], sums to amount
    envelope        JSONB NOT NULL,         -- envelope v1.0 FHIR Bundle
    envelope_jws    TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    correlation_id  TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Settlement/collection records: the materialized, queryable mirror of the
-- TigerBeetle ledger entries for collections (the TB ledger stays the
-- system of record upstream; tb_transfer_id links back). Reconciliation
-- reads this table so recon is deterministic and replayable.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS settlement_records (
    settlement_id   TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    request_hash    TEXT NOT NULL,
    debit_note_id   TEXT REFERENCES revenue_debit_notes (debit_note_id),
    bank_reference  TEXT NOT NULL,
    tb_transfer_id  TEXT,                   -- TigerBeetle transfer id (hex), when posted
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    currency        TEXT NOT NULL CHECK (currency IN ('USD', 'NGN')),
    payer_ref       TEXT NOT NULL,
    value_date      DATE NOT NULL,
    recorded_by     TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS settlement_records_bank_ref_idx ON settlement_records (bank_reference);
CREATE INDEX IF NOT EXISTS settlement_records_note_idx ON settlement_records (debit_note_id);

-- ---------------------------------------------------------------------------
-- TSA/bank statement ingest: structured statements carried in a signed
-- envelope v1.0 (verified at ingest — fail closed). statement_hash makes
-- re-ingest of the same statement a replay; (bank, bank_reference) on the
-- lines dedupes across overlapping statements, and a conflicting duplicate
-- reference raises a DUPLICATE_BANK_REF exception instead of inserting.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bank_statements (
    statement_id    TEXT PRIMARY KEY,
    statement_hash  TEXT NOT NULL UNIQUE,   -- sha256 of the JCS statement payload
    bank            TEXT NOT NULL,
    account_ref     TEXT NOT NULL,
    statement_ref   TEXT NOT NULL,
    period_start    DATE NOT NULL,
    period_end      DATE NOT NULL,
    signer_kid      TEXT NOT NULL,
    envelope        JSONB NOT NULL,         -- envelope v1.0 FHIR Bundle as received
    envelope_jws    TEXT NOT NULL,
    ingested_by     TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bank_statement_lines (
    statement_id    TEXT NOT NULL REFERENCES bank_statements (statement_id),
    line_no         INTEGER NOT NULL,
    bank            TEXT NOT NULL,          -- denormalized for cross-statement dedupe
    bank_reference  TEXT NOT NULL,
    value_date      DATE NOT NULL,
    direction       TEXT NOT NULL CHECK (direction IN ('CREDIT', 'DEBIT')),
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    currency        TEXT NOT NULL CHECK (currency IN ('USD', 'NGN')),
    narrative       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (statement_id, line_no),
    UNIQUE (bank, bank_reference)           -- dedupe by bank reference
);
CREATE INDEX IF NOT EXISTS bank_statement_lines_ref_idx ON bank_statement_lines (bank_reference);

-- ---------------------------------------------------------------------------
-- Reconciliation: runs, three-way matches, exception queue, resolution
-- audit. Exception classes (CHECK-enforced): UNMATCHED_ASSESSMENT,
-- UNMATCHED_SETTLEMENT, UNMATCHED_STATEMENT, AMOUNT_MISMATCH,
-- DUPLICATE_BANK_REF. dedupe_key keeps re-runs idempotent: one OPEN
-- exception per (class, entity).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS recon_runs (
    run_id          TEXT PRIMARY KEY,
    trigger_kind    TEXT NOT NULL CHECK (trigger_kind IN ('MANUAL', 'TEMPORAL_BATCH')),
    state           TEXT NOT NULL CHECK (state IN ('RUNNING', 'COMPLETED', 'FAILED')),
    started_by      TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    matched_count   INTEGER NOT NULL DEFAULT 0,
    exception_count INTEGER NOT NULL DEFAULT 0,
    failure_reason  TEXT
);

CREATE TABLE IF NOT EXISTS recon_matches (
    match_id        TEXT PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES recon_runs (run_id),
    debit_note_id   TEXT NOT NULL REFERENCES revenue_debit_notes (debit_note_id),
    settlement_id   TEXT NOT NULL REFERENCES settlement_records (settlement_id),
    statement_id    TEXT NOT NULL REFERENCES bank_statements (statement_id),
    statement_line_no INTEGER NOT NULL,
    amount_minor    BIGINT NOT NULL CHECK (amount_minor > 0),
    currency        TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (debit_note_id),
    UNIQUE (settlement_id),
    UNIQUE (statement_id, statement_line_no)
);

CREATE TABLE IF NOT EXISTS recon_exceptions (
    exception_id    TEXT PRIMARY KEY,
    run_id          TEXT NOT NULL REFERENCES recon_runs (run_id),
    class           TEXT NOT NULL CHECK (class IN (
                        'UNMATCHED_ASSESSMENT',
                        'UNMATCHED_SETTLEMENT',
                        'UNMATCHED_STATEMENT',
                        'AMOUNT_MISMATCH',
                        'DUPLICATE_BANK_REF')),
    state           TEXT NOT NULL CHECK (state IN ('OPEN', 'RESOLVED')),
    dedupe_key      TEXT NOT NULL,          -- class|entity — one OPEN exception per key
    debit_note_id   TEXT,
    settlement_id   TEXT,
    statement_id    TEXT,
    statement_line_no INTEGER,
    expected_amount_minor BIGINT,
    actual_amount_minor   BIGINT,
    currency        TEXT,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    raised_by       TEXT NOT NULL DEFAULT 'recon-engine',
    resolver        TEXT,
    resolution_note TEXT,
    resolved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (state <> 'RESOLVED' OR (resolver IS NOT NULL AND resolution_note IS NOT NULL AND resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS recon_exceptions_open_dedupe_idx
    ON recon_exceptions (dedupe_key) WHERE state = 'OPEN';
CREATE INDEX IF NOT EXISTS recon_exceptions_class_idx ON recon_exceptions (class, state);

CREATE TABLE IF NOT EXISTS recon_exception_events (
    event_id        TEXT PRIMARY KEY,
    exception_id    TEXT NOT NULL REFERENCES recon_exceptions (exception_id),
    event_kind      TEXT NOT NULL CHECK (event_kind IN ('RAISED', 'RESOLVED')),
    actor           TEXT NOT NULL,
    note            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Revenue outbox (same shape as tariff_outbox; drained by the publisher).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS revenue_outbox (
    event_id      UUID PRIMARY KEY,
    subject_id    TEXT NOT NULL,
    event_type    TEXT NOT NULL CHECK (event_type IN (
                      'revenue.debit_note.created',
                      'revenue.debit_note.transitioned',
                      'revenue.split_rule.created',
                      'revenue.split_rule.activated',
                      'revenue.remittance_advice.issued',
                      'revenue.settlement.recorded',
                      'revenue.statement.ingested',
                      'revenue.recon.run_completed',
                      'revenue.recon.exception_raised',
                      'revenue.recon.exception_resolved')),
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    published_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS revenue_outbox_unpublished_idx ON revenue_outbox (created_at) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- Seed split rules (maker/checker seed pair). Percentages whose statutory
-- basis is not gazetted in the verified research are PROVISIONAL with the
-- ambiguity documented; the CVFF fiduciary segregation is structural
-- (100% fiduciary — the fund never touches consolidated revenue).
-- ---------------------------------------------------------------------------

-- Cabotage Vessel Financing Fund: fiduciary-segregated treatment per the
-- CVFF four-party rail (gitops); collections on cabotage surcharge accrue
-- wholly to the segregated fund, not to FGN consolidated revenue.
INSERT INTO tsa_split_rules (rule_id, revenue_line, agency, beneficiary, share_bps,
    statutory_reference, provisional, effective_from, state, maker, checker)
VALUES ('split-cab-cvff-fiduciary', 'CABOTAGE_SURCHARGE', 'NIMASA', 'CVFF_FIDUCIARY', 10000,
    'CVFF fiduciary-segregated treatment (CIOTA/Coastal trade fund rail, gitops cvff-four-party-rail); NIMASA Act 2007 s.42-44', FALSE,
    DATE '2007-01-01', 'ACTIVE', 'seed-revenue', 'seed-revenue-review') ON CONFLICT DO NOTHING;

-- NPA ship dues: agency-retained vs FGN consolidated split is NOT gazetted
-- in the verified research — provisional 70/30 pending the FMMBE/CBN TSA
-- circular reference.
INSERT INTO tsa_split_rules (rule_id, revenue_line, agency, beneficiary, share_bps,
    statutory_reference, provisional, effective_from, state, maker, checker)
VALUES ('split-npa-dues-agency', 'NPA_SHIP_DUES', 'NPA', 'AGENCY_RETAINED', 7000,
    'PROVISIONAL: NPA retained share pending FMMBE/CBN TSA circular (not gazetted in verified research)', TRUE,
    DATE '2007-01-01', 'ACTIVE', 'seed-revenue', 'seed-revenue-review') ON CONFLICT DO NOTHING;
INSERT INTO tsa_split_rules (rule_id, revenue_line, agency, beneficiary, share_bps,
    statutory_reference, provisional, effective_from, state, maker, checker)
VALUES ('split-npa-dues-fgn', 'NPA_SHIP_DUES', 'NPA', 'FGN_CONSOLIDATED', 3000,
    'PROVISIONAL: FGN consolidated share pending FMMBE/CBN TSA circular (not gazetted in verified research)', TRUE,
    DATE '2007-01-01', 'ACTIVE', 'seed-revenue', 'seed-revenue-review') ON CONFLICT DO NOTHING;

-- Sea Protection Levy 2012 (NIMASA): same provisional treatment.
INSERT INTO tsa_split_rules (rule_id, revenue_line, agency, beneficiary, share_bps,
    statutory_reference, provisional, effective_from, state, maker, checker)
VALUES ('split-spl2012-agency', 'SEA_PROTECTION_LEVY_2012', 'NIMASA', 'AGENCY_RETAINED', 7000,
    'PROVISIONAL: NIMASA retained share pending FMMBE/CBN TSA circular (not gazetted in verified research)', TRUE,
    DATE '2012-01-01', 'ACTIVE', 'seed-revenue', 'seed-revenue-review') ON CONFLICT DO NOTHING;
INSERT INTO tsa_split_rules (rule_id, revenue_line, agency, beneficiary, share_bps,
    statutory_reference, provisional, effective_from, state, maker, checker)
VALUES ('split-spl2012-fgn', 'SEA_PROTECTION_LEVY_2012', 'NIMASA', 'FGN_CONSOLIDATED', 3000,
    'PROVISIONAL: FGN consolidated share pending FMMBE/CBN TSA circular (not gazetted in verified research)', TRUE,
    DATE '2012-01-01', 'ACTIVE', 'seed-revenue', 'seed-revenue-review') ON CONFLICT DO NOTHING;
